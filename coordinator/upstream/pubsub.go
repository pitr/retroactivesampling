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

var _ interface {
	Publish(context.Context, string) (bool, error)
	Subscribe(context.Context, func(string)) error
	Close() error
} = (*PubSub)(nil)

const sendBufSize = 256

type PubSub struct {
	endpoint string
	sendCh   chan string
	mu       sync.RWMutex
	handlers []func(string)
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
}

func New(endpoint string) *PubSub {
	ctx, cancel := context.WithCancel(context.Background())
	p := &PubSub{
		endpoint: endpoint,
		sendCh:   make(chan string, sendBufSize),
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	go func() { defer close(p.done); p.run() }()
	return p
}

// Publish enqueues traceID to be forwarded upstream. Always returns (true, nil) — no local dedup.
func (p *PubSub) Publish(_ context.Context, traceID string) (bool, error) {
	select {
	case p.sendCh <- traceID:
	default: // best-effort: drop if buffer full during reconnect
	}
	return true, nil
}

// Subscribe registers handler and blocks until ctx is cancelled.
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
	return nil
}

func (p *PubSub) run() {
	backoff := time.Second
	for {
		select {
		case <-p.ctx.Done():
			return
		default:
		}
		if err := p.connect(); err != nil {
			log.Printf("upstream: connection lost, retrying in %s: %v", backoff, err)
			select {
			case <-p.ctx.Done():
				return
			case <-time.After(backoff):
				backoff = min(backoff*2, 30*time.Second)
			}
		} else {
			backoff = time.Second
		}
	}
}

func (p *PubSub) connect() error {
	conn, err := grpc.NewClient(p.endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	stream, err := gen.NewCoordinatorClient(conn).Connect(p.ctx)
	if err != nil {
		return err
	}

	recvErr := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				recvErr <- err
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
	}()

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
