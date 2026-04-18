package processor_test

import (
	"context"
	"encoding/hex"
	"io"
	"net"
	"os"
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

	gen "retroactivesampling/proto"
	processor "retroactivesampling/processor/retroactivesampling"
	"retroactivesampling/processor/retroactivesampling/internal/evaluator"
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
	f, err := os.CreateTemp("", "proc-*.db")
	require.NoError(t, err)
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })

	cfg := &processor.Config{
		BufferDBPath:        f.Name(),
		BufferTTL:           200 * time.Millisecond,
		DropTTL:             500 * time.Millisecond,
		InterestCacheTTL:    5 * time.Second,
		CoordinatorEndpoint: addr,
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

	// Wait for buffer_ttl + processing
	time.Sleep(400 * time.Millisecond)

	assert.Equal(t, 1, sink.SpanCount(), "error trace should be ingested")

	// Coordinator should be notified
	time.Sleep(100 * time.Millisecond)
	fc.mu.Lock()
	notified := fc.notified
	fc.mu.Unlock()
	assert.NotEmpty(t, notified, "coordinator should be notified of interesting trace")
}

func TestNonInterestingTraceDropped(t *testing.T) {
	_, addr := startFakeCoordinator(t)
	sink := &consumertest.TracesSink{}
	p := newTestProcessor(t, addr, sink)

	okTrace := makeTraceWithStatus("aabbccdd22222222aabbccdd22222222", ptrace.StatusCodeOk)
	require.NoError(t, p.ConsumeTraces(context.Background(), okTrace))

	// Wait for buffer_ttl + drop_ttl
	time.Sleep(800 * time.Millisecond)

	assert.Equal(t, 0, sink.SpanCount(), "non-interesting trace should be dropped")
}

func TestCoordinatorPushCausesIngestion(t *testing.T) {
	fc, addr := startFakeCoordinator(t)
	sink := &consumertest.TracesSink{}
	p := newTestProcessor(t, addr, sink)

	const tid3 = "aabbccdd33333333aabbccdd33333333"
	okTrace := makeTraceWithStatus(tid3, ptrace.StatusCodeOk)
	require.NoError(t, p.ConsumeTraces(context.Background(), okTrace))

	// Wait for buffer_ttl evaluation (no ingest yet)
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, 0, sink.SpanCount())

	// Coordinator signals: keep this trace
	fc.sendDecision(tid3, true)
	time.Sleep(100 * time.Millisecond)

	assert.Equal(t, 1, sink.SpanCount(), "coordinator push should trigger ingestion")
}
