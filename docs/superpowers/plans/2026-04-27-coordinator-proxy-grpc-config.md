# Coordinator proxy mode: gRPC client + server TLS/auth/compression

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add YAML-native TLS, static auth headers, gzip compression, and keepalive config to coordinator proxy mode (client side); add TLS + optional client-cert auth to coordinator server side.

**Architecture:** Roll our own `proxy.ClientConfig` and `server.TLSConfig` (no `configgrpc`/OTel framework dep). `proxy.ClientConfig.dialOptions()` returns `[]grpc.DialOption`; `server.TLSConfig.Credentials()` returns `credentials.TransportCredentials` (or nil). `main.go` builds both, hands them to `proxy.New` and `srv.Start`.

**Tech Stack:** `google.golang.org/grpc`, `google.golang.org/grpc/credentials`, `google.golang.org/grpc/credentials/insecure`, `google.golang.org/grpc/keepalive`, `google.golang.org/grpc/encoding/gzip`, `google.golang.org/grpc/metadata`, stdlib `crypto/tls`, `crypto/x509`.

**Spec:** `docs/superpowers/specs/2026-04-27-coordinator-proxy-grpc-config-design.md`

---

## File map

**New:**
- `coordinator/proxy/config.go` — `ClientConfig`, `TLSConfig`, `KeepAliveConfig`, `Validate()`, `dialOptions()`.
- `coordinator/proxy/config_test.go` — table-driven validation tests.
- `coordinator/server/tls.go` — `TLSConfig`, `Credentials()`.
- `coordinator/server/tls_test.go` — `Credentials()` happy + error cases.
- `coordinator/internal/testtls/testtls.go` — shared test helper that generates a self-signed CA + leaf cert pair and writes PEM files into `t.TempDir()`.

**Modified:**
- `coordinator/proxy/pubsub.go` — `New(cfg ClientConfig, handler ...) (*PubSub, error)`; metadata header injection.
- `coordinator/proxy/pubsub_test.go` — update existing call sites; add mTLS round-trip test.
- `coordinator/proxy/integration_test.go` — update existing call site (insecure ClientConfig); add TLS variant.
- `coordinator/server/server.go` — `Start(..., tlsCreds credentials.TransportCredentials) error`.
- `coordinator/server/server_test.go` — TLS variant.
- `coordinator/config.go` — `Config.TLS *server.TLSConfig`; `ModeConfig.Proxy *proxy.ClientConfig` (drop the local `ProxyConfig` struct).
- `coordinator/main.go` — build `tlsCreds` once, pass through; pass `*m` (a `*proxy.ClientConfig`) to `proxy.New`.
- `coordinator/README.md` — document new fields.

---

## Task 1: Test helper — generate self-signed CA + leaf cert files

**Files:**
- Create: `coordinator/internal/testtls/testtls.go`

- [ ] **Step 1: Create the helper**

Write:

```go
// Package testtls generates an in-memory CA + leaf cert pair and writes them to
// a temp directory, returning the file paths. Used by gRPC TLS tests.
package testtls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Files holds PEM file paths for a CA cert and one leaf cert/key pair signed by it.
type Files struct {
	CACert string
	Cert   string
	Key    string
}

// Generate creates a CA and a single leaf certificate with the given DNS names
// and IPs in its SAN, writes all three PEM files into t.TempDir(), and returns
// their paths. Suitable for both server and client certificates.
func Generate(t *testing.T, dnsNames []string, ips []net.IP) Files {
	t.Helper()
	dir := t.TempDir()

	// CA
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	require.NoError(t, err)

	// Leaf
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "test-leaf"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     dnsNames,
		IPAddresses:  ips,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caTmpl, &leafKey.PublicKey, caKey)
	require.NoError(t, err)
	leafKeyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	require.NoError(t, err)

	files := Files{
		CACert: filepath.Join(dir, "ca.pem"),
		Cert:   filepath.Join(dir, "leaf.pem"),
		Key:    filepath.Join(dir, "leaf.key"),
	}
	writePEM(t, files.CACert, "CERTIFICATE", caDER)
	writePEM(t, files.Cert, "CERTIFICATE", leafDER)
	writePEM(t, files.Key, "PRIVATE KEY", leafKeyDER)
	return files
}

func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()
	require.NoError(t, pem.Encode(f, &pem.Block{Type: blockType, Bytes: der}))
}
```

- [ ] **Step 2: Verify it compiles**

Run: `go -C coordinator build ./internal/testtls/...`
Expected: no output, exit 0.

- [ ] **Step 3: Commit**

```bash
git add coordinator/internal/testtls/testtls.go
git commit -m "test: add testtls helper for generating CA+leaf cert pairs"
```

---

## Task 2: server.TLSConfig + Credentials() (red)

**Files:**
- Create: `coordinator/server/tls_test.go`
- Test: `coordinator/server/tls_test.go`

- [ ] **Step 1: Write the failing test**

Write:

```go
package server_test

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pitr.ca/retroactivesampling/coordinator/internal/testtls"
	"pitr.ca/retroactivesampling/coordinator/server"
)

func TestTLSConfig_Nil_ReturnsNilCreds(t *testing.T) {
	var cfg *server.TLSConfig
	creds, err := cfg.Credentials()
	require.NoError(t, err)
	assert.Nil(t, creds)
}

func TestTLSConfig_ServerOnly(t *testing.T) {
	files := testtls.Generate(t, []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
	cfg := &server.TLSConfig{CertFile: files.Cert, KeyFile: files.Key}
	creds, err := cfg.Credentials()
	require.NoError(t, err)
	assert.NotNil(t, creds)
	assert.Equal(t, "tls", creds.Info().SecurityProtocol)
}

func TestTLSConfig_MTLS(t *testing.T) {
	files := testtls.Generate(t, []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
	cfg := &server.TLSConfig{
		CertFile:     files.Cert,
		KeyFile:      files.Key,
		ClientCAFile: files.CACert,
	}
	creds, err := cfg.Credentials()
	require.NoError(t, err)
	require.NotNil(t, creds)
}

func TestTLSConfig_MissingKeyFile(t *testing.T) {
	files := testtls.Generate(t, []string{"localhost"}, nil)
	cfg := &server.TLSConfig{CertFile: files.Cert}
	_, err := cfg.Credentials()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "key_file")
}

func TestTLSConfig_MissingCertFile(t *testing.T) {
	files := testtls.Generate(t, []string{"localhost"}, nil)
	cfg := &server.TLSConfig{KeyFile: files.Key}
	_, err := cfg.Credentials()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cert_file")
}

func TestTLSConfig_BadCAFile(t *testing.T) {
	files := testtls.Generate(t, []string{"localhost"}, nil)
	cfg := &server.TLSConfig{
		CertFile:     files.Cert,
		KeyFile:      files.Key,
		ClientCAFile: "/nonexistent/ca.pem",
	}
	_, err := cfg.Credentials()
	require.Error(t, err)
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `go -C coordinator test ./server/ -run TestTLSConfig -v`
Expected: build fails — undefined: `server.TLSConfig`.

---

## Task 3: server.TLSConfig + Credentials() (green)

**Files:**
- Create: `coordinator/server/tls.go`

- [ ] **Step 1: Implement**

Write:

```go
package server

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"

	"google.golang.org/grpc/credentials"
)

type TLSConfig struct {
	CertFile     string `yaml:"cert_file"`
	KeyFile      string `yaml:"key_file"`
	ClientCAFile string `yaml:"client_ca_file"`
}

// Credentials returns gRPC server transport credentials, or (nil, nil) if c is
// nil (caller should use plain gRPC).
func (c *TLSConfig) Credentials() (credentials.TransportCredentials, error) {
	if c == nil {
		return nil, nil
	}
	if c.CertFile == "" {
		return nil, errors.New("server tls: cert_file is required")
	}
	if c.KeyFile == "" {
		return nil, errors.New("server tls: key_file is required")
	}
	cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("server tls: load keypair: %w", err)
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	if c.ClientCAFile != "" {
		pem, err := os.ReadFile(c.ClientCAFile)
		if err != nil {
			return nil, fmt.Errorf("server tls: read client_ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("server tls: client_ca_file %q contains no PEM certs", c.ClientCAFile)
		}
		tlsCfg.ClientCAs = pool
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return credentials.NewTLS(tlsCfg), nil
}
```

- [ ] **Step 2: Run, confirm pass**

Run: `go -C coordinator test ./server/ -run TestTLSConfig -v`
Expected: 6 tests pass.

- [ ] **Step 3: Commit**

```bash
git add coordinator/server/tls.go coordinator/server/tls_test.go
git commit -m "feat(coordinator): add server.TLSConfig with Credentials() builder"
```

---

## Task 4: Wire server TLS into Server.Start

**Files:**
- Modify: `coordinator/server/server.go` (function `Start`)
- Modify: `coordinator/server/server_test.go` (existing call sites + new TLS test)

- [ ] **Step 1: Update Start signature**

Edit `coordinator/server/server.go` — change the `grpc.NewServer()` call inside `Start` to accept TLS creds. Replace the existing `Start` method:

```go
func (s *Server) Start(ctx context.Context, addr string, shutdownTimeout time.Duration, tlsCreds credentials.TransportCredentials) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	var opts []grpc.ServerOption
	if tlsCreds != nil {
		opts = append(opts, grpc.Creds(tlsCreds))
	}
	gs := grpc.NewServer(opts...)
	gen.RegisterCoordinatorServer(gs, s)
	go func() {
		<-ctx.Done()
		slog.Info("shutting down")
		if shutdownTimeout == 0 {
			shutdownTimeout = 10 * time.Second
		}
		stopCtx, stopCancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer stopCancel()
		done := make(chan struct{})
		go func() {
			gs.GracefulStop()
			close(done)
		}()
		select {
		case <-done:
		case <-stopCtx.Done():
			slog.Warn("graceful stop timed out, forcing")
			gs.Stop()
		}
	}()
	slog.Info("coordinator listening", "addr", addr, "tls", tlsCreds != nil)
	return gs.Serve(lis)
}
```

Add the import: `"google.golang.org/grpc/credentials"`.

- [ ] **Step 2: Add a TLS round-trip test**

Append to `coordinator/server/server_test.go`:

```go
func TestServerStartWithTLS(t *testing.T) {
	files := testtls.Generate(t, []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})

	// Pick a port up front; Start blocks.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := lis.Addr().String()
	require.NoError(t, lis.Close())

	tlsCfg := &server.TLSConfig{CertFile: files.Cert, KeyFile: files.Key}
	creds, err := tlsCfg.Credentials()
	require.NoError(t, err)

	notified := make(chan []byte, 1)
	srv := server.New(func(id []byte) { notified <- id }, nil, nil, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.Start(ctx, addr, time.Second, creds) }()

	// Plain client must fail
	t.Run("plain client rejected", func(t *testing.T) {
		conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		callCtx, callCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer callCancel()
		stream, err := gen.NewCoordinatorClient(conn).Connect(callCtx)
		if err == nil {
			_, err = stream.Recv()
		}
		require.Error(t, err)
	})

	// TLS client must succeed
	t.Run("tls client accepted", func(t *testing.T) {
		caPEM, err := os.ReadFile(files.CACert)
		require.NoError(t, err)
		pool := x509.NewCertPool()
		require.True(t, pool.AppendCertsFromPEM(caPEM))
		clientCreds := credentials.NewTLS(&tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS12})
		conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(clientCreds))
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		stream, err := gen.NewCoordinatorClient(conn).Connect(context.Background())
		require.NoError(t, err)
		require.NoError(t, stream.Send(&gen.ProcessorMessage{
			Payload: &gen.ProcessorMessage_Notify{
				Notify: &gen.NotifyInteresting{TraceId: make([]byte, 16)},
			},
		}))
		select {
		case <-notified:
		case <-time.After(2 * time.Second):
			t.Fatal("notify not received")
		}
	})

	cancel()
	select {
	case <-srvErr:
	case <-time.After(3 * time.Second):
		t.Fatal("server did not shut down")
	}
}
```

Add imports at top of file: `"crypto/tls"`, `"crypto/x509"`, `"os"`, `"google.golang.org/grpc/credentials"`, and `"pitr.ca/retroactivesampling/coordinator/internal/testtls"`.

- [ ] **Step 3: Update existing test call sites**

`coordinator/server/server_test.go` does NOT call `srv.Start` directly today (it uses local `startServer`), so existing tests are unaffected. Verify with grep:

Run: `grep -n "\.Start(" coordinator/server/server_test.go`
Expected: no match (only the new test calls `srv.Start`).

- [ ] **Step 4: Run all server tests**

Run: `go -C coordinator test ./server/ -v`
Expected: all pass including `TestServerStartWithTLS`.

- [ ] **Step 5: Commit**

```bash
git add coordinator/server/server.go coordinator/server/server_test.go
git commit -m "feat(coordinator): plumb TLS credentials through Server.Start"
```

---

## Task 5: proxy.ClientConfig + Validate (red)

**Files:**
- Create: `coordinator/proxy/config_test.go`

- [ ] **Step 1: Write the failing test**

Write:

```go
package proxy_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pitr.ca/retroactivesampling/coordinator/proxy"
)

func TestClientConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     proxy.ClientConfig
		wantErr string
	}{
		{
			name: "minimal",
			cfg:  proxy.ClientConfig{Endpoint: "host:9090"},
		},
		{
			name:    "missing endpoint",
			cfg:     proxy.ClientConfig{},
			wantErr: "endpoint",
		},
		{
			name: "tls server-only",
			cfg: proxy.ClientConfig{
				Endpoint: "host:9090",
				TLS:      &proxy.TLSConfig{CAFile: "/etc/ssl/ca.pem"},
			},
		},
		{
			name: "tls cert without key",
			cfg: proxy.ClientConfig{
				Endpoint: "host:9090",
				TLS:      &proxy.TLSConfig{CertFile: "/etc/ssl/c.pem"},
			},
			wantErr: "key_file",
		},
		{
			name: "tls key without cert",
			cfg: proxy.ClientConfig{
				Endpoint: "host:9090",
				TLS:      &proxy.TLSConfig{KeyFile: "/etc/ssl/k.pem"},
			},
			wantErr: "cert_file",
		},
		{
			name: "compression unknown",
			cfg: proxy.ClientConfig{
				Endpoint:    "host:9090",
				Compression: "snappy",
			},
			wantErr: "compression",
		},
		{
			name: "compression gzip",
			cfg: proxy.ClientConfig{
				Endpoint:    "host:9090",
				Compression: "gzip",
			},
		},
		{
			name: "header invalid uppercase",
			cfg: proxy.ClientConfig{
				Endpoint: "host:9090",
				Headers:  map[string]string{"Authorization": "Bearer x"},
			},
			wantErr: "header",
		},
		{
			name: "header reserved grpc- prefix",
			cfg: proxy.ClientConfig{
				Endpoint: "host:9090",
				Headers:  map[string]string{"grpc-trace-bin": "x"},
			},
			wantErr: "grpc-",
		},
		{
			name: "header valid",
			cfg: proxy.ClientConfig{
				Endpoint: "host:9090",
				Headers:  map[string]string{"authorization": "Bearer x", "x-tenant-id": "t"},
			},
		},
		{
			name: "keepalive timeout >= time",
			cfg: proxy.ClientConfig{
				Endpoint:  "host:9090",
				KeepAlive: &proxy.KeepAliveConfig{Time: 10 * time.Second, Timeout: 10 * time.Second},
			},
			wantErr: "timeout",
		},
		{
			name: "keepalive valid",
			cfg: proxy.ClientConfig{
				Endpoint:  "host:9090",
				KeepAlive: &proxy.KeepAliveConfig{Time: 30 * time.Second, Timeout: 10 * time.Second},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `go -C coordinator test ./proxy/ -run TestClientConfigValidate -v`
Expected: build fails — undefined `proxy.ClientConfig`, `proxy.TLSConfig`, `proxy.KeepAliveConfig`.

---

## Task 6: proxy.ClientConfig + Validate + dialOptions (green)

**Files:**
- Create: `coordinator/proxy/config.go`

- [ ] **Step 1: Implement**

Write:

```go
package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	_ "google.golang.org/grpc/encoding/gzip" // register gzip codec
	"google.golang.org/grpc/keepalive"
)

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

func (c *ClientConfig) Validate() error {
	if c.Endpoint == "" {
		return errors.New("proxy: endpoint is required")
	}
	if c.TLS != nil {
		if (c.TLS.CertFile == "") != (c.TLS.KeyFile == "") {
			if c.TLS.CertFile == "" {
				return errors.New("proxy tls: cert_file is required when key_file is set")
			}
			return errors.New("proxy tls: key_file is required when cert_file is set")
		}
	}
	if c.Compression != "" && c.Compression != "gzip" {
		return fmt.Errorf("proxy: unsupported compression %q (only \"gzip\" or empty)", c.Compression)
	}
	for k := range c.Headers {
		if err := validateHeaderKey(k); err != nil {
			return err
		}
	}
	if c.KeepAlive != nil && c.KeepAlive.Time > 0 && c.KeepAlive.Timeout > 0 && c.KeepAlive.Timeout >= c.KeepAlive.Time {
		return errors.New("proxy keepalive: timeout must be less than time")
	}
	return nil
}

func validateHeaderKey(k string) error {
	if k == "" {
		return errors.New("proxy: header name must not be empty")
	}
	if strings.HasPrefix(k, "grpc-") {
		return fmt.Errorf("proxy: header %q uses reserved grpc- prefix", k)
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '.':
		default:
			return fmt.Errorf("proxy: header %q has invalid character; must be lowercase a-z, 0-9, '-', '_', '.'", k)
		}
	}
	return nil
}

func (c *ClientConfig) dialOptions() ([]grpc.DialOption, error) {
	var opts []grpc.DialOption
	creds, err := c.transportCredentials()
	if err != nil {
		return nil, err
	}
	opts = append(opts, grpc.WithTransportCredentials(creds))
	if c.Compression == "gzip" {
		opts = append(opts, grpc.WithDefaultCallOptions(grpc.UseCompressor("gzip")))
	}
	if kp := c.KeepAlive; kp != nil {
		opts = append(opts, grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                kp.Time,
			Timeout:             kp.Timeout,
			PermitWithoutStream: kp.PermitWithoutStream,
		}))
	}
	return opts, nil
}

func (c *ClientConfig) transportCredentials() (credentials.TransportCredentials, error) {
	if c.TLS == nil {
		return insecure.NewCredentials(), nil
	}
	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: c.TLS.InsecureSkipVerify, // #nosec G402
		ServerName:         c.TLS.ServerNameOverride,
	}
	if c.TLS.CAFile != "" {
		pem, err := os.ReadFile(c.TLS.CAFile)
		if err != nil {
			return nil, fmt.Errorf("proxy tls: read ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("proxy tls: ca_file %q contains no PEM certs", c.TLS.CAFile)
		}
		tlsCfg.RootCAs = pool
	}
	if c.TLS.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(c.TLS.CertFile, c.TLS.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("proxy tls: load keypair: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	return credentials.NewTLS(tlsCfg), nil
}
```

- [ ] **Step 2: Run, confirm validation tests pass**

Run: `go -C coordinator test ./proxy/ -run TestClientConfigValidate -v`
Expected: all 12 subtests pass.

- [ ] **Step 3: Commit**

```bash
git add coordinator/proxy/config.go coordinator/proxy/config_test.go
git commit -m "feat(coordinator): add proxy.ClientConfig with TLS/headers/compression/keepalive"
```

---

## Task 7: Wire ClientConfig into proxy.New + update existing tests

**Files:**
- Modify: `coordinator/proxy/pubsub.go`
- Modify: `coordinator/proxy/pubsub_test.go`
- Modify: `coordinator/proxy/integration_test.go`

- [ ] **Step 1: Update pubsub.go**

Replace the existing `New` function and the `run`/`recv` logic that needs the headers context. Edit `coordinator/proxy/pubsub.go`:

```go
package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	gen "pitr.ca/retroactivesampling/proto"
)

const (
	sendBufSize = 256
	maxBackoff  = 30 * time.Second
)

type PubSub struct {
	conn    *grpc.ClientConn
	headers metadata.MD
	sendCh  chan []byte
	handler func([]byte)
	ctx     context.Context
	cancel  context.CancelFunc
}

func New(cfg ClientConfig, handler func([]byte)) (*PubSub, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	opts, err := cfg.dialOptions()
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(cfg.Endpoint, opts...)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &PubSub{
		conn:    conn,
		headers: headersToMD(cfg.Headers),
		sendCh:  make(chan []byte, sendBufSize),
		handler: handler,
		ctx:     ctx,
		cancel:  cancel,
	}
	go p.run()
	return p, nil
}

func headersToMD(h map[string]string) metadata.MD {
	if len(h) == 0 {
		return nil
	}
	md := metadata.MD{}
	for k, v := range h {
		md.Set(k, v)
	}
	return md
}
```

Then in the existing `run` method, replace the `gen.NewCoordinatorClient(p.conn).Connect(connCtx)` call with one that injects headers:

```go
func (p *PubSub) run() {
	backoff := time.Second
	for {
		select {
		case <-p.ctx.Done():
			return
		default:
		}
		connCtx, connCancel := context.WithCancel(p.ctx)
		streamCtx := connCtx
		if p.headers != nil {
			streamCtx = metadata.NewOutgoingContext(connCtx, p.headers)
		}
		client, err := gen.NewCoordinatorClient(p.conn).Connect(streamCtx)
		if err != nil {
			connCancel()
			slog.Warn("proxy: connect failed, retrying", "backoff", backoff, "err", err)
			select {
			case <-p.ctx.Done():
				return
			case <-time.After(backoff):
				backoff = min(backoff*2, maxBackoff)
			}
			continue
		}
		go p.recv(client, connCancel)
		if err := p.send(connCtx, client); err != nil {
			connCancel()
			slog.Warn("proxy: connection lost, retrying", "backoff", backoff, "err", err)
			select {
			case <-p.ctx.Done():
				return
			case <-time.After(backoff):
				backoff = min(backoff*2, maxBackoff)
			}
		} else {
			connCancel()
			backoff = time.Second
		}
	}
}
```

The existing `Publish`, `Close`, `recv`, `send` methods remain unchanged. The `_ "fmt"` import stays because `Publish` uses it for the trace_id format.

- [ ] **Step 2: Update existing pubsub_test.go call sites**

In `coordinator/proxy/pubsub_test.go`, replace each `proxy.New(addr, handler)` call with `proxy.New(proxy.ClientConfig{Endpoint: addr}, handler)`. Use Edit with `replace_all`:

Find: `proxy.New(addr, func`
Replace: `proxy.New(proxy.ClientConfig{Endpoint: addr}, func`

Also replace in the `TestCloseStopsReconnectLoop` test where the variable is `addr`:

Find: `ps, err := proxy.New(addr, func([]byte) {})`
Replace: `ps, err := proxy.New(proxy.ClientConfig{Endpoint: addr}, func([]byte) {})`

(That occurs twice — `replace_all` on the first pattern handles both.)

- [ ] **Step 3: Update integration_test.go call site**

In `coordinator/proxy/integration_test.go`:

Find: `localPS, err := proxy.New(centralAddr, func(id []byte) { localSrv.Broadcast(id) })`
Replace: `localPS, err := proxy.New(proxy.ClientConfig{Endpoint: centralAddr}, func(id []byte) { localSrv.Broadcast(id) })`

- [ ] **Step 4: Run all proxy tests**

Run: `go -C coordinator test ./proxy/ -v`
Expected: all existing tests still pass (validation + non-TLS pubsub + integration).

- [ ] **Step 5: Commit**

```bash
git add coordinator/proxy/pubsub.go coordinator/proxy/pubsub_test.go coordinator/proxy/integration_test.go
git commit -m "refactor(coordinator): proxy.New takes ClientConfig instead of endpoint string"
```

---

## Task 8: mTLS round-trip test for proxy

**Files:**
- Modify: `coordinator/proxy/pubsub_test.go`

- [ ] **Step 1: Add a TLS server helper alongside startGRPCServer**

Append to `coordinator/proxy/pubsub_test.go`:

```go
func startTLSGRPCServer(t *testing.T, srv gen.CoordinatorServer, certFile, keyFile, clientCAFile string) string {
	t.Helper()
	tlsCfg := &server.TLSConfig{CertFile: certFile, KeyFile: keyFile, ClientCAFile: clientCAFile}
	creds, err := tlsCfg.Credentials()
	require.NoError(t, err)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	gs := grpc.NewServer(grpc.Creds(creds))
	gen.RegisterCoordinatorServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	return lis.Addr().String()
}

func TestProxyMTLSRoundTrip(t *testing.T) {
	files := testtls.Generate(t, []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
	mock := newMock()
	addr := startTLSGRPCServer(t, mock, files.Cert, files.Key, files.CACert)

	cfg := proxy.ClientConfig{
		Endpoint: addr,
		TLS: &proxy.TLSConfig{
			CAFile:             files.CACert,
			CertFile:           files.Cert,
			KeyFile:            files.Key,
			ServerNameOverride: "localhost",
		},
	}
	ps, err := proxy.New(cfg, func([]byte) {})
	require.NoError(t, err)
	t.Cleanup(func() { _ = ps.Close() })

	novel, err := ps.Publish(t.Context(), traceBytes)
	require.NoError(t, err)
	assert.False(t, novel)

	select {
	case id := <-mock.notified:
		assert.Equal(t, traceHex, id)
	case <-time.After(3 * time.Second):
		t.Fatal("timeout: NotifyInteresting not received over TLS")
	}
}
```

Add imports at top of file (alongside existing): `"pitr.ca/retroactivesampling/coordinator/internal/testtls"`, `"pitr.ca/retroactivesampling/coordinator/server"`.

- [ ] **Step 2: Run, confirm pass**

Run: `go -C coordinator test ./proxy/ -run TestProxyMTLSRoundTrip -v`
Expected: pass.

- [ ] **Step 3: Run full coordinator suite**

Run: `go -C coordinator test ./... -v`
Expected: all green.

- [ ] **Step 4: Commit**

```bash
git add coordinator/proxy/pubsub_test.go
git commit -m "test(coordinator): mTLS round-trip for proxy.New"
```

---

## Task 9: Header propagation test for proxy

**Files:**
- Modify: `coordinator/proxy/pubsub_test.go`

- [ ] **Step 1: Extend mockServer to capture incoming metadata**

In `coordinator/proxy/pubsub_test.go`, locate the `mockServer` struct and add a field + capture:

Find:

```go
type mockServer struct {
	gen.UnimplementedCoordinatorServer
	notified   chan string
	decisionCh chan []byte
	connected  chan struct{}
	once       sync.Once
}
```

Replace with:

```go
type mockServer struct {
	gen.UnimplementedCoordinatorServer
	notified   chan string
	decisionCh chan []byte
	connected  chan struct{}
	once       sync.Once
	gotMD      chan metadata.MD
}
```

In `Connect`, before the `m.once.Do` line, add:

```go
if m.gotMD != nil {
	if md, ok := metadata.FromIncomingContext(stream.Context()); ok {
		select {
		case m.gotMD <- md:
		default:
		}
	}
}
```

In `newMock`, add `gotMD: make(chan metadata.MD, 1),`. Add the import `"google.golang.org/grpc/metadata"` to the file.

- [ ] **Step 2: Add the test**

Append:

```go
func TestProxyHeadersAttachedToStream(t *testing.T) {
	mock := newMock()
	addr := startGRPCServer(t, mock)

	cfg := proxy.ClientConfig{
		Endpoint: addr,
		Headers: map[string]string{
			"authorization": "Bearer test-token",
			"x-tenant-id":   "tenant-42",
		},
	}
	ps, err := proxy.New(cfg, func([]byte) {})
	require.NoError(t, err)
	t.Cleanup(func() { _ = ps.Close() })

	select {
	case md := <-mock.gotMD:
		assert.Equal(t, []string{"Bearer test-token"}, md.Get("authorization"))
		assert.Equal(t, []string{"tenant-42"}, md.Get("x-tenant-id"))
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for stream metadata")
	}
}
```

- [ ] **Step 3: Run, confirm pass**

Run: `go -C coordinator test ./proxy/ -run TestProxyHeadersAttachedToStream -v`
Expected: pass.

- [ ] **Step 4: Commit**

```bash
git add coordinator/proxy/pubsub_test.go
git commit -m "test(coordinator): proxy attaches static headers to stream metadata"
```

---

## Task 10: Update coordinator/config.go and main.go

**Files:**
- Modify: `coordinator/config.go`
- Modify: `coordinator/main.go`

- [ ] **Step 1: Drop the local ProxyConfig; embed proxy.ClientConfig**

Edit `coordinator/config.go`:

```go
package main

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"pitr.ca/retroactivesampling/coordinator/proxy"
	"pitr.ca/retroactivesampling/coordinator/redis"
	"pitr.ca/retroactivesampling/coordinator/server"
)

type Config struct {
	GRPCListen      string             `yaml:"grpc_listen"`
	LogLevel        slog.Level         `yaml:"log_level"`
	MetricsListen   string             `yaml:"metrics_listen"`
	ShutdownTimeout time.Duration      `yaml:"shutdown_timeout"`
	TLS             *server.TLSConfig  `yaml:"tls"`
	Mode            ModeConfig         `yaml:"mode"`
}

type ModeConfig struct {
	Single      *SingleConfig        `yaml:"single"`
	Distributed *DistributedConfig   `yaml:"distributed"`
	Proxy       *proxy.ClientConfig  `yaml:"proxy"`
}

func (m ModeConfig) active() (any, error) {
	var n int
	var result any
	if m.Single != nil {
		n++
		result = m.Single
	}
	if m.Distributed != nil {
		n++
		result = m.Distributed
	}
	if m.Proxy != nil {
		n++
		result = m.Proxy
	}
	if n != 1 {
		return nil, fmt.Errorf("exactly one mode must be configured (got %d)", n)
	}
	return result, nil
}

type SingleConfig struct {
	DecidedKeyTTL time.Duration `yaml:"decided_key_ttl"`
}

type DistributedConfig struct {
	DecidedKeyTTL time.Duration  `yaml:"decided_key_ttl"`
	RedisPrimary  redis.Config   `yaml:"redis_primary"`
	RedisReplicas []redis.Config `yaml:"redis_replicas"`
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	return &cfg, yaml.Unmarshal(data, &cfg)
}
```

The local `ProxyConfig` type is gone. (Its sole field `Endpoint` is now in `proxy.ClientConfig`.)

- [ ] **Step 2: Update main.go switch case + Start call**

Edit `coordinator/main.go`. Two edits:

(a) The switch case for proxy mode — find:

```go
	case *ProxyConfig:
		if m.Endpoint == "" {
			fatal("proxy mode: endpoint is required")
		}
		slog.Info("running in proxy mode", "endpoint", m.Endpoint)
		ps, err = proxy.New(m.Endpoint, srv.Broadcast)
		if err != nil {
			fatal("proxy", "err", err)
		}
```

Replace with:

```go
	case *proxy.ClientConfig:
		slog.Info("running in proxy mode", "endpoint", m.Endpoint)
		ps, err = proxy.New(*m, srv.Broadcast)
		if err != nil {
			fatal("proxy", "err", err)
		}
```

(`proxy.New` already calls `cfg.Validate()` so the explicit endpoint check is redundant.)

(b) Build TLS creds and pass to `srv.Start`. Find:

```go
	if err := srv.Start(ctx, cfg.GRPCListen, cfg.ShutdownTimeout); err != nil {
		fatal("serve", "err", err)
	}
```

Replace with:

```go
	tlsCreds, err := cfg.TLS.Credentials()
	if err != nil {
		fatal("server tls", "err", err)
	}
	if err := srv.Start(ctx, cfg.GRPCListen, cfg.ShutdownTimeout, tlsCreds); err != nil {
		fatal("serve", "err", err)
	}
```

- [ ] **Step 3: Build the binary**

Run: `make build`
Expected: `bin/coordinator` builds cleanly.

- [ ] **Step 4: Run full coordinator suite**

Run: `go -C coordinator test ./... -v`
Expected: all green. (No test exercises `main.go` directly; build success is sufficient for the wiring change.)

- [ ] **Step 5: Commit**

```bash
git add coordinator/config.go coordinator/main.go
git commit -m "refactor(coordinator): wire proxy.ClientConfig and server.TLSConfig into main"
```

---

## Task 11: End-to-end TLS integration test (proxy → parent over TLS)

**Files:**
- Modify: `coordinator/proxy/integration_test.go`

- [ ] **Step 1: Add a TLS variant of the daisy-chain test**

Append to `coordinator/proxy/integration_test.go`:

```go
func TestIntegrationLocalToCentralTLS(t *testing.T) {
	files := testtls.Generate(t, []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})

	// Central coordinator: TLS gRPC server with mock notify-handler -> memory PubSub.
	var centralSrv *server.Server
	memPS := memory.New(time.Minute, func(id []byte) { centralSrv.Broadcast(id) })
	centralSrv = server.New(func(traceID []byte) {
		_, _ = memPS.Publish(t.Context(), traceID)
	}, nil, nil, nil, nil)

	tlsCfg := &server.TLSConfig{CertFile: files.Cert, KeyFile: files.Key, ClientCAFile: files.CACert}
	creds, err := tlsCfg.Credentials()
	require.NoError(t, err)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	gs := grpc.NewServer(grpc.Creds(creds))
	gen.RegisterCoordinatorServer(gs, centralSrv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	centralAddr := lis.Addr().String()

	// Local coordinator: proxy PubSub points at central via mTLS.
	var localSrv *server.Server
	localPS, err := proxy.New(proxy.ClientConfig{
		Endpoint: centralAddr,
		TLS: &proxy.TLSConfig{
			CAFile:             files.CACert,
			CertFile:           files.Cert,
			KeyFile:            files.Key,
			ServerNameOverride: "localhost",
		},
	}, func(id []byte) { localSrv.Broadcast(id) })
	require.NoError(t, err)
	t.Cleanup(func() { _ = localPS.Close() })
	localSrv = server.New(func(traceID []byte) {
		_, _ = localPS.Publish(t.Context(), traceID)
	}, nil, nil, nil, nil)
	localAddr := startGRPCServer(t, localSrv)

	// Plain client connects to the local coordinator (still insecure on that hop).
	conn, err := grpc.NewClient(localAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	stream, err := gen.NewCoordinatorClient(conn).Connect(t.Context())
	require.NoError(t, err)

	traceBytes, _ := hex.DecodeString("aabbccdd11223344aabbccdd11223344")
	require.NoError(t, stream.Send(&gen.ProcessorMessage{
		Payload: &gen.ProcessorMessage_Notify{
			Notify: &gen.NotifyInteresting{TraceId: traceBytes},
		},
	}))

	msg, err := stream.Recv()
	require.NoError(t, err)
	b := msg.GetBatch()
	require.NotNil(t, b)
	require.Len(t, b.TraceIds, 1)
	assert.Equal(t, traceBytes, b.TraceIds[0])
}
```

Add imports if not present: `"net"`, `"pitr.ca/retroactivesampling/coordinator/internal/testtls"`.

- [ ] **Step 2: Run all proxy tests**

Run: `go -C coordinator test ./proxy/ -v`
Expected: all green including the new TLS daisy-chain test.

- [ ] **Step 3: Commit**

```bash
git add coordinator/proxy/integration_test.go
git commit -m "test(coordinator): end-to-end TLS daisy-chain integration test"
```

---

## Task 12: Update coordinator/README.md

**Files:**
- Modify: `coordinator/README.md`

- [ ] **Step 1: Update the common-fields table to add TLS**

Find the line in the common fields table:

```
| `mode` | yes | Exactly one of `single`, `distributed`, or `proxy` must be set |
```

Insert immediately above it:

```
| `tls` | no | If set, serve gRPC over TLS. See [Server TLS fields](#server-tls-fields) |
```

- [ ] **Step 2: Add a "Server TLS fields" subsection**

Insert after the `### Common fields` block (before `### `mode.single` fields`):

```markdown
### Server TLS fields

| Key | Description |
|---|---|
| `cert_file` | Path to server certificate PEM (required) |
| `key_file` | Path to server private key PEM (required) |
| `client_ca_file` | Optional. If set, require client certificates signed by this CA (mTLS) |

Example:

```yaml
tls:
  cert_file: /etc/ssl/server.crt
  key_file: /etc/ssl/server.key
  client_ca_file: /etc/ssl/ca.crt   # optional; enables mTLS
```
```

- [ ] **Step 3: Replace the proxy mode example**

Find:

````
### Proxy

```yaml
grpc_listen: :9090
log_level: INFO           # optional, default INFO
metrics_listen: :9091     # optional
shutdown_timeout: 10s     # optional, default 10s

mode:
  proxy:
    endpoint: central-coordinator:9090
```
````

Replace with:

````
### Proxy

```yaml
grpc_listen: :9090
log_level: INFO           # optional, default INFO
metrics_listen: :9091     # optional
shutdown_timeout: 10s     # optional, default 10s

mode:
  proxy:
    endpoint: central-coordinator:9090
    tls:                                   # optional; insecure if omitted
      ca_file: /etc/ssl/ca.crt
      cert_file: /etc/ssl/client.crt       # optional; required for mTLS
      key_file: /etc/ssl/client.key        # required if cert_file set
    headers:                               # optional
      authorization: "Bearer xyz"
      x-tenant-id: "abc"
    compression: gzip                      # optional; "" or "gzip"
    keepalive:                             # optional
      time: 30s
      timeout: 10s
      permit_without_stream: true
```
````

- [ ] **Step 4: Replace the `mode.proxy` fields subsection**

Find:

```
### `mode.proxy` fields

| Key | Required | Description |
|---|---|---|
| `endpoint` | yes | `host:port` of the parent coordinator to connect to |
```

Replace with:

````
### `mode.proxy` fields

| Key | Required | Description |
|---|---|---|
| `endpoint` | yes | `host:port` of the parent coordinator to connect to |
| `tls` | no | TLS settings for the upstream connection. See [`mode.proxy.tls` fields](#modeproxytls-fields) |
| `headers` | no | Map of static metadata headers attached to the outbound stream (e.g. bearer tokens, tenant IDs). Keys must be lowercase ASCII; the `grpc-` prefix is reserved |
| `compression` | no | `gzip` or empty (default) |
| `keepalive` | no | Client keepalive params. See [`mode.proxy.keepalive` fields](#modeproxykeepalive-fields) |

### `mode.proxy.tls` fields

| Key | Description |
|---|---|
| `ca_file` | Path to CA certificate PEM. If unset, system roots are used |
| `cert_file` | Client certificate PEM. Required for mTLS; `key_file` must also be set |
| `key_file` | Client private key PEM. Required when `cert_file` is set |
| `server_name_override` | Override `tls.Config.ServerName` (useful for IP-addressed endpoints with cert SAN mismatch) |
| `insecure_skip_verify` | Skip server certificate verification |

### `mode.proxy.keepalive` fields

| Key | Description |
|---|---|
| `time` | Send a keepalive ping after this much idle time (default: gRPC default — no pings) |
| `timeout` | Wait this long for ping ack before closing the connection. Must be less than `time` |
| `permit_without_stream` | Send pings even when there are no active streams |
````

- [ ] **Step 5: Sanity-check rendered Markdown**

Run: `head -200 coordinator/README.md`
Expected: tables look intact (no broken pipes), proxy section now mentions tls/headers/compression/keepalive.

- [ ] **Step 6: Final full validation**

Run: `make test test-integration lint`
Expected: all green.

- [ ] **Step 7: Commit**

```bash
git add coordinator/README.md
git commit -m "docs(coordinator): document server tls + proxy tls/headers/compression/keepalive"
```

---

## Task 13: Remove TODO entry

**Files:**
- Modify: `TODO.md`

- [ ] **Step 1: Remove the line**

In `TODO.md`, delete:

```
- coordinator: in proxy mode should support configs for auth/tls/headers/compression/etc, consider using go.opentelemetry.io/collector/config/configgrpc
```

- [ ] **Step 2: Commit**

```bash
git add TODO.md
git commit -m "chore: drop completed coordinator proxy grpc-config TODO"
```

---

## Self-review notes

- **Spec coverage:** All five spec sections (config shape, code layout, behavior, tests, docs) map to tasks 1-12. Out-of-scope items in the spec are not implemented.
- **Type consistency:** `proxy.ClientConfig`, `proxy.TLSConfig`, `proxy.KeepAliveConfig`, `server.TLSConfig`, `proxy.New(cfg ClientConfig, ...)`, `Server.Start(ctx, addr, shutdownTimeout, tlsCreds)`, `TLSConfig.Credentials()` — used identically across all tasks.
- **Header validation rules:** lowercase + `[a-z0-9-_.]` + no `grpc-` prefix — used the same way in Task 5 (test cases) and Task 6 (impl).
- **Test helper reuse:** `testtls.Generate` is built once in Task 1 and reused in tasks 2, 4, 8, 11.
- **Backward compatibility:** existing single-mode and distributed-mode YAML configs continue to parse unchanged (only adds optional fields and an optional top-level `tls` block).
