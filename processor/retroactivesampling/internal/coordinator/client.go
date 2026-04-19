package coordinator

import (
	"context"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	gen "pitr.ca/retroactivesampling/proto"
)

type DecisionHandler func(traceID string, keep bool)

type Client struct {
	endpoint string
	handler  DecisionHandler
	sendCh   chan string
	ctx      context.Context
	cancel   context.CancelFunc
	logger   *zap.Logger
}

func New(endpoint string, handler DecisionHandler, logger *zap.Logger) *Client {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		endpoint: endpoint,
		handler:  handler,
		sendCh:   make(chan string, 256),
		ctx:      ctx,
		cancel:   cancel,
		logger:   logger,
	}
	go c.run()
	return c
}

func (c *Client) Notify(traceID string) {
	select {
	case c.sendCh <- traceID:
		c.logger.Debug("coordinator: queued notify", zap.String("trace_id", traceID), zap.Int("queue_len", len(c.sendCh)))
	default:
		c.logger.Warn("coordinator: send queue full, dropping notify", zap.String("trace_id", traceID))
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
		c.logger.Info("coordinator: connecting", zap.String("endpoint", c.endpoint))
		if err := c.connect(); err != nil {
			c.logger.Warn("coordinator: connection lost, retrying", zap.Error(err), zap.Duration("backoff", backoff))
			select {
			case <-c.ctx.Done():
				return
			case <-time.After(backoff):
				if backoff < 30*time.Second {
					backoff *= 2
				}
			}
		} else {
			c.logger.Info("coordinator: connection closed cleanly")
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
	c.logger.Info("coordinator: stream established", zap.String("endpoint", c.endpoint))

	recvErr := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			if d := msg.GetDecision(); d != nil {
				c.logger.Debug("coordinator: received decision", zap.String("trace_id", d.TraceId), zap.Bool("keep", d.Keep))
				c.handler(d.TraceId, d.Keep)
			}
		}
	}()

	for {
		select {
		case traceID := <-c.sendCh:
			c.logger.Debug("coordinator: sending notify", zap.String("trace_id", traceID))
			if err := stream.Send(&gen.ProcessorMessage{
				Payload: &gen.ProcessorMessage_Notify{
					Notify: &gen.NotifyInteresting{TraceId: traceID},
				},
			}); err != nil {
				c.logger.Error("coordinator: send failed", zap.String("trace_id", traceID), zap.Error(err))
				return err
			}
		case err := <-recvErr:
			c.logger.Error("coordinator: recv failed", zap.Error(err))
			return err
		case <-c.ctx.Done():
			return nil
		}
	}
}
