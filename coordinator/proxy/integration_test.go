package proxy_test

import (
	"encoding/hex"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"pitr.ca/retroactivesampling/coordinator/internal/testtls"
	"pitr.ca/retroactivesampling/coordinator/memory"
	"pitr.ca/retroactivesampling/coordinator/proxy"
	"pitr.ca/retroactivesampling/coordinator/server"
	gen "pitr.ca/retroactivesampling/proto"
)

func TestIntegrationLocalToCentral(t *testing.T) {
	// Central coordinator: memory PubSub.
	var centralSrv *server.Server
	memPS := memory.New(time.Minute, func(id []byte) { centralSrv.Broadcast(id) })
	centralSrv = server.New(func(traceID []byte) {
		_, _ = memPS.Publish(t.Context(), traceID)
	}, nil, nil, nil, nil)
	centralAddr := startGRPCServer(t, centralSrv)

	// Local coordinator: proxy PubSub pointing at central.
	var localSrv *server.Server
	localPS, err := proxy.New(proxy.ClientConfig{Endpoint: centralAddr}, func(id []byte) { localSrv.Broadcast(id) })
	require.NoError(t, err)
	t.Cleanup(func() { _ = localPS.Close() })
	localSrv = server.New(func(traceID []byte) {
		_, _ = localPS.Publish(t.Context(), traceID)
	}, nil, nil, nil, nil)
	localAddr := startGRPCServer(t, localSrv)

	// Collector client: connects to local coordinator.
	conn, err := grpc.NewClient(localAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	stream, err := gen.NewCoordinatorClient(conn).Connect(t.Context())
	require.NoError(t, err)

	traceBytes, _ := hex.DecodeString("aabbccdd11223344aabbccdd11223344")

	err = stream.Send(&gen.ProcessorMessage{
		Payload: &gen.ProcessorMessage_Notify{
			Notify: &gen.NotifyInteresting{TraceId: traceBytes},
		},
	})
	require.NoError(t, err)

	msg, err := stream.Recv()
	require.NoError(t, err)
	b := msg.GetBatch()
	require.NotNil(t, b)
	require.Len(t, b.TraceIds, 1)
	assert.Equal(t, traceBytes, b.TraceIds[0])
}

func TestIntegrationLocalToCentralTLS(t *testing.T) {
	files := testtls.Generate(t, []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})

	// Central coordinator: TLS gRPC server with mock notify-handler -> memory PubSub.
	var centralSrv *server.Server
	memPS := memory.New(time.Minute, func(id []byte) { centralSrv.Broadcast(id) })
	centralSrv = server.New(func(traceID []byte) {
		_, _ = memPS.Publish(t.Context(), traceID)
	}, nil, nil, nil, nil)

	tlsCfg := &server.TLSConfig{CertFile: files.Cert, KeyFile: files.Key, ClientCAFile: files.CACert}
	creds, err := tlsCfg.Credentials()
	require.NoError(t, err)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	gs := grpc.NewServer(grpc.Creds(creds))
	gen.RegisterCoordinatorServer(gs, centralSrv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	centralAddr := lis.Addr().String()

	// Local coordinator: proxy PubSub points at central via mTLS.
	var localSrv *server.Server
	localPS, err := proxy.New(proxy.ClientConfig{
		Endpoint: centralAddr,
		TLS: &proxy.TLSConfig{
			CAFile:             files.CACert,
			CertFile:           files.Cert,
			KeyFile:            files.Key,
			ServerNameOverride: "localhost",
		},
	}, func(id []byte) { localSrv.Broadcast(id) })
	require.NoError(t, err)
	t.Cleanup(func() { _ = localPS.Close() })
	localSrv = server.New(func(traceID []byte) {
		_, _ = localPS.Publish(t.Context(), traceID)
	}, nil, nil, nil, nil)
	localAddr := startGRPCServer(t, localSrv)

	// Plain client connects to the local coordinator (still insecure on that hop).
	conn, err := grpc.NewClient(localAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	stream, err := gen.NewCoordinatorClient(conn).Connect(t.Context())
	require.NoError(t, err)

	traceBytes, _ := hex.DecodeString("aabbccdd11223344aabbccdd11223344")
	require.NoError(t, stream.Send(&gen.ProcessorMessage{
		Payload: &gen.ProcessorMessage_Notify{
			Notify: &gen.NotifyInteresting{TraceId: traceBytes},
		},
	}))

	msg, err := stream.Recv()
	require.NoError(t, err)
	b := msg.GetBatch()
	require.NotNil(t, b)
	require.Len(t, b.TraceIds, 1)
	assert.Equal(t, traceBytes, b.TraceIds[0])
}
