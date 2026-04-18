package coordinator

import (
	"context"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	gen "retroactivesampling/proto"
)

type DecisionHandler func(traceID string, keep bool)

type Client struct {
	endpoint string
	handler  DecisionHandler
	sendCh   chan string
	ctx      context.Context
	cancel   context.CancelFunc
}

func New(endpoint string, handler DecisionHandler) *Client {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		endpoint: endpoint,
		handler:  handler,
		sendCh:   make(chan string, 256),
		ctx:      ctx,
		cancel:   cancel,
	}
	go c.run()
	return c
}

func (c *Client) Notify(traceID string) {
	select {
	case c.sendCh <- traceID:
	default: // drop if backpressure
	}
}

func (c *Client) Close() { c.cancel() }

func (c *Client) run() {
	backoff := time.Second
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}
		if err := c.connect(); err != nil {
			select {
			case <-c.ctx.Done():
				return
			case <-time.After(backoff):
				if backoff < 30*time.Second {
					backoff *= 2
				}
			}
		} else {
			backoff = time.Second
		}
	}
}

func (c *Client) connect() error {
	conn, err := grpc.NewClient(c.endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()

	stream, err := gen.NewCoordinatorClient(conn).Connect(c.ctx)
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
			if d := msg.GetDecision(); d != nil {
				c.handler(d.TraceId, d.Keep)
			}
		}
	}()

	for {
		select {
		case traceID := <-c.sendCh:
			if err := stream.Send(&gen.ProcessorMessage{
				Payload: &gen.ProcessorMessage_Notify{
					Notify: &gen.NotifyInteresting{TraceId: traceID},
				},
			}); err != nil {
				return err
			}
		case err := <-recvErr:
			return err
		case <-c.ctx.Done():
			return nil
		}
	}
}
