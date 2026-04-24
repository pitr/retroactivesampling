package proxy_test

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"pitr.ca/retroactivesampling/coordinator/memory"
	"pitr.ca/retroactivesampling/coordinator/proxy"
	"pitr.ca/retroactivesampling/coordinator/server"
	gen "pitr.ca/retroactivesampling/proto"
)

func TestIntegrationLocalToCentral(t *testing.T) {
	// Central coordinator: single-node with memory PubSub.
	memPS := memory.New(time.Minute)
	centralSrv := server.New(func(traceID []byte) {
		_, _ = memPS.Publish(t.Context(), traceID)
	}, nil, nil, nil, nil)
	go func() {
		_ = memPS.Subscribe(t.Context(), func(id []byte) { centralSrv.Broadcast(id) })
	}()
	centralAddr := startGRPCServer(t, centralSrv)

	// Local coordinator: proxy PubSub pointing at central.
	localPS := proxy.New(centralAddr)
	t.Cleanup(func() { _ = localPS.Close() })

	localSrv := server.New(func(traceID []byte) {
		_, _ = localPS.Publish(t.Context(), traceID)
	}, nil, nil, nil, nil)
	// Subscribe before waiting for connection — handler is registered synchronously inside
	// Subscribe before it blocks, so by the time the gRPC stream is established the handler
	// is guaranteed to be in place.
	// Handler is registered synchronously inside Subscribe before it blocks,
	// so it is in place before localSrv starts and any collector can connect.
	go func() {
		_ = localPS.Subscribe(t.Context(), func(id []byte) { localSrv.Broadcast(id) })
	}()
	localAddr := startGRPCServer(t, localSrv)

	// Collector client: connects to local coordinator.
	conn, err := grpc.NewClient(localAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	stream, err := gen.NewCoordinatorClient(conn).Connect(ctx)
	require.NoError(t, err)

	traceBytes, _ := hex.DecodeString("aabbccdd11223344aabbccdd11223344")

	// Send NotifyInteresting from collector to local coordinator.
	err = stream.Send(&gen.ProcessorMessage{
		Payload: &gen.ProcessorMessage_Notify{
			Notify: &gen.NotifyInteresting{TraceId: traceBytes},
		},
	})
	require.NoError(t, err)

	// Expect BatchTraceDecision to arrive via: local→central→central-mem→central.Broadcast
	// →local-proxy-client→local.Broadcast→collector.
	msg, err := stream.Recv()
	require.NoError(t, err)
	b := msg.GetBatch()
	require.NotNil(t, b)
	require.Len(t, b.TraceIds, 1)
	assert.Equal(t, traceBytes, b.TraceIds[0])
}
