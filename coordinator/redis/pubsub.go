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
	client *redis.Client
	ttl    time.Duration
}

func New(addr string, ttl time.Duration) *PubSub {
	return &PubSub{
		client: redis.NewClient(&redis.Options{Addr: addr}),
		ttl:    ttl,
	}
}

// Publish publishes traceID if not already seen (SET NX + PUBLISH). Returns true if newly published.
// Note: SET NX and PUBLISH are not atomic. A coordinator crash between the two operations
// causes silent trace loss — the NX key blocks retries but no broadcast is sent.
// For this system's best-effort semantics this is acceptable.
func (p *PubSub) Publish(ctx context.Context, traceID string) (bool, error) {
	key := fmt.Sprintf(decidedKeyFmt, traceID)
	result, err := p.client.SetArgs(ctx, key, 1, redis.SetArgs{TTL: p.ttl, Mode: "NX"}).Result()
	if errors.Is(err, redis.Nil) {
		return false, nil // key already existed
	}
	if err != nil {
		return false, err
	}
	if result != "OK" {
		return false, nil // key already existed (nil bulk → empty string path)
	}
	return true, p.client.Publish(ctx, channel, traceID).Err()
}

// Subscribe calls handler for each traceID received. Blocks until ctx is cancelled.
func (p *PubSub) Subscribe(ctx context.Context, handler func(traceID string)) error {
	sub := p.client.Subscribe(ctx, channel)
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
	return p.client.Close()
}
