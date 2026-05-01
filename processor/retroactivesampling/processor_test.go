package retroactivesampling_test

import (
	"context"
	"encoding/hex"
	"io"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/config/configgrpc"
	"go.opentelemetry.io/collector/config/configtls"
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
			f.notified = append(f.notified, hex.EncodeToString(n.TraceId))
			f.mu.Unlock()
		}
	}
}

func (f *fakeCoordinator) sendDecision(traceID string) {
	tid, _ := hex.DecodeString(traceID)
	msg := &gen.CoordinatorMessage{
		Payload: &gen.CoordinatorMessage_Batch{
			Batch: &gen.BatchTraceDecision{TraceIds: [][]byte{tid}},
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

func insecureGRPCConfig(addr string) configgrpc.ClientConfig {
	return configgrpc.ClientConfig{
		Endpoint:   addr,
		TLS: configtls.ClientConfig{Insecure: true},
	}
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
	p := evaluator.PolicyCfg{}
	p.Name = "errors"
	p.Type = evaluator.StatusCode
	p.StatusCodeCfg = evaluator.StatusCodeCfg{StatusCodes: []string{"ERROR"}}
	cfg := &processor.Config{
		BufferFile:              filepath.Join(t.TempDir(), "buf.ring"),
		MaxBufferBytes:          100 << 20,
		ChunkSize:               4096,
		DecisionWaitTime:        300 * time.Millisecond,
		CoordinatorGRPC:         insecureGRPCConfig(addr),
		Policies:                []evaluator.PolicyCfg{p},
	}
	factory := processor.NewFactory()
	proc, err := factory.CreateTraces(
		context.Background(),
		processortest.NewNopSettings(factory.Type()),
		cfg,
		sink,
	)
	require.NoError(t, err)
	require.NoError(t, proc.Start(context.Background(), componenttest.NewNopHost()))
	t.Cleanup(func() { _ = proc.Shutdown(context.Background()) })
	return proc
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
	fc.sendDecision(tid3)
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

	// Buffered ok span is delivered async via sweeper; error span is forwarded synchronously.
	require.Eventually(t, func() bool {
		return sink.SpanCount() == 2
	}, 2*time.Second, 5*time.Millisecond, "buffered ok span and error span must both be ingested")
}

// TestConcurrentInterestingSpansSameTrace verifies no double-write when two goroutines
// concurrently decide the same trace is interesting.
func TestConcurrentInterestingSpansSameTrace(t *testing.T) {
	_, addr := startFakeCoordinator(t)
	sink := &consumertest.TracesSink{}
	p := newTestProcessor(t, addr, sink)

	const tid = "aabbccdd66666666aabbccdd66666666"

	// Pre-buffer one ok span.
	require.NoError(t, p.ConsumeTraces(context.Background(), makeTraceWithStatus(tid, ptrace.StatusCodeOk)))
	assert.Equal(t, 0, sink.SpanCount(), "ok span should be buffered, not ingested")

	// Release two goroutines simultaneously, each delivering an error span for the same trace.
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	done.Add(2)
	for range 2 {
		go func() {
			start.Wait()
			_ = p.ConsumeTraces(context.Background(), makeTraceWithStatus(tid, ptrace.StatusCodeError))
			done.Done()
		}()
	}
	start.Done()
	done.Wait()

	// 1 pre-buffered ok span (async delivery) + 2 error spans = 3. Must not be 4 (double-buffered).
	require.Eventually(t, func() bool {
		return sink.SpanCount() >= 3
	}, 2*time.Second, 5*time.Millisecond, "buffered span must not be duplicated")
	assert.Equal(t, 3, sink.SpanCount(), "buffered span must not be duplicated")
}

// TestConcurrentInterestingAndCoordinatorDecision verifies no double-write when
// ingestInteresting and onDecision race to consume the same buffered trace.
func TestConcurrentInterestingAndCoordinatorDecision(t *testing.T) {
	fc, addr := startFakeCoordinator(t)
	sink := &consumertest.TracesSink{}
	p := newTestProcessor(t, addr, sink)

	const tid = "aabbccdd77777777aabbccdd77777777"

	// Wait for the gRPC stream to establish before pre-buffering.
	require.Eventually(t, func() bool {
		fc.mu.Lock()
		defer fc.mu.Unlock()
		return len(fc.streams) > 0
	}, 2*time.Second, 10*time.Millisecond, "coordinator stream must connect")

	// Pre-buffer one ok span.
	require.NoError(t, p.ConsumeTraces(context.Background(), makeTraceWithStatus(tid, ptrace.StatusCodeOk)))
	assert.Equal(t, 0, sink.SpanCount(), "ok span should be buffered")

	// Concurrently: error span via ConsumeTraces AND coordinator decision.
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	done.Add(2)
	go func() {
		start.Wait()
		_ = p.ConsumeTraces(context.Background(), makeTraceWithStatus(tid, ptrace.StatusCodeError))
		done.Done()
	}()
	go func() {
		start.Wait()
		fc.sendDecision(tid)
		done.Done()
	}()
	start.Done()
	done.Wait()

	// 1 pre-buffered ok + 1 error = 2 total. Must not be 3 (double-buffered).
	require.Eventually(t, func() bool {
		return sink.SpanCount() >= 2
	}, 2*time.Second, 10*time.Millisecond, "spans must be ingested")
	assert.Never(t, func() bool { return sink.SpanCount() > 2 }, 50*time.Millisecond, 5*time.Millisecond, "double-write: buffered span must not be duplicated")
}

func TestProbabilistic_NoCoordinatorNotify(t *testing.T) {
	fc, addr := startFakeCoordinator(t)
	sink := &consumertest.TracesSink{}

	p := evaluator.PolicyCfg{}
	p.Name = "prob"
	p.Type = evaluator.Probabilistic
	p.ProbabilisticCfg = evaluator.ProbabilisticCfg{SamplingPercentage: 100}
	cfg := &processor.Config{
		BufferFile:       filepath.Join(t.TempDir(), "buf.ring"),
		MaxBufferBytes:   100 << 20,
		ChunkSize:        4096,
		DecisionWaitTime: 300 * time.Millisecond,
		CoordinatorGRPC:  insecureGRPCConfig(addr),
		Policies:         []evaluator.PolicyCfg{p},
	}
	factory := processor.NewFactory()
	proc, err := factory.CreateTraces(
		context.Background(),
		processortest.NewNopSettings(factory.Type()),
		cfg,
		sink,
	)
	require.NoError(t, err)
	require.NoError(t, proc.Start(context.Background(), componenttest.NewNopHost()))
	t.Cleanup(func() { _ = proc.Shutdown(context.Background()) })

	trace := makeTraceWithStatus("aabbccdd11111111aabbccdd11111111", ptrace.StatusCodeOk)
	require.NoError(t, proc.ConsumeTraces(context.Background(), trace))

	assert.Equal(t, 1, sink.SpanCount())

	time.Sleep(100 * time.Millisecond)
	fc.mu.Lock()
	notified := len(fc.notified)
	fc.mu.Unlock()
	assert.Equal(t, 0, notified, "probabilistic policy must not notify coordinator")
}
