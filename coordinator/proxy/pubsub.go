package proxy

import (
	"context"
	"fmt"
	"log/slog"
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
}

func New(endpoint string, handler func([]byte)) (*PubSub, error) {
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &PubSub{
		conn:    conn,
		sendCh:  make(chan []byte, sendBufSize),
		handler: handler,
		ctx:     ctx,
		cancel:  cancel,
	}
	go p.run()
	return p, nil
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
		connCtx, connCancel := context.WithCancel(p.ctx)
		client, err := gen.NewCoordinatorClient(p.conn).Connect(connCtx)
		if err != nil {
			connCancel()
			slog.Warn("proxy: connect failed, retrying", "backoff", backoff, "err", err)
			select {
			case <-p.ctx.Done():
				return
			case <-time.After(backoff):
				backoff = min(backoff*2, maxBackoff)
			}
			continue
		}
		go p.recv(client, connCancel)
		if err := p.send(connCtx, client); err != nil {
			connCancel()
			slog.Warn("proxy: connection lost, retrying", "backoff", backoff, "err", err)
			select {
			case <-p.ctx.Done():
				return
			case <-time.After(backoff):
				backoff = min(backoff*2, maxBackoff)
			}
		} else {
			connCancel()
			backoff = time.Second
		}
	}
}

func (p *PubSub) recv(client gen.Coordinator_ConnectClient, cancel context.CancelFunc) {
	for {
		msg, err := client.Recv()
		if err != nil {
			cancel()
			return
		}
		if b := msg.GetBatch(); b != nil {
			for _, traceID := range b.TraceIds {
				p.handler(traceID)
			}
		}
	}
}

func (p *PubSub) send(ctx context.Context, client gen.Coordinator_ConnectClient) error {
	for {
		select {
		case traceID := <-p.sendCh:
			if err := client.Send(&gen.ProcessorMessage{
				Payload: &gen.ProcessorMessage_Notify{
					Notify: &gen.NotifyInteresting{TraceId: traceID},
				},
			}); err != nil {
				return err
			}
		case <-ctx.Done():
			if p.ctx.Err() != nil {
				return nil
			}
			return ctx.Err()
		}
	}
}
