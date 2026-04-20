package processor_test

import (
	"context"
	"encoding/hex"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	otelprocessor "go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/processor/processortest"
	"google.golang.org/grpc"

	gen "pitr.ca/retroactivesampling/proto"
	processor "pitr.ca/retroactivesampling/processor/retroactivesampling"
	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/evaluator"
)

// fakeCoordinator records notifications and allows triggering decisions.
type fakeCoordinator struct {
	gen.UnimplementedCoordinatorServer
	mu       sync.Mutex
	notified []string
	streams  []gen.Coordinator_ConnectServer
}

func (f *fakeCoordinator) Connect(stream gen.Coordinator_ConnectServer) error {
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
			f.notified = append(f.notified, n.TraceId)
			f.mu.Unlock()
		}
	}
}

func (f *fakeCoordinator) sendDecision(traceID string, keep bool) {
	msg := &gen.CoordinatorMessage{
		Payload: &gen.CoordinatorMessage_Decision{
			Decision: &gen.TraceDecision{TraceId: traceID, Keep: keep},
		},
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.streams {
		_ = s.Send(msg)
	}
}

func startFakeCoordinator(t *testing.T) (*fakeCoordinator, string) {
	t.Helper()
	fc := &fakeCoordinator{}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	gs := grpc.NewServer()
	gen.RegisterCoordinatorServer(gs, fc)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	return fc, lis.Addr().String()
}

func makeTraceWithStatus(traceIDHex string, status ptrace.StatusCode) ptrace.Traces {
	td := ptrace.NewTraces()
	span := td.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	var tid pcommon.TraceID
	b, _ := hex.DecodeString(traceIDHex)
	copy(tid[:], b)
	span.SetTraceID(tid)
	span.Status().SetCode(status)
	now := time.Now()
	span.SetStartTimestamp(pcommon.NewTimestampFromTime(now))
	span.SetEndTimestamp(pcommon.NewTimestampFromTime(now.Add(100 * time.Millisecond)))
	return td
}

func newTestProcessor(t *testing.T, addr string, sink *consumertest.TracesSink) otelprocessor.Traces {
	t.Helper()
	cfg := &processor.Config{
		BufferDir:               t.TempDir(),
		MaxBufferBytes:          100 << 20,
		MaxInterestCacheEntries: 1000,
		CoordinatorEndpoint:     addr,
		Rules: []evaluator.RuleConfig{
			{Type: "error_status"},
		},
	}
	factory := processor.NewFactory()
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

func TestInterestingTraceIngestedImmediately(t *testing.T) {
	fc, addr := startFakeCoordinator(t)
	sink := &consumertest.TracesSink{}
	p := newTestProcessor(t, addr, sink)

	errTrace := makeTraceWithStatus("aabbccdd11111111aabbccdd11111111", ptrace.StatusCodeError)
	require.NoError(t, p.ConsumeTraces(context.Background(), errTrace))

	assert.Equal(t, 1, sink.SpanCount(), "error trace should be ingested")

	require.Eventually(t, func() bool {
		fc.mu.Lock()
		defer fc.mu.Unlock()
		return len(fc.notified) > 0
	}, 2*time.Second, 10*time.Millisecond, "coordinator should be notified of interesting trace")
}

func TestCoordinatorPushCausesIngestion(t *testing.T) {
	fc, addr := startFakeCoordinator(t)
	sink := &consumertest.TracesSink{}
	p := newTestProcessor(t, addr, sink)

	const tid3 = "aabbccdd33333333aabbccdd33333333"
	okTrace := makeTraceWithStatus(tid3, ptrace.StatusCodeOk)
	require.NoError(t, p.ConsumeTraces(context.Background(), okTrace))

	// ok span is not interesting: buffered immediately, nothing ingested.
	assert.Equal(t, 0, sink.SpanCount())

	// Wait for gRPC stream to establish before sending decision.
	require.Eventually(t, func() bool {
		fc.mu.Lock()
		defer fc.mu.Unlock()
		return len(fc.streams) > 0
	}, 2*time.Second, 10*time.Millisecond, "coordinator stream should connect")

	// Coordinator signals: keep this trace.
	fc.sendDecision(tid3, true)
	require.Eventually(t, func() bool {
		return sink.SpanCount() == 1
	}, 2*time.Second, 10*time.Millisecond, "coordinator push should trigger ingestion")
}

func TestInterestingSpanIngestedWithoutDelay(t *testing.T) {
	_, addr := startFakeCoordinator(t)
	sink := &consumertest.TracesSink{}
	p := newTestProcessor(t, addr, sink)

	errTrace := makeTraceWithStatus("aabbccdd44444444aabbccdd44444444", ptrace.StatusCodeError)
	require.NoError(t, p.ConsumeTraces(context.Background(), errTrace))

	// Eager evaluation: interesting span ingested synchronously, no timer wait.
	assert.Equal(t, 1, sink.SpanCount())
}

// TestEagerEval_BufferedSpansIncludedOnInterestingBatch: a non-interesting span is buffered first;
// a later interesting span for the same trace triggers ingestion of both.
func TestEagerEval_BufferedSpansIncludedOnInterestingBatch(t *testing.T) {
	_, addr := startFakeCoordinator(t)
	sink := &consumertest.TracesSink{}
	p := newTestProcessor(t, addr, sink)

	const tid = "aabbccdd55555555aabbccdd55555555"

	okTrace := makeTraceWithStatus(tid, ptrace.StatusCodeOk)
	require.NoError(t, p.ConsumeTraces(context.Background(), okTrace))
	assert.Equal(t, 0, sink.SpanCount(), "ok span should be buffered")

	errTrace := makeTraceWithStatus(tid, ptrace.StatusCodeError)
	require.NoError(t, p.ConsumeTraces(context.Background(), errTrace))

	// Both spans (buffered ok + current error) ingested in one ConsumeTraces call.
	assert.Equal(t, 2, sink.SpanCount(), "buffered ok span and error span must both be ingested")
}
