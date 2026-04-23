package memory

import (
	"context"
	"sync"
	"time"

	gocache "github.com/patrickmn/go-cache"
)

type PubSub struct {
	cache    *gocache.Cache
	mu       sync.RWMutex
	handlers []func([]byte)
}

func New(ttl time.Duration) *PubSub {
	return &PubSub{
		cache: gocache.New(ttl, ttl*2),
	}
}

func (p *PubSub) Publish(_ context.Context, traceID []byte) (bool, error) {
	if p.cache.Add(string(traceID), struct{}{}, gocache.DefaultExpiration) != nil {
		return false, nil
	}
	p.mu.RLock()
	hs := p.handlers
	p.mu.RUnlock()
	for _, h := range hs {
		h(traceID)
	}
	return true, nil
}

func (p *PubSub) Subscribe(ctx context.Context, handler func([]byte)) error {
	p.mu.Lock()
	p.handlers = append(p.handlers, handler)
	p.mu.Unlock()
	<-ctx.Done()
	return nil
}

// Close is a no-op. The ctx passed to Subscribe must be cancelled to unblock those goroutines.
func (p *PubSub) Close() error { return nil }
