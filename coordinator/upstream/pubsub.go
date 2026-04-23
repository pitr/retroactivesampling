package upstream

import (
	"context"
	"encoding/hex"
	"log"
	"sync"
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
	conn     *grpc.ClientConn
	sendCh   chan string
	mu       sync.RWMutex
	handlers []func(string)
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
}

func New(endpoint string) *PubSub {
	ctx, cancel := context.WithCancel(context.Background())
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("upstream: new client %s: %v", endpoint, err)
	}
	p := &PubSub{
		conn:   conn,
		sendCh: make(chan string, sendBufSize),
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	go func() { defer close(p.done); p.run() }()
	return p
}

// Always returns (false, nil) — no local dedup; novel count is tracked by the upstream coordinator.
func (p *PubSub) Publish(_ context.Context, traceID string) (bool, error) {
	select {
	case p.sendCh <- traceID:
	default:
		log.Printf("upstream: send buffer full, dropping notify for trace %s", traceID)
	}
	return false, nil
}

// Call before meaningful upstream decisions are expected to avoid a startup race.
func (p *PubSub) Subscribe(ctx context.Context, handler func(string)) error {
	p.mu.Lock()
	p.handlers = append(p.handlers, handler)
	p.mu.Unlock()
	<-ctx.Done()
	return nil
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
			log.Printf("upstream: connect failed, retrying in %s: %v", backoff, err)
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
			log.Printf("upstream: connection lost, retrying in %s: %v", backoff, err)
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
		if b := msg.GetBatch(); b != nil {
			p.mu.RLock()
			hs := p.handlers
			p.mu.RUnlock()
			for _, raw := range b.TraceIds {
				tid := hex.EncodeToString(raw)
				for _, h := range hs {
					h(tid)
				}
			}
		}
	}
}

func (p *PubSub) send(stream gen.Coordinator_ConnectClient, recvErr <-chan error) error {
	for {
		select {
		case traceID := <-p.sendCh:
			tid, _ := hex.DecodeString(traceID)
			if err := stream.Send(&gen.ProcessorMessage{
				Payload: &gen.ProcessorMessage_Notify{
					Notify: &gen.NotifyInteresting{TraceId: tid},
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
