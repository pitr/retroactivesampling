package cache

import (
	"sync"
	"time"
)

type InterestCache struct {
	mu      sync.Mutex
	entries map[string]time.Time
	ttl     time.Duration
}

func New(ttl time.Duration) *InterestCache {
	c := &InterestCache{
		entries: make(map[string]time.Time),
		ttl:     ttl,
	}
	go c.sweep()
	return c
}

func (c *InterestCache) Add(traceID string) {
	c.mu.Lock()
	c.entries[traceID] = time.Now().Add(c.ttl)
	c.mu.Unlock()
}

func (c *InterestCache) Has(traceID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	exp, ok := c.entries[traceID]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(c.entries, traceID)
		return false
	}
	return true
}

func (c *InterestCache) sweep() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for range t.C {
		now := time.Now()
		c.mu.Lock()
		for id, exp := range c.entries {
			if now.After(exp) {
				delete(c.entries, id)
			}
		}
		c.mu.Unlock()
	}
}
