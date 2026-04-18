package coordinator_test

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	gen "retroactivesampling/proto"
	coord "retroactivesampling/processor/retroactivesampling/internal/coordinator"
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
			f.notified = append(f.notified, n.TraceId)
			f.mu.Unlock()
		}
	}
}

func (f *fakeServer) broadcast(traceID string) {
	msg := &gen.CoordinatorMessage{
		Payload: &gen.CoordinatorMessage_Decision{
			Decision: &gen.TraceDecision{TraceId: traceID, Keep: true},
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

func TestClientSendsNotification(t *testing.T) {
	srv, addr := startFakeServer(t)
	received := make(chan string, 1)
	c := coord.New(addr, func(traceID string, keep bool) { received <- traceID })
	t.Cleanup(c.Close)

	time.Sleep(100 * time.Millisecond) // connection establishment
	c.Notify("trace-99")
	time.Sleep(100 * time.Millisecond)

	srv.mu.Lock()
	notified := srv.notified
	srv.mu.Unlock()
	require.Contains(t, notified, "trace-99")
}

func TestClientReceivesDecision(t *testing.T) {
	srv, addr := startFakeServer(t)
	received := make(chan string, 1)
	c := coord.New(addr, func(traceID string, keep bool) {
		if keep {
			received <- traceID
		}
	})
	t.Cleanup(c.Close)

	time.Sleep(100 * time.Millisecond)
	srv.broadcast("trace-55")

	select {
	case id := <-received:
		assert.Equal(t, "trace-55", id)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for decision")
	}
}
