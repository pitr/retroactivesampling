//go:build integration

package processor_test

import (
	"context"
	"encoding/hex"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/processor/processortest"
	"google.golang.org/grpc"
	otelprocessor "go.opentelemetry.io/collector/processor"

	gen "pitr.ca/retroactivesampling/proto"
	proc "pitr.ca/retroactivesampling/processor/retroactivesampling"
	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/evaluator"
)

// stubCoordinator is a minimal in-process gRPC coordinator: broadcasts keep
// decisions to all connected processors when any one notifies an interesting trace.
type stubCoordinator struct {
	gen.UnimplementedCoordinatorServer
	mu      sync.Mutex
	streams []gen.Coordinator_ConnectServer
}

func (s *stubCoordinator) Connect(stream gen.Coordinator_ConnectServer) error {
	s.mu.Lock()
	s.streams = append(s.streams, stream)
	s.mu.Unlock()
	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		if n := msg.GetNotify(); n != nil {
			s.broadcast(n.TraceId)
		}
	}
}

func (s *stubCoordinator) broadcast(traceID string) {
	decision := &gen.CoordinatorMessage{
		Payload: &gen.CoordinatorMessage_Decision{
			Decision: &gen.TraceDecision{TraceId: traceID, Keep: true},
		},
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, stream := range s.streams {
		_ = stream.Send(decision)
	}
}

func startCoordinator(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	gs := grpc.NewServer()
	gen.RegisterCoordinatorServer(gs, &stubCoordinator{})
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	return lis.Addr().String()
}

func newE2EProcessor(t *testing.T, coordAddr string, sink *consumertest.TracesSink) otelprocessor.Traces {
	t.Helper()
	cfg := &proc.Config{
		BufferDir:           t.TempDir(),
		DropTTL:             2 * time.Second,
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
	coordAddr := startCoordinator(t)

	sinkA := &consumertest.TracesSink{}
	sinkB := &consumertest.TracesSink{}
	procA := newE2EProcessor(t, coordAddr, sinkA)
	procB := newE2EProcessor(t, coordAddr, sinkB)

	traceIDHex := "aabbccddeeff00112233445566778899"

	// B receives ok span — buffered immediately (not interesting).
	require.NoError(t, procB.ConsumeTraces(context.Background(), spanWithStatus(traceIDHex, ptrace.StatusCodeOk)))
	assert.Equal(t, 0, sinkB.SpanCount(), "ok span should be buffered on B")

	// A receives error span — ingested immediately and coordinator notified.
	require.NoError(t, procA.ConsumeTraces(context.Background(), spanWithStatus(traceIDHex, ptrace.StatusCodeError)))
	assert.Equal(t, 1, sinkA.SpanCount(), "processor A should ingest its error span immediately")

	require.Eventually(t, func() bool {
		return sinkB.SpanCount() == 1
	}, 3*time.Second, 20*time.Millisecond, "processor B should ingest its buffered span after coordinator push")
}
