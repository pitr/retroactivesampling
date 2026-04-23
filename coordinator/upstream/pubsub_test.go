package upstream_test

import (
	"context"
	"encoding/hex"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	gen "pitr.ca/retroactivesampling/proto"
	"pitr.ca/retroactivesampling/coordinator/upstream"
)

// mockServer implements gen.CoordinatorServer. notified receives traceID hex
// strings for each NotifyInteresting received. decisionCh items are sent as
// BatchTraceDecision to the connected client. connected is closed once.
type mockServer struct {
	gen.UnimplementedCoordinatorServer
	notified   chan string
	decisionCh chan []byte
	connected  chan struct{}
	once       sync.Once
}

func (m *mockServer) Connect(stream gen.Coordinator_ConnectServer) error {
	m.once.Do(func() { close(m.connected) })
	go func() {
		for {
			select {
			case tid, ok := <-m.decisionCh:
				if !ok {
					return
				}
				_ = stream.Send(&gen.CoordinatorMessage{
					Payload: &gen.CoordinatorMessage_Batch{
						Batch: &gen.BatchTraceDecision{TraceIds: [][]byte{tid}},
					},
				})
			case <-stream.Context().Done():
				return
			}
		}
	}()
	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		if n := msg.GetNotify(); n != nil {
			m.notified <- hex.EncodeToString(n.TraceId)
		}
	}
}

func newMock() *mockServer {
	return &mockServer{
		notified:   make(chan string, 10),
		decisionCh: make(chan []byte, 10),
		connected:  make(chan struct{}),
	}
}

func startMockServer(t *testing.T, srv *mockServer) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	gs := grpc.NewServer()
	gen.RegisterCoordinatorServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	return lis.Addr().String()
}

const traceHex = "aabbccdd11223344aabbccdd11223344"

func TestPublishSendsNotifyInteresting(t *testing.T) {
	mock := newMock()
	addr := startMockServer(t, mock)

	ps := upstream.New(addr)
	t.Cleanup(func() { _ = ps.Close() })

	novel, err := ps.Publish(t.Context(), traceHex)
	require.NoError(t, err)
	assert.True(t, novel)

	select {
	case id := <-mock.notified:
		assert.Equal(t, traceHex, id)
	case <-time.After(3 * time.Second):
		t.Fatal("timeout: NotifyInteresting not received by upstream")
	}
}

func TestPublishAlwaysReturnsTrue(t *testing.T) {
	mock := newMock()
	addr := startMockServer(t, mock)

	ps := upstream.New(addr)
	t.Cleanup(func() { _ = ps.Close() })

	for range 3 {
		novel, err := ps.Publish(t.Context(), traceHex)
		require.NoError(t, err)
		assert.True(t, novel, "upstream.PubSub has no local dedup — must always return true")
	}
}

func TestSubscribeHandlerFiredOnDecision(t *testing.T) {
	mock := newMock()
	addr := startMockServer(t, mock)

	ps := upstream.New(addr)
	t.Cleanup(func() { _ = ps.Close() })

	received := make(chan string, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// Subscribe before connection is established to guarantee handler is registered first.
	go func() { _ = ps.Subscribe(ctx, func(id string) { received <- id }) }()
	time.Sleep(50 * time.Millisecond) // let goroutine run and register handler

	// Wait for connection before sending decision.
	select {
	case <-mock.connected:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream connection not established")
	}

	traceBytes, _ := hex.DecodeString(traceHex)
	mock.decisionCh <- traceBytes

	select {
	case id := <-received:
		assert.Equal(t, traceHex, id)
	case <-time.After(3 * time.Second):
		t.Fatal("timeout: Subscribe handler not called with decision")
	}
}

func TestCloseStopsReconnectLoop(t *testing.T) {
	// No server listening — PubSub will loop retrying. Close() must still return quickly.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := lis.Addr().String()
	lis.Close() // immediately release so nothing is listening

	ps := upstream.New(addr)
	done := make(chan struct{})
	go func() { _ = ps.Close(); close(done) }()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close() did not return in time")
	}
}
