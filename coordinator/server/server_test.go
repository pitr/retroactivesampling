package server_test

import (
	"context"
	"encoding/hex"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	gen "pitr.ca/retroactivesampling/proto"
	"pitr.ca/retroactivesampling/coordinator/server"
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
	srv := server.New(func(traceID string) { notified <- traceID }, nil, nil)
	addr := startServer(t, srv)

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	stream, err := gen.NewCoordinatorClient(conn).Connect(context.Background())
	require.NoError(t, err)

	const traceHex = "aabbccdd11111111aabbccdd11111111"
	traceBytes, _ := hex.DecodeString(traceHex)

	// Processor sends notification
	err = stream.Send(&gen.ProcessorMessage{
		Payload: &gen.ProcessorMessage_Notify{
			Notify: &gen.NotifyInteresting{TraceId: traceBytes},
		},
	})
	require.NoError(t, err)

	select {
	case id := <-notified:
		assert.Equal(t, traceHex, id)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for notification")
	}

	// notified channel receipt proves the stream is registered — Broadcast will reach it.
	srv.Broadcast(traceHex)

	msg, err := stream.Recv()
	require.NoError(t, err)
	d := msg.GetDecision()
	require.NotNil(t, d)
	assert.Equal(t, traceBytes, d.TraceId)
}
