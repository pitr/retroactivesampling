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
		Payload: &gen.CoordinatorMessage_Batch{
			Batch: &gen.BatchTraceDecision{TraceIds: [][]byte{tid}},
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
		TLS: configtls.ClientConfig{Insecure: true},
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
