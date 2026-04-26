package redis

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	channel       = "retrosampling:interesting"
	decidedKeyFmt = "retrosampling:decided:%x"
)

type PubSub struct {
	writeClient *redis.Client
	readClient  *redis.Client
	ttl         time.Duration
	handler     func([]byte)
	ctx         context.Context
	cancel      context.CancelFunc
}

func New(writeCfg Config, readCfg *Config, ttl time.Duration, handler func([]byte)) (*PubSub, error) {
	writeOpts, err := writeCfg.options()
	if err != nil {
		return nil, err
	}
	wc := redis.NewClient(writeOpts)

	rc := wc
	if readCfg != nil {
		readOpts, err := readCfg.options()
		if err != nil {
			return nil, err
		}
		rc = redis.NewClient(readOpts)
	}

	ctx, cancel := context.WithCancel(context.Background())
	p := &PubSub{
		writeClient: wc,
		readClient:  rc,
		ttl:         ttl,
		handler:     handler,
		ctx:         ctx,
		cancel:      cancel,
	}
	go p.receive()
	return p, nil
}

// Publish publishes traceID if not already seen (SET NX + PUBLISH). Returns true if newly published.
// Note: SET NX and PUBLISH are not atomic. A coordinator crash between the two operations
// causes silent trace loss — the NX key blocks retries but no broadcast is sent.
// For this system's best-effort semantics this is acceptable.
func (p *PubSub) Publish(ctx context.Context, traceID []byte) (bool, error) {
	result, err := p.writeClient.SetArgs(ctx, fmt.Sprintf(decidedKeyFmt, traceID), 1, redis.SetArgs{TTL: p.ttl, Mode: "NX"}).Result()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if result != "OK" {
		return false, nil
	}
	return true, p.writeClient.Publish(ctx, channel, traceID).Err()
}

func (p *PubSub) receive() {
	sub := p.readClient.Subscribe(p.ctx, channel)
	defer func() { _ = sub.Close() }()
	for {
		select {
		case msg := <-sub.Channel():
			if msg != nil {
				p.handler([]byte(msg.Payload))
			}
		case <-p.ctx.Done():
			return
		}
	}
}

func (p *PubSub) Close() error {
	p.cancel()
	err := p.writeClient.Close()
	if p.readClient != p.writeClient {
		err = errors.Join(err, p.readClient.Close())
	}
	return err
}
