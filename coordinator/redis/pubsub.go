package redis

import (
	"context"
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

// Publish publishes traceID if not already seen (SET NX). Returns true if newly published.
func (p *PubSub) Publish(ctx context.Context, traceID string) (bool, error) {
	key := fmt.Sprintf(decidedKeyFmt, traceID)
	ok, err := p.client.SetNX(ctx, key, 1, p.ttl).Result()
	if err != nil || !ok {
		return false, err
	}
	return true, p.client.Publish(ctx, channel, traceID).Err()
}

// Subscribe calls handler for each traceID received. Blocks until ctx is cancelled.
func (p *PubSub) Subscribe(ctx context.Context, handler func(traceID string)) error {
	sub := p.client.Subscribe(ctx, channel)
	defer sub.Close()
	for {
		select {
		case msg := <-sub.Channel():
			handler(msg.Payload)
		case <-ctx.Done():
			return nil
		}
	}
}

func (p *PubSub) Close() error {
	return p.client.Close()
}
