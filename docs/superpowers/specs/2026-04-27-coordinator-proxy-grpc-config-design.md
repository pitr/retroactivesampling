# Coordinator proxy mode: gRPC client + server TLS/auth/compression config

## Problem

`coordinator/proxy/pubsub.go` hardcodes `grpc.WithTransportCredentials(insecure.NewCredentials())` and accepts only `endpoint string`. There is no way to:

- connect to a TLS-protected parent coordinator,
- present a client certificate (mTLS),
- attach static auth headers (bearer token, tenant ID),
- enable gRPC-level compression on cross-cluster links,
- tune client keep-alive for NAT'd / proxied connections.

Symmetrically, `coordinator/server/server.go` uses plain `grpc.NewServer()`. Without server-side TLS the proxy-side TLS knobs are unusable end-to-end without an external TLS terminator.

The processor side already supports all these knobs via `go.opentelemetry.io/collector/config/configgrpc.ClientConfig`, because the processor is hosted by the OTel collector framework. The standalone coordinator binary has no such framework.

## Decision

Roll our own YAML-native `proxy.ClientConfig` and `server.TLSConfig` rather than embed `configgrpc`. Reasons:

- `configgrpc.ClientConfig.ToClientConn` requires a `component.Host` (with extension map) and `component.TelemetrySettings`. Wiring stub versions into a standalone binary is awkward and pulls 5-10 OTel modules into the coordinator just to call one function.
- The set of features actually needed (TLS, headers, compression, keepalive) is small and stable. Hand-rolled config is ~150 lines and stays in the YAML idiom shared by the rest of `coordinator/config.go` and `redis.Config`.
- Auth extensions (OAuth, bearer-via-extension) only work inside the OTel framework anyway. Static `headers` covers the realistic bearer-token case.

## Out of scope

- Retry budget / circuit-breaker config (gRPC defaults + existing exponential backoff in `proxy.run()` are sufficient).
- gRPC load-balancer config (`round_robin` etc.) — single-endpoint daisy-chain has no use for it.
- Per-call deadlines on `Connect` — it's a long-lived stream.
- Auth extensions (OAuth, bearer-via-extension). Static headers cover the 90% case.
- Server-side header-based auth (would need a server interceptor). Server auth is mTLS-only.
- HTTP transport, separate processor metrics fix — covered by the other two TODO.md large items.

## YAML config shape

### Top-level (server-side TLS)

```yaml
grpc_listen: :9090
log_level: INFO
metrics_listen: :9091
shutdown_timeout: 10s

tls:                          # optional; plain gRPC if omitted
  cert_file: /etc/ssl/server.crt
  key_file: /etc/ssl/server.key
  client_ca_file: /etc/ssl/ca.crt   # optional; enables mTLS

mode:
  proxy:
    endpoint: central-coordinator:9090
    tls:                       # optional; insecure if omitted
      ca_file: /etc/ssl/ca.crt
      cert_file: /etc/ssl/client.crt    # optional; required for mTLS
      key_file: /etc/ssl/client.key     # required if cert_file set
      server_name_override: ""          # optional
      insecure_skip_verify: false       # optional
    headers:                   # optional; attached to outbound stream metadata
      authorization: "Bearer xyz"
      x-tenant-id: "abc"
    compression: gzip          # optional; "" or "gzip"
    keepalive:                 # optional
      time: 30s
      timeout: 10s
      permit_without_stream: true
```

### Naming choices

- `headers` (not `metadata`) — operators read it as HTTP-style headers.
- `keepalive.time` mirrors `google.golang.org/grpc/keepalive.ClientParameters.Time`.
- TLS shape mirrors `redis.Config.TLS` (`ca_file`, `cert_file`, `key_file`, `insecure_skip_verify`) plus `server_name_override` (gRPC-specific).
- Server TLS uses `client_ca_file` instead of `ca_file` to make the mTLS intent obvious.

## Code layout

### New files

**`coordinator/proxy/config.go`**

```go
type ClientConfig struct {
    Endpoint    string            `yaml:"endpoint"`
    TLS         *TLSConfig        `yaml:"tls"`
    Headers     map[string]string `yaml:"headers"`
    Compression string            `yaml:"compression"`
    KeepAlive   *KeepAliveConfig  `yaml:"keepalive"`
}

type TLSConfig struct {
    CAFile             string `yaml:"ca_file"`
    CertFile           string `yaml:"cert_file"`
    KeyFile            string `yaml:"key_file"`
    ServerNameOverride string `yaml:"server_name_override"`
    InsecureSkipVerify bool   `yaml:"insecure_skip_verify"`
}

type KeepAliveConfig struct {
    Time                time.Duration `yaml:"time"`
    Timeout             time.Duration `yaml:"timeout"`
    PermitWithoutStream bool          `yaml:"permit_without_stream"`
}

func (c *ClientConfig) Validate() error
func (c *ClientConfig) dialOptions() ([]grpc.DialOption, error)
```

**`coordinator/server/tls.go`**

```go
type TLSConfig struct {
    CertFile     string `yaml:"cert_file"`
    KeyFile      string `yaml:"key_file"`
    ClientCAFile string `yaml:"client_ca_file"`
}

func (c *TLSConfig) Credentials() (credentials.TransportCredentials, error)
```

### Modified files

**`coordinator/proxy/pubsub.go`**

- `func New(cfg ClientConfig, handler func([]byte)) (*PubSub, error)` — replaces `New(endpoint string, ...)`.
- Builds dial options via `cfg.dialOptions()`, calls `grpc.NewClient(cfg.Endpoint, opts...)`.
- Headers attached via `metadata.NewOutgoingContext(ctx, md)` once before each `Connect(ctx)` call.

**`coordinator/server/server.go`**

- Signature: `func (s *Server) Start(ctx context.Context, addr string, shutdownTimeout time.Duration, tlsCreds credentials.TransportCredentials) error`.
- `nil` creds → `grpc.NewServer()` (current path); non-nil → `grpc.NewServer(grpc.Creds(tlsCreds))`.

**`coordinator/config.go`**

- `Config.TLS *server.TLSConfig` (top-level, peer of `grpc_listen`).
- `ProxyConfig` type removed; `ModeConfig.Proxy` becomes `*proxy.ClientConfig`.

**`coordinator/main.go`**

- Build `tlsCreds` once from `cfg.TLS` before `srv.Start`; pass through.
- Pass `*m` (the `*proxy.ClientConfig`) directly to `proxy.New`.

## Behavior

### Client TLS

| Config state | Result |
|---|---|
| no `tls` block | `insecure.NewCredentials()` (current behavior) |
| `tls` set, no cert/key | server-auth TLS; CA pool from `ca_file` or system roots if absent |
| `tls` set, cert + key | mTLS via `tls.LoadX509KeyPair` |
| `insecure_skip_verify: true` | bypass cert validation; logs a WARN at startup |
| `server_name_override` set | sets `tls.Config.ServerName` (for IP endpoints with cert SAN mismatch) |

### Server TLS

| Config state | Result |
|---|---|
| no `tls` block | plain gRPC (current behavior) |
| `cert_file` + `key_file` | server-auth TLS; `tls.NoClientCert` |
| above + `client_ca_file` | mTLS, `tls.RequireAndVerifyClientCert` against that CA |
| `cert_file` xor `key_file` | startup error |

### Headers

- Validated against gRPC metadata key rules: lowercase ASCII, no leading `grpc-`, no whitespace.
- Attached once at stream creation via `metadata.NewOutgoingContext`. Stays for the stream lifetime; not modified per message.

### Compression

- `""` → no compression (current).
- `"gzip"` → `grpc.WithDefaultCallOptions(grpc.UseCompressor("gzip"))`. Codec registered via blank import `_ "google.golang.org/grpc/encoding/gzip"`.
- Anything else → startup validation error.

### Keep-alive

- No block → grpc defaults (no client keepalive pings).
- Block present, all fields zero → defaults: `time: 30s, timeout: 10s, permit_without_stream: false`.
- Validation: `timeout < time` (per gRPC requirements).

### Validation (called from `loadConfig`)

- TLS cert/key pair mutual requirement on both client and server.
- Compression value whitelist.
- Header key validity.
- Keep-alive `timeout < time`.

## Tests

**New:**

- `coordinator/proxy/config_test.go` — table-driven validation: TLS pair requirements, compression whitelist, header key rules, keepalive timeout/time relationship.
- `coordinator/server/tls_test.go` — `Credentials()` happy path + missing-file errors.

**Extended:**

- `coordinator/proxy/pubsub_test.go` — mTLS round-trip with self-signed CA generated in-test (no fixtures on disk).
- `coordinator/server/server_test.go` — TLS variant: server starts with TLS creds, plain client fails, TLS client connects.
- `coordinator/proxy/integration_test.go` — one end-to-end test wiring TLS server + TLS client.

## Documentation

`coordinator/README.md` updates:

- Add top-level `tls` field to common-fields table.
- New `tls` subtable under common config.
- Expand proxy mode example showing `tls` + `headers` + `compression` + `keepalive`.
- New subtables: `mode.proxy` fields, `mode.proxy.tls` fields, `mode.proxy.keepalive` fields.

## Risks

- **TLS handshake errors are opaque.** A typo in `ca_file` or wrong `server_name_override` shows up only at first stream attempt as a generic "transport: authentication handshake failed". Mitigation: log the configured CA file path + ServerName at startup.
- **gRPC keepalive misconfiguration disconnects healthy streams.** `time` too low or `timeout` too short causes the parent to return GOAWAY. Mitigation: validation enforces `timeout < time`; defaults are conservative.
- **Static bearer tokens in YAML config.** Not encrypted at rest. Acceptable: same risk as `redis_primary.password` already in this config. Documented as such.
