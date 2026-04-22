# configgrpc Coordinator Endpoint Migration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the flat `coordinator_endpoint string` config field in the processor with `configgrpc.ClientConfig`, enabling TLS, auth, compression, and keepalive.

**Architecture:** Config gains a `CoordinatorGRPC configgrpc.ClientConfig` field (key `coordinator_grpc`). The processor gains a `Start(ctx, host)` lifecycle method where the coordinator client is created, since `configgrpc.ClientConfig.ToClientConn` needs `component.Host` which is only available at start time. The coordinator client drops its `endpoint string` field and calls `ToClientConn` for each reconnect attempt.

**Tech Stack:** `go.opentelemetry.io/collector/config/configgrpc v0.150.0`, `go.opentelemetry.io/collector/component/componenttest v0.150.0` (already in go.mod)

---

## File Map

| File | Change |
|---|---|
| `processor/retroactivesampling/go.mod` | Add `configgrpc v0.150.0` |
| `processor/retroactivesampling/config.go` | Replace `CoordinatorEndpoint string` with `CoordinatorGRPC configgrpc.ClientConfig`; add `Validate()` |
| `processor/retroactivesampling/internal/coordinator/client.go` | New `New()` signature; use `ToClientConn`; drop `endpoint string` |
| `processor/retroactivesampling/processor.go` | Add `grpcCfg`/`telSettings` fields; add `Start()`; nil-guard `Shutdown`; remove coord creation from `newProcessor` |
| `processor/retroactivesampling/factory.go` | Add `processorhelper.WithStart(p.Start)` |
| `processor/retroactivesampling/internal/coordinator/client_test.go` | Update `coord.New(...)` calls |
| `processor/retroactivesampling/processor_test.go` | Update `Config` construction; update `Start(..., nil)` to use `componenttest.NewNopHost()` |
| `processor/retroactivesampling/integration_test.go` | Same updates as `processor_test.go` |
| `processor/retroactivesampling/metadata.yaml` | Update `coordinator_endpoint` key to `coordinator_grpc` |
| `example/otelcol1.yaml` | Update `coordinator_endpoint` to `coordinator_grpc:` block |
| `example/otelcol2.yaml` | Same |

---

### Task 1: Add configgrpc dependency

**Files:**
- Modify: `processor/retroactivesampling/go.mod`

- [ ] **Step 1: Add the dependency**

Run from the repo root:
```bash
cd processor/retroactivesampling && go get go.opentelemetry.io/collector/config/configgrpc@v0.150.0
```

Expected: line added to `go.mod` like:
```
go.opentelemetry.io/collector/config/configgrpc v0.150.0
```
`go.sum` updated automatically.

- [ ] **Step 2: Verify module resolves**

```bash
cd processor/retroactivesampling && go mod download go.opentelemetry.io/collector/config/configgrpc
```

Expected: exits 0, no error output.

---

### Task 2: Update config.go

**Files:**
- Modify: `processor/retroactivesampling/config.go`

- [ ] **Step 1: Replace the file**

```go
package retroactivesampling

import (
	"go.opentelemetry.io/collector/config/configgrpc"

	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/evaluator"
)

type Config struct {
	BufferFile              string                  `mapstructure:"buffer_file"`
	MaxBufferBytes          int64                   `mapstructure:"max_buffer_bytes"`
	MaxInterestCacheEntries int                     `mapstructure:"max_interest_cache_entries"`
	CoordinatorGRPC         configgrpc.ClientConfig `mapstructure:"coordinator_grpc"`
	Policies                []evaluator.PolicyCfg   `mapstructure:"policies"`
}

func (c *Config) Validate() error {
	return c.CoordinatorGRPC.Validate()
}
```

- [ ] **Step 2: Verify the package compiles (errors expected in processor.go — that's OK)**

```bash
cd processor/retroactivesampling && go build ./internal/... 2>&1 | head -20
```

Expected: no errors from `internal/...`. Errors in the top-level package (`processor.go` still references `CoordinatorEndpoint`) are expected at this stage.

---

### Task 3: Rewrite coordinator/client.go

**Files:**
- Modify: `processor/retroactivesampling/internal/coordinator/client.go`

- [ ] **Step 1: Replace the file**

```go
package coordinator

import (
	"context"
	"encoding/hex"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configgrpc"
	"go.uber.org/zap"

	gen "pitr.ca/retroactivesampling/proto"
)

type DecisionHandler func(traceID string)

type Client struct {
	grpcCfg  configgrpc.ClientConfig
	host     component.Host
	settings component.TelemetrySettings
	handler  DecisionHandler
	sendCh   chan string
	ctx      context.Context
	cancel   context.CancelFunc
	logger   *zap.Logger
	done     chan struct{}
}

func New(grpcCfg configgrpc.ClientConfig, host component.Host, settings component.TelemetrySettings, handler DecisionHandler, logger *zap.Logger) *Client {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		grpcCfg:  grpcCfg,
		host:     host,
		settings: settings,
		handler:  handler,
		sendCh:   make(chan string, 256),
		ctx:      ctx,
		cancel:   cancel,
		logger:   logger,
		done:     make(chan struct{}),
	}
	go func() { defer close(c.done); c.run() }()
	return c
}

func (c *Client) Notify(traceID string) {
	select {
	case c.sendCh <- traceID:
		c.logger.Debug("coordinator: queued notify", zap.String("trace_id", traceID), zap.Int("queue_len", len(c.sendCh)))
	default:
		c.logger.Warn("coordinator: send queue full, dropping notify", zap.String("trace_id", traceID))
	}
}

func (c *Client) Close() { c.cancel(); <-c.done }

func (c *Client) run() {
	backoff := time.Second
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}
		c.logger.Info("coordinator: connecting", zap.String("endpoint", c.grpcCfg.Endpoint))
		if err := c.connect(); err != nil {
			c.logger.Warn("coordinator: connection lost, retrying", zap.Error(err), zap.Duration("backoff", backoff))
			select {
			case <-c.ctx.Done():
				return
			case <-time.After(backoff):
				if backoff < 30*time.Second {
					backoff *= 2
				}
			}
		} else {
			c.logger.Info("coordinator: connection closed cleanly")
			backoff = time.Second
		}
	}
}

func (c *Client) connect() error {
	conn, err := c.grpcCfg.ToClientConn(c.ctx, c.host, c.settings)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	stream, err := gen.NewCoordinatorClient(conn).Connect(c.ctx)
	if err != nil {
		return err
	}
	c.logger.Info("coordinator: stream established", zap.String("endpoint", c.grpcCfg.Endpoint))

	recvErr := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			if d := msg.GetDecision(); d != nil {
				tid := hex.EncodeToString(d.TraceId)
				c.logger.Debug("coordinator: received decision", zap.String("trace_id", tid))
				c.handler(tid)
			}
		}
	}()

	for {
		select {
		case traceID := <-c.sendCh:
			tid, _ := hex.DecodeString(traceID)
			c.logger.Debug("coordinator: sending notify", zap.String("trace_id", traceID))
			if err := stream.Send(&gen.ProcessorMessage{
				Payload: &gen.ProcessorMessage_Notify{
					Notify: &gen.NotifyInteresting{TraceId: tid},
				},
			}); err != nil {
				c.logger.Error("coordinator: send failed", zap.String("trace_id", traceID), zap.Error(err))
				return err
			}
		case err := <-recvErr:
			c.logger.Error("coordinator: recv failed", zap.Error(err))
			return err
		case <-c.ctx.Done():
			return nil
		}
	}
}
```

- [ ] **Step 2: Verify coordinator package compiles**

```bash
cd processor/retroactivesampling && go build ./internal/coordinator/...
```

Expected: exits 0. (client_test.go will fail — that's expected, fixed in Task 6.)

---

### Task 4: Update processor.go

**Files:**
- Modify: `processor/retroactivesampling/processor.go`

- [ ] **Step 1: Replace the file**

```go
package retroactivesampling

import (
	"context"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configgrpc"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/processor/processorhelper"
	"go.uber.org/zap"

	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/buffer"
	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/cache"
	coord "pitr.ca/retroactivesampling/processor/retroactivesampling/internal/coordinator"
	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/evaluator"
	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/metadata"
)

type retroactiveProcessor struct {
	logger      *zap.Logger
	next        consumer.Traces
	buf         *buffer.SpanBuffer
	ic          *cache.InterestCache
	eval        evaluator.Evaluator
	coord       *coord.Client
	telemetry   *metadata.TelemetryBuilder
	grpcCfg     configgrpc.ClientConfig
	telSettings component.TelemetrySettings
}

func newProcessor(set component.TelemetrySettings, cfg *Config, next consumer.Traces) (*retroactiveProcessor, error) {
	tb, err := metadata.NewTelemetryBuilder(set)
	if err != nil {
		return nil, err
	}
	obs := func(d time.Duration) {
		tb.RetroactiveSamplingBufferSpanAgeOnEviction.Record(context.Background(), d.Milliseconds())
	}
	ic := cache.New(cfg.MaxInterestCacheEntries)
	buf, err := buffer.New(cfg.BufferFile, cfg.MaxBufferBytes, obs)
	if err != nil {
		tb.Shutdown()
		return nil, err
	}
	chain, err := evaluator.Build(set, cfg.Policies)
	if err != nil {
		_ = buf.Close()
		tb.Shutdown()
		return nil, err
	}
	return &retroactiveProcessor{
		logger:      set.Logger,
		next:        next,
		buf:         buf,
		ic:          ic,
		eval:        chain,
		telemetry:   tb,
		grpcCfg:     cfg.CoordinatorGRPC,
		telSettings: set,
	}, nil
}

func (p *retroactiveProcessor) Start(_ context.Context, host component.Host) error {
	p.coord = coord.New(p.grpcCfg, host, p.telSettings, p.onDecision, p.logger)
	return nil
}

func (p *retroactiveProcessor) Shutdown(_ context.Context) error {
	if p.coord != nil {
		p.coord.Close()
	}
	err := p.buf.Close()
	p.telemetry.Shutdown()
	return err
}

func (p *retroactiveProcessor) processTraces(_ context.Context, td ptrace.Traces) (ptrace.Traces, error) {
	out := ptrace.NewTraces()
	now := time.Now()
	for traceID, spans := range groupByTrace(td) {
		if p.ic.Has(traceID) {
			spans.ResourceSpans().MoveAndAppendTo(out.ResourceSpans())
			continue
		}
		d, err := p.eval.Evaluate(spans)
		if err != nil {
			p.logger.Warn("policy evaluation error", zap.String("trace_id", traceID), zap.Error(err))
		}
		switch d {
		case evaluator.Sampled:
			p.ingestInteresting(traceID, spans)
			continue
		case evaluator.SampledLocal:
			p.ingestLocal(traceID, spans)
			continue
		}
		if err := p.buf.WriteWithEviction(traceID, spans, now); err != nil {
			p.logger.Error("buffer full after eviction", zap.String("trace_id", traceID), zap.Error(err))
			spans.ResourceSpans().MoveAndAppendTo(out.ResourceSpans())
			continue
		}
	}
	if out.SpanCount() == 0 {
		return out, processorhelper.ErrSkipProcessingData
	}
	return out, nil
}

func (p *retroactiveProcessor) ingestInteresting(traceID string, current ptrace.Traces) {
	p.ic.Add(traceID)
	p.coord.Notify(traceID)
	p.ingestTrace(traceID, current)
}

func (p *retroactiveProcessor) ingestLocal(traceID string, current ptrace.Traces) {
	p.ic.Add(traceID)
	p.ingestTrace(traceID, current)
}

func (p *retroactiveProcessor) ingestTrace(traceID string, current ptrace.Traces) {
	buffered, ok, err := p.buf.ReadAndDelete(traceID)
	if err != nil {
		p.logger.Warn("read buffer", zap.String("trace_id", traceID), zap.Error(err))
		if err2 := p.next.ConsumeTraces(context.Background(), current); err2 != nil {
			p.logger.Error("ingest trace", zap.String("trace_id", traceID), zap.Error(err2))
		}
		return
	}
	if ok {
		current.ResourceSpans().MoveAndAppendTo(buffered.ResourceSpans())
		current = buffered
	}
	if err := p.next.ConsumeTraces(context.Background(), current); err != nil {
		p.logger.Error("ingest trace", zap.String("trace_id", traceID), zap.Error(err))
	}
}

func (p *retroactiveProcessor) onDecision(traceID string) {
	p.logger.Debug("coordinator decision received", zap.String("trace_id", traceID))
	p.ic.Add(traceID)
	traces, ok, err := p.buf.ReadAndDelete(traceID)
	if err != nil {
		p.logger.Warn("coordinator decision: error fetching buffered trace", zap.String("trace_id", traceID), zap.Error(err))
		return
	}
	if !ok {
		return
	}
	if err := p.next.ConsumeTraces(context.Background(), traces); err != nil {
		p.logger.Error("ingest coordinator-decided trace", zap.String("trace_id", traceID), zap.Error(err))
	}
}
```

- [ ] **Step 2: Verify top-level package compiles (excluding tests)**

```bash
cd processor/retroactivesampling && go build .
```

Expected: exits 0.

---

### Task 5: Update factory.go

**Files:**
- Modify: `processor/retroactivesampling/factory.go`

- [ ] **Step 1: Add `WithStart` to the processorhelper call**

Replace the `processorhelper.NewTraces(...)` call:

```go
return processorhelper.NewTraces(ctx, set, cfg, next, p.processTraces,
    processorhelper.WithStart(p.Start),
    processorhelper.WithShutdown(p.Shutdown),
    processorhelper.WithCapabilities(consumer.Capabilities{MutatesData: false}),
)
```

The full file after change:

```go
package retroactivesampling

import (
	"context"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	otelprocessor "go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/processor/processorhelper"

	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/metadata"
)

func NewFactory() otelprocessor.Factory {
	return otelprocessor.NewFactory(
		metadata.Type,
		createDefaultConfig,
		otelprocessor.WithTraces(createTracesProcessor, metadata.TracesStability),
	)
}

func createDefaultConfig() component.Config {
	return &Config{
		MaxInterestCacheEntries: 100_000,
	}
}

func createTracesProcessor(
	ctx context.Context,
	set otelprocessor.Settings,
	cfg component.Config,
	next consumer.Traces,
) (otelprocessor.Traces, error) {
	p, err := newProcessor(set.TelemetrySettings, cfg.(*Config), next)
	if err != nil {
		return nil, err
	}
	return processorhelper.NewTraces(ctx, set, cfg, next, p.processTraces,
		processorhelper.WithStart(p.Start),
		processorhelper.WithShutdown(p.Shutdown),
		processorhelper.WithCapabilities(consumer.Capabilities{MutatesData: false}),
	)
}
```

- [ ] **Step 2: Verify top-level package builds**

```bash
cd processor/retroactivesampling && go build .
```

Expected: exits 0.

---

### Task 6: Fix test files

**Files:**
- Modify: `processor/retroactivesampling/internal/coordinator/client_test.go`
- Modify: `processor/retroactivesampling/processor_test.go`
- Modify: `processor/retroactivesampling/integration_test.go`

- [ ] **Step 1: Update coordinator/client_test.go**

Two changes: `coord.New(...)` calls gain three new leading params. Replace the full file:

```go
package coordinator_test

import (
	"encoding/hex"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/config/configgrpc"
	"go.opentelemetry.io/collector/config/configtls"
	"go.uber.org/zap"
	"google.golang.org/grpc"

	gen "pitr.ca/retroactivesampling/proto"
	coord "pitr.ca/retroactivesampling/processor/retroactivesampling/internal/coordinator"
)

// fakeServer records notifications and allows broadcasting decisions.
type fakeServer struct {
	gen.UnimplementedCoordinatorServer
	mu       sync.Mutex
	notified []string
	streams  []gen.Coordinator_ConnectServer
}

func (f *fakeServer) Connect(stream gen.Coordinator_ConnectServer) error {
	f.mu.Lock()
	f.streams = append(f.streams, stream)
	f.mu.Unlock()
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if n := msg.GetNotify(); n != nil {
			f.mu.Lock()
			f.notified = append(f.notified, hex.EncodeToString(n.TraceId))
			f.mu.Unlock()
		}
	}
}

func (f *fakeServer) broadcast(traceID string) {
	tid, _ := hex.DecodeString(traceID)
	msg := &gen.CoordinatorMessage{
		Payload: &gen.CoordinatorMessage_Decision{
			Decision: &gen.TraceDecision{TraceId: tid},
		},
	}
	f.mu.Lock()
	for _, s := range f.streams {
		_ = s.Send(msg)
	}
	f.mu.Unlock()
}

func startFakeServer(t *testing.T) (*fakeServer, string) {
	t.Helper()
	srv := &fakeServer{}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	gs := grpc.NewServer()
	gen.RegisterCoordinatorServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	return srv, lis.Addr().String()
}

func insecureGRPCConfig(addr string) configgrpc.ClientConfig {
	return configgrpc.ClientConfig{
		Endpoint:   addr,
		TLSSetting: configtls.ClientConfig{Insecure: true},
	}
}

func TestClientSendsNotification(t *testing.T) {
	const traceHex = "aabbccdd99999999aabbccdd99999999"
	srv, addr := startFakeServer(t)
	c := coord.New(insecureGRPCConfig(addr), componenttest.NewNopHost(), component.TelemetrySettings{}, func(string) {}, zap.NewNop())
	t.Cleanup(c.Close)

	time.Sleep(100 * time.Millisecond) // connection establishment
	c.Notify(traceHex)
	time.Sleep(100 * time.Millisecond)

	srv.mu.Lock()
	notified := srv.notified
	srv.mu.Unlock()
	require.Contains(t, notified, traceHex)
}

func TestClientReceivesDecision(t *testing.T) {
	const traceHex = "aabbccdd55555555aabbccdd55555555"
	srv, addr := startFakeServer(t)
	received := make(chan string, 1)
	c := coord.New(insecureGRPCConfig(addr), componenttest.NewNopHost(), component.TelemetrySettings{}, func(traceID string) { received <- traceID }, zap.NewNop())
	t.Cleanup(c.Close)

	time.Sleep(100 * time.Millisecond)
	srv.broadcast(traceHex)

	select {
	case id := <-received:
		assert.Equal(t, traceHex, id)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for decision")
	}
}
```

- [ ] **Step 2: Verify coordinator tests compile and pass**

```bash
cd processor/retroactivesampling && go test ./internal/coordinator/... -v -count=1
```

Expected: both tests PASS.

- [ ] **Step 3: Update processor_test.go**

Two changes: `CoordinatorEndpoint: addr` → `CoordinatorGRPC: insecureGRPCConfig(addr)` and `Start(ctx, nil)` → `Start(ctx, componenttest.NewNopHost())`.

Add helper at the top of the test file (after imports):

```go
func insecureGRPCConfig(addr string) configgrpc.ClientConfig {
	return configgrpc.ClientConfig{
		Endpoint:   addr,
		TLSSetting: configtls.ClientConfig{Insecure: true},
	}
}
```

Update imports to add:
```go
"go.opentelemetry.io/collector/component/componenttest"
"go.opentelemetry.io/collector/config/configgrpc"
"go.opentelemetry.io/collector/config/configtls"
```

In `newTestProcessor`, change:
```go
// Before:
CoordinatorEndpoint: addr,
// ...
require.NoError(t, proc.Start(context.Background(), nil))

// After:
CoordinatorGRPC: insecureGRPCConfig(addr),
// ...
require.NoError(t, proc.Start(context.Background(), componenttest.NewNopHost()))
```

In `TestProbabilistic_NoCoordinatorNotify`, change:
```go
// Before:
CoordinatorEndpoint: addr,
// ...
require.NoError(t, proc.Start(context.Background(), nil))

// After:
CoordinatorGRPC: insecureGRPCConfig(addr),
// ...
require.NoError(t, proc.Start(context.Background(), componenttest.NewNopHost()))
```

- [ ] **Step 4: Verify processor tests compile and pass**

```bash
cd processor/retroactivesampling && go test . -v -count=1 -timeout=30s
```

Expected: all tests PASS.

- [ ] **Step 5: Update integration_test.go**

In `newE2EProcessor`, change:
```go
// Before:
CoordinatorEndpoint: coordAddr,
// ...
require.NoError(t, p.Start(context.Background(), nil))

// After:
CoordinatorGRPC: configgrpc.ClientConfig{
    Endpoint:   coordAddr,
    TLSSetting: configtls.ClientConfig{Insecure: true},
},
// ...
require.NoError(t, p.Start(context.Background(), componenttest.NewNopHost()))
```

Update imports to add:
```go
"go.opentelemetry.io/collector/component/componenttest"
"go.opentelemetry.io/collector/config/configgrpc"
"go.opentelemetry.io/collector/config/configtls"
```

- [ ] **Step 6: Verify integration test compiles**

```bash
cd processor/retroactivesampling && go build -tags integration ./...
```

Expected: exits 0.

- [ ] **Step 7: Commit test and implementation changes**

```bash
cd processor/retroactivesampling && git add -p
git commit -m "feat(processor): replace coordinator_endpoint string with configgrpc.ClientConfig"
```

---

### Task 7: Update metadata.yaml and example YAMLs

**Files:**
- Modify: `processor/retroactivesampling/metadata.yaml`
- Modify: `example/otelcol1.yaml`
- Modify: `example/otelcol2.yaml`

- [ ] **Step 1: Update metadata.yaml test config**

In `processor/retroactivesampling/metadata.yaml`, change:
```yaml
# Before:
tests:
  config:
    buffer_file: /tmp/retrosampling-lifecycle-test.ring
    max_buffer_bytes: 1048576
    coordinator_endpoint: "127.0.0.1:4317"

# After:
tests:
  config:
    buffer_file: /tmp/retrosampling-lifecycle-test.ring
    max_buffer_bytes: 1048576
    coordinator_grpc:
      endpoint: "127.0.0.1:4317"
      tls:
        insecure: true
```

- [ ] **Step 2: Update example/otelcol1.yaml**

Replace:
```yaml
    coordinator_endpoint: localhost:9090
```
With:
```yaml
    coordinator_grpc:
      endpoint: localhost:9090
      tls:
        insecure: true
```

- [ ] **Step 3: Update example/otelcol2.yaml**

Replace:
```yaml
    coordinator_endpoint: localhost:9090
```
With:
```yaml
    coordinator_grpc:
      endpoint: localhost:9090
      tls:
        insecure: true
```

- [ ] **Step 4: Verify generated lifecycle test still compiles**

```bash
cd processor/retroactivesampling && go test -run TestComponentLifecycle -v -count=1 2>&1 | head -30
```

Expected: test passes (it exercises the generated lifecycle test which reads `metadata.yaml`). If it errors on unknown field `coordinator_endpoint`, the metadata.yaml update fixed it.

- [ ] **Step 5: Commit**

```bash
git add processor/retroactivesampling/metadata.yaml example/otelcol1.yaml example/otelcol2.yaml
git commit -m "docs: update coordinator_endpoint → coordinator_grpc in examples and metadata"
```

---

### Task 8: go mod tidy, full build and test, remove TODO item

**Files:**
- Modify: `processor/retroactivesampling/go.mod` (tidy)
- Modify: `TODO.md` (remove item)

- [ ] **Step 1: Run go mod tidy**

```bash
cd processor/retroactivesampling && go mod tidy
```

Expected: exits 0. `configtls` may be added to `go.mod` as a direct dependency if it wasn't already.

- [ ] **Step 2: Build all packages**

```bash
cd /Users/p.vernigorov/code/retroactivesampling && go build ./...
```

Expected: exits 0, no errors.

- [ ] **Step 3: Run all unit tests**

```bash
cd processor/retroactivesampling && go test ./... -count=1 -timeout=60s
```

Expected: all PASS.

- [ ] **Step 4: Remove item from TODO.md**

Remove this line from `TODO.md`:
```
- [ ] coordinator_endpoint string config in processor should instead be go.opentelemetry.io/collector/config/configgrpc
```

- [ ] **Step 5: Commit**

```bash
git add processor/retroactivesampling/go.mod processor/retroactivesampling/go.sum TODO.md
git commit -m "chore: go mod tidy after configgrpc migration; remove completed TODO"
```
