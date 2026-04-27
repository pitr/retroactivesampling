package server_test

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"pitr.ca/retroactivesampling/coordinator/internal/testtls"
	"pitr.ca/retroactivesampling/coordinator/server"
	gen "pitr.ca/retroactivesampling/proto"
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
	notified := make(chan []byte, 1)
	srv := server.New(func(traceID []byte) { notified <- traceID }, nil, nil, nil, nil)
	addr := startServer(t, srv)

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	stream, err := gen.NewCoordinatorClient(conn).Connect(context.Background())
	require.NoError(t, err)

	traceBytes, _ := hex.DecodeString("aabbccdd11111111aabbccdd11111111")

	// Processor sends notification
	err = stream.Send(&gen.ProcessorMessage{
		Payload: &gen.ProcessorMessage_Notify{
			Notify: &gen.NotifyInteresting{TraceId: traceBytes},
		},
	})
	require.NoError(t, err)

	select {
	case id := <-notified:
		assert.Equal(t, traceBytes, id)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for notification")
	}

	// notified channel receipt proves the stream is registered — Broadcast will reach it.
	srv.Broadcast(traceBytes)

	msg, err := stream.Recv()
	require.NoError(t, err)
	b := msg.GetBatch()
	require.NotNil(t, b)
	require.Len(t, b.TraceIds, 1)
	assert.Equal(t, traceBytes, b.TraceIds[0])
}

func TestBroadcastBatching(t *testing.T) {
	notified := make(chan []byte, 1)
	srv := server.New(func(id []byte) { notified <- id }, nil, nil, nil, nil)
	addr := startServer(t, srv)

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := gen.NewCoordinatorClient(conn).Connect(ctx)
	require.NoError(t, err)

	// Wait for stream to be registered before broadcasting.
	marker := make([]byte, 16)
	_, _ = rand.Read(marker)
	err = stream.Send(&gen.ProcessorMessage{
		Payload: &gen.ProcessorMessage_Notify{
			Notify: &gen.NotifyInteresting{TraceId: marker},
		},
	})
	require.NoError(t, err)
	select {
	case <-notified:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for stream registration")
	}

	const N = 50
	sent := make([][]byte, N)
	for i := range sent {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		sent[i] = b
		srv.Broadcast(b)
	}

	var received [][]byte
	var batchCount int
	for len(received) < N {
		msg, err := stream.Recv()
		require.NoError(t, err)
		b := msg.GetBatch()
		require.NotNil(t, b)
		received = append(received, b.TraceIds...)
		batchCount++
	}

	assert.ElementsMatch(t, sent, received)
	assert.Less(t, batchCount, N, "expected batching to reduce message count")
}

func TestServerStartWithTLS(t *testing.T) {
	files := testtls.Generate(t, []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})

	// Pick a port up front; Start blocks.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := lis.Addr().String()
	require.NoError(t, lis.Close())

	tlsCfg := &server.TLSConfig{CertFile: files.Cert, KeyFile: files.Key}
	creds, err := tlsCfg.Credentials()
	require.NoError(t, err)

	notified := make(chan []byte, 1)
	srv := server.New(func(id []byte) { notified <- id }, nil, nil, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.Start(ctx, addr, time.Second, creds) }()

	// Plain client must fail
	t.Run("plain client rejected", func(t *testing.T) {
		conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		callCtx, callCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer callCancel()
		stream, err := gen.NewCoordinatorClient(conn).Connect(callCtx)
		if err == nil {
			_, err = stream.Recv()
		}
		require.Error(t, err)
	})

	// TLS client must succeed
	t.Run("tls client accepted", func(t *testing.T) {
		caPEM, err := os.ReadFile(files.CACert)
		require.NoError(t, err)
		pool := x509.NewCertPool()
		require.True(t, pool.AppendCertsFromPEM(caPEM))
		clientCreds := credentials.NewTLS(&tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS12})
		conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(clientCreds))
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		stream, err := gen.NewCoordinatorClient(conn).Connect(context.Background())
		require.NoError(t, err)
		require.NoError(t, stream.Send(&gen.ProcessorMessage{
			Payload: &gen.ProcessorMessage_Notify{
				Notify: &gen.NotifyInteresting{TraceId: make([]byte, 16)},
			},
		}))
		select {
		case <-notified:
		case <-time.After(2 * time.Second):
			t.Fatal("notify not received")
		}
	})

	cancel()
	select {
	case <-srvErr:
	case <-time.After(3 * time.Second):
		t.Fatal("server did not shut down")
	}
}
