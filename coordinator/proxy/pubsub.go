package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	gen "pitr.ca/retroactivesampling/proto"
)

const (
	sendBufSize = 256
	maxBackoff  = 30 * time.Second
)

type PubSub struct {
	conn    *grpc.ClientConn
	sendCh  chan []byte
	handler func([]byte)
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
}

func New(endpoint string, handler func([]byte)) *PubSub {
	ctx, cancel := context.WithCancel(context.Background())
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		slog.Error("proxy: new client", "endpoint", endpoint, "err", err)
		os.Exit(1)
	}
	p := &PubSub{
		conn:    conn,
		sendCh:  make(chan []byte, sendBufSize),
		handler: handler,
		ctx:     ctx,
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	go func() { defer close(p.done); p.run() }()
	return p
}

// Always returns (false, nil) — no local dedup; novel count is tracked by the parent coordinator.
func (p *PubSub) Publish(_ context.Context, traceID []byte) (bool, error) {
	select {
	case p.sendCh <- traceID:
	default:
		slog.Warn("proxy: send buffer full, dropping notify", "trace_id", fmt.Sprintf("%x", traceID))
	}
	return false, nil
}

func (p *PubSub) Close() error {
	p.cancel()
	<-p.done
	return p.conn.Close()
}

func (p *PubSub) run() {
	backoff := time.Second
	for {
		select {
		case <-p.ctx.Done():
			return
		default:
		}
		stream, err := gen.NewCoordinatorClient(p.conn).Connect(p.ctx)
		if err != nil {
			slog.Warn("proxy: connect failed, retrying", "backoff", backoff, "err", err)
			select {
			case <-p.ctx.Done():
				return
			case <-time.After(backoff):
				backoff = min(backoff*2, maxBackoff)
			}
			continue
		}
		recvErr := make(chan error, 1)
		go p.recv(stream, recvErr)
		if err := p.send(stream, recvErr); err != nil {
			slog.Warn("proxy: connection lost, retrying", "backoff", backoff, "err", err)
			select {
			case <-p.ctx.Done():
				return
			case <-time.After(backoff):
				backoff = min(backoff*2, maxBackoff)
			}
		} else {
			backoff = time.Second
		}
	}
}

func (p *PubSub) recv(stream gen.Coordinator_ConnectClient, errCh chan<- error) {
	for {
		msg, err := stream.Recv()
		if err != nil {
			errCh <- err
			return
		}
		if b := msg.GetBatch(); b != nil && p.handler != nil {
			for _, traceID := range b.TraceIds {
				p.handler(traceID)
			}
		}
	}
}

func (p *PubSub) send(stream gen.Coordinator_ConnectClient, recvErr <-chan error) error {
	for {
		select {
		case traceID := <-p.sendCh:
			if err := stream.Send(&gen.ProcessorMessage{
				Payload: &gen.ProcessorMessage_Notify{
					Notify: &gen.NotifyInteresting{TraceId: traceID},
				},
			}); err != nil {
				return err
			}
		case err := <-recvErr:
			return err
		case <-p.ctx.Done():
			return nil
		}
	}
}
