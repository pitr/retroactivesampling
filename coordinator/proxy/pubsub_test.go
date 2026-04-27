package proxy_test

import (
	"encoding/hex"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"pitr.ca/retroactivesampling/coordinator/proxy"
	gen "pitr.ca/retroactivesampling/proto"
)

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

func startGRPCServer(t *testing.T, srv gen.CoordinatorServer) string {
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

var traceBytes, _ = hex.DecodeString(traceHex)

func TestPublishSendsNotifyInteresting(t *testing.T) {
	mock := newMock()
	addr := startGRPCServer(t, mock)

	ps, err := proxy.New(proxy.ClientConfig{Endpoint: addr}, func([]byte) {})
	require.NoError(t, err)
	t.Cleanup(func() { _ = ps.Close() })

	novel, err := ps.Publish(t.Context(), traceBytes)
	require.NoError(t, err)
	assert.False(t, novel)

	select {
	case id := <-mock.notified:
		assert.Equal(t, traceHex, id)
	case <-time.After(3 * time.Second):
		t.Fatal("timeout: NotifyInteresting not received by parent coordinator")
	}
}

func TestPublishAlwaysReturnsFalse(t *testing.T) {
	mock := newMock()
	addr := startGRPCServer(t, mock)

	ps, err := proxy.New(proxy.ClientConfig{Endpoint: addr}, func([]byte) {})
	require.NoError(t, err)
	t.Cleanup(func() { _ = ps.Close() })

	for range 3 {
		novel, err := ps.Publish(t.Context(), traceBytes)
		require.NoError(t, err)
		assert.False(t, novel, "proxy.PubSub has no local dedup — novel count tracked by parent coordinator")
	}
}

func TestHandlerFiredOnDecision(t *testing.T) {
	mock := newMock()
	addr := startGRPCServer(t, mock)

	received := make(chan []byte, 1)
	ps, err := proxy.New(proxy.ClientConfig{Endpoint: addr}, func(id []byte) { received <- id })
	require.NoError(t, err)
	t.Cleanup(func() { _ = ps.Close() })

	select {
	case <-mock.connected:
	case <-time.After(3 * time.Second):
		t.Fatal("proxy connection not established")
	}

	mock.decisionCh <- traceBytes

	select {
	case id := <-received:
		assert.Equal(t, traceBytes, id)
	case <-time.After(3 * time.Second):
		t.Fatal("timeout: handler not called with decision")
	}
}

func TestCloseStopsReconnectLoop(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := lis.Addr().String()
	_ = lis.Close()

	ps, err := proxy.New(proxy.ClientConfig{Endpoint: addr}, func([]byte) {})
	require.NoError(t, err)
	done := make(chan struct{})
	go func() { _ = ps.Close(); close(done) }()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close() did not return in time")
	}
}
