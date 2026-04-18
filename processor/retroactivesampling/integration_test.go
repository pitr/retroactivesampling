//go:build integration

package integration_test

import (
	"context"
	"encoding/hex"
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/processor/processortest"
	"google.golang.org/grpc"
	otelprocessor "go.opentelemetry.io/collector/processor"

	gen "pitr.ca/retroactivesampling/proto"
	rds "pitr.ca/retroactivesampling/coordinator/redis"
	coordserver "pitr.ca/retroactivesampling/coordinator/server"
	proc "pitr.ca/retroactivesampling/processor/retroactivesampling"
	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/evaluator"
)

func startRedis(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "redis:7-alpine",
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor:   wait.ForLog("Ready to accept connections"),
		},
		Started: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Terminate(ctx) })
	host, err := c.Host(ctx)
	require.NoError(t, err)
	port, err := c.MappedPort(ctx, "6379")
	require.NoError(t, err)
	return host + ":" + port.Port()
}

func startCoordinator(t *testing.T, redisAddr string) string {
	t.Helper()
	ps := rds.New(redisAddr, 60*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = ps.Close() })

	srv := coordserver.New(func(traceID string) {
		ok, err := ps.Publish(ctx, traceID)
		if err != nil || !ok {
			return
		}
	})
	go func() {
		_ = ps.Subscribe(ctx, func(traceID string) {
			srv.Broadcast(traceID, true)
		})
	}()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	gs := grpc.NewServer()
	gen.RegisterCoordinatorServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	return lis.Addr().String()
}

func newTestProcessor(t *testing.T, coordAddr string, sink *consumertest.TracesSink) otelprocessor.Traces {
	t.Helper()
	f, err := os.CreateTemp("", "e2e-proc-*.db")
	require.NoError(t, err)
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })

	cfg := &proc.Config{
		BufferDBPath:        f.Name(),
		BufferTTL:           200 * time.Millisecond,
		DropTTL:             2 * time.Second,
		InterestCacheTTL:    10 * time.Second,
		CoordinatorEndpoint: coordAddr,
		Rules:               []evaluator.RuleConfig{{Type: "error_status"}},
	}
	factory := proc.NewFactory()
	p, err := factory.CreateTraces(
		context.Background(),
		processortest.NewNopSettings(factory.Type()),
		cfg,
		sink,
	)
	require.NoError(t, err)
	require.NoError(t, p.Start(context.Background(), nil))
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return p
}

func spanWithStatus(traceIDHex string, status ptrace.StatusCode) ptrace.Traces {
	td := ptrace.NewTraces()
	span := td.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	var tid pcommon.TraceID
	b, _ := hex.DecodeString(traceIDHex)
	copy(tid[:], b)
	span.SetTraceID(tid)
	span.Status().SetCode(status)
	now := time.Now()
	span.SetStartTimestamp(pcommon.NewTimestampFromTime(now))
	span.SetEndTimestamp(pcommon.NewTimestampFromTime(now.Add(50 * time.Millisecond)))
	return td
}

// TestE2E_ErrorPropagatesAcrossTwoProcessors: Processor A sees error span → ingests + notifies coordinator →
// coordinator broadcasts → Processor B (holding non-interesting spans of same trace) ingests them.
func TestE2E_ErrorPropagatesAcrossTwoProcessors(t *testing.T) {
	redisAddr := startRedis(t)
	coordAddr := startCoordinator(t, redisAddr)

	sinkA := &consumertest.TracesSink{}
	sinkB := &consumertest.TracesSink{}
	procA := newTestProcessor(t, coordAddr, sinkA)
	procB := newTestProcessor(t, coordAddr, sinkB)

	// Use a valid 32-char hex trace ID
	traceIDHex := "aabbccddeeff00112233445566778899"

	// Processor B buffers non-interesting span for the trace
	require.NoError(t, procB.ConsumeTraces(context.Background(), spanWithStatus(traceIDHex, ptrace.StatusCodeOk)))

	time.Sleep(300 * time.Millisecond) // B evaluates: not interesting, holds in buffer

	// Processor A sees error span for same trace
	require.NoError(t, procA.ConsumeTraces(context.Background(), spanWithStatus(traceIDHex, ptrace.StatusCodeError)))

	// Budget: A buffer_ttl(200ms) + Redis roundtrip + gRPC broadcast + B onDecision; 1s gives margin on slow CI.
	time.Sleep(1000 * time.Millisecond)

	assert.Equal(t, 1, sinkA.SpanCount(), "processor A should ingest its error span")
	assert.Equal(t, 1, sinkB.SpanCount(), "processor B should ingest its buffered span after coordinator push")
}
