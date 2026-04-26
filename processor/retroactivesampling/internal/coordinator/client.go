package coordinator

import (
	"context"
	"encoding/hex"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configgrpc"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.uber.org/zap"

	gen "pitr.ca/retroactivesampling/proto"
)

type DecisionHandler func(traceID string)

type Client struct {
	grpcCfg  configgrpc.ClientConfig
	host     component.Host
	settings component.TelemetrySettings
	handler  DecisionHandler
	sendCh   chan pcommon.TraceID
	ctx      context.Context
	cancel   context.CancelFunc
	logger   *zap.Logger
	done     chan struct{}
}

func New(grpcCfg configgrpc.ClientConfig, host component.Host, settings component.TelemetrySettings, handler DecisionHandler, logger *zap.Logger) *Client {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		grpcCfg:  grpcCfg,
		host:     host,
		settings: settings,
		handler:  handler,
		sendCh:   make(chan pcommon.TraceID, 256),
		ctx:      ctx,
		cancel:   cancel,
		logger:   logger,
		done:     make(chan struct{}),
	}
	go func() { defer close(c.done); c.run() }()
	return c
}

func (c *Client) Notify(traceID pcommon.TraceID) {
	select {
	case c.sendCh <- traceID:
		c.logger.Debug("coordinator: queued notify", zap.Stringer("trace_id", traceID), zap.Int("queue_len", len(c.sendCh)))
	default:
		c.logger.Warn("coordinator: send queue full, dropping notify", zap.Stringer("trace_id", traceID))
	}
}

func (c *Client) Close() { c.cancel(); <-c.done }

func (c *Client) run() {
	backoff := time.Second
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}
		c.logger.Info("coordinator: connecting", zap.String("endpoint", c.grpcCfg.Endpoint))
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
	conn, err := c.grpcCfg.ToClientConn(c.ctx, c.host.GetExtensions(), c.settings)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	stream, err := gen.NewCoordinatorClient(conn).Connect(c.ctx)
	if err != nil {
		return err
	}
	c.logger.Info("coordinator: stream established", zap.String("endpoint", c.grpcCfg.Endpoint))

	recvErr := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			if b := msg.GetBatch(); b != nil {
				for _, raw := range b.TraceIds {
					tid := hex.EncodeToString(raw)
					c.logger.Debug("coordinator: received decision", zap.String("trace_id", tid))
					c.handler(tid)
				}
			}
		}
	}()

	for {
		select {
		case traceID := <-c.sendCh:
			if err := stream.Send(&gen.ProcessorMessage{
				Payload: &gen.ProcessorMessage_Notify{
					Notify: &gen.NotifyInteresting{TraceId: traceID[:]},
				},
			}); err != nil {
				c.logger.Error("coordinator: send failed", zap.Stringer("trace_id", traceID), zap.Error(err))
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
