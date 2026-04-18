package server_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	gen "retroactivesampling/proto"
	"retroactivesampling/coordinator/server"
)

func startServer(t *testing.T, s *server.Server) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	gs := grpc.NewServer()
	gen.RegisterCoordinatorServer(gs, s)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	return lis.Addr().String()
}

func TestBroadcastToConnectedProcessors(t *testing.T) {
	notified := make(chan string, 1)
	srv := server.New(func(traceID string) { notified <- traceID })
	addr := startServer(t, srv)

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	stream, err := gen.NewCoordinatorClient(conn).Connect(context.Background())
	require.NoError(t, err)

	// Processor sends notification
	err = stream.Send(&gen.ProcessorMessage{
		Payload: &gen.ProcessorMessage_Notify{
			Notify: &gen.NotifyInteresting{TraceId: "trace-1"},
		},
	})
	require.NoError(t, err)

	select {
	case id := <-notified:
		assert.Equal(t, "trace-1", id)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for notification")
	}

	// notified channel receipt proves the stream is registered — Broadcast will reach it.
	srv.Broadcast("trace-1", true)

	msg, err := stream.Recv()
	require.NoError(t, err)
	d := msg.GetDecision()
	require.NotNil(t, d)
	assert.Equal(t, "trace-1", d.TraceId)
	assert.True(t, d.Keep)
}
