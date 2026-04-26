package memory

import (
	"context"
	"time"

	gocache "github.com/patrickmn/go-cache"
)

type PubSub struct {
	cache   *gocache.Cache
	handler func([]byte)
}

func New(ttl time.Duration, handler func([]byte)) *PubSub {
	return &PubSub{
		cache:   gocache.New(ttl, ttl*2),
		handler: handler,
	}
}

func (p *PubSub) Publish(_ context.Context, traceID []byte) (bool, error) {
	if p.cache.Add(string(traceID), struct{}{}, gocache.DefaultExpiration) != nil {
		return false, nil
	}
	p.handler(traceID)
	return true, nil
}

func (p *PubSub) Close() error { return nil }
