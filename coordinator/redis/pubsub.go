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
	decidedKeyFmt = "retrosampling:decided:%s"
)

type PubSub struct {
	writeClient *redis.Client
	readClient  *redis.Client
	ttl         time.Duration
}

func New(writeCfg Config, readCfg *Config, ttl time.Duration) (*PubSub, error) {
	writeOpts, err := writeCfg.options()
	if err != nil {
		return nil, err
	}
	wc := redis.NewClient(writeOpts)
	if readCfg == nil {
		return &PubSub{writeClient: wc, readClient: wc, ttl: ttl}, nil
	}
	readOpts, err := readCfg.options()
	if err != nil {
		return nil, err
	}
	return &PubSub{writeClient: wc, readClient: redis.NewClient(readOpts), ttl: ttl}, nil
}

// Publish publishes traceID if not already seen (SET NX + PUBLISH). Returns true if newly published.
// Note: SET NX and PUBLISH are not atomic. A coordinator crash between the two operations
// causes silent trace loss — the NX key blocks retries but no broadcast is sent.
// For this system's best-effort semantics this is acceptable.
func (p *PubSub) Publish(ctx context.Context, traceID string) (bool, error) {
	key := fmt.Sprintf(decidedKeyFmt, traceID)
	result, err := p.writeClient.SetArgs(ctx, key, 1, redis.SetArgs{TTL: p.ttl, Mode: "NX"}).Result()
	if errors.Is(err, redis.Nil) {
		return false, nil // key already existed
	}
	if err != nil {
		return false, err
	}
	if result != "OK" {
		return false, nil // key already existed (nil bulk → empty string path)
	}
	return true, p.writeClient.Publish(ctx, channel, traceID).Err()
}

// Subscribe calls handler for each traceID received. Blocks until ctx is cancelled.
func (p *PubSub) Subscribe(ctx context.Context, handler func(traceID string)) error {
	sub := p.readClient.Subscribe(ctx, channel)
	defer func() { _ = sub.Close() }()
	for {
		select {
		case msg := <-sub.Channel():
			if msg != nil {
				handler(msg.Payload)
			}
		case <-ctx.Done():
			return nil
		}
	}
}

func (p *PubSub) Close() error {
	err := p.writeClient.Close()
	if p.readClient != p.writeClient {
		if e := p.readClient.Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}
