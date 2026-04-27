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
	"google.golang.org/grpc/metadata"

	"pitr.ca/retroactivesampling/coordinator/internal/testtls"
	"pitr.ca/retroactivesampling/coordinator/proxy"
	"pitr.ca/retroactivesampling/coordinator/server"
	gen "pitr.ca/retroactivesampling/proto"
)

type mockServer struct {
	gen.UnimplementedCoordinatorServer
	notified   chan string
	decisionCh chan []byte
	connected  chan struct{}
	once       sync.Once
	gotMD      chan metadata.MD
}

func (m *mockServer) Connect(stream gen.Coordinator_ConnectServer) error {
	if m.gotMD != nil {
		if md, ok := metadata.FromIncomingContext(stream.Context()); ok {
			select {
			case m.gotMD <- md:
			default:
			}
		}
	}
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
		gotMD:      make(chan metadata.MD, 1),
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
