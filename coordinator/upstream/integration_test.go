package upstream_test

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

	"pitr.ca/retroactivesampling/coordinator/memory"
	"pitr.ca/retroactivesampling/coordinator/server"
	"pitr.ca/retroactivesampling/coordinator/upstream"
	gen "pitr.ca/retroactivesampling/proto"
)

func startCoordServer(t *testing.T, srv *server.Server) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	gs := grpc.NewServer()
	gen.RegisterCoordinatorServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	return lis.Addr().String()
}

func TestIntegrationLocalToCentral(t *testing.T) {
	// Central coordinator: single-node with memory PubSub.
	memPS := memory.New(time.Minute)
	centralSrv := server.New(func(traceID string) {
		_, _ = memPS.Publish(t.Context(), traceID)
	}, nil, nil, nil, nil)
	go func() {
		_ = memPS.Subscribe(t.Context(), func(id string) { centralSrv.Broadcast(id) })
	}()
	centralAddr := startCoordServer(t, centralSrv)

	// Local coordinator: upstream PubSub pointing at central.
	localPS := upstream.New(centralAddr)
	t.Cleanup(func() { _ = localPS.Close() })

	localSrv := server.New(func(traceID string) {
		_, _ = localPS.Publish(t.Context(), traceID)
	}, nil, nil, nil, nil)
	// Subscribe must be registered before any decisions can arrive from upstream.
	go func() {
		_ = localPS.Subscribe(t.Context(), func(id string) { localSrv.Broadcast(id) })
	}()
	time.Sleep(200 * time.Millisecond) // let upstream connection establish and handler register
	localAddr := startCoordServer(t, localSrv)

	// Collector client: connects to local coordinator.
	conn, err := grpc.NewClient(localAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	stream, err := gen.NewCoordinatorClient(conn).Connect(ctx)
	require.NoError(t, err)

	const traceHex = "aabbccdd11223344aabbccdd11223344"
	traceBytes, _ := hex.DecodeString(traceHex)

	// Send NotifyInteresting from collector to local coordinator.
	err = stream.Send(&gen.ProcessorMessage{
		Payload: &gen.ProcessorMessage_Notify{
			Notify: &gen.NotifyInteresting{TraceId: traceBytes},
		},
	})
	require.NoError(t, err)

	// Expect BatchTraceDecision to arrive via: local→central→central-mem→central.Broadcast
	// →local-upstream-client→local.Broadcast→collector.
	msg, err := stream.Recv()
	require.NoError(t, err)
	b := msg.GetBatch()
	require.NotNil(t, b)
	require.Len(t, b.TraceIds, 1)
	assert.Equal(t, traceBytes, b.TraceIds[0])
}
