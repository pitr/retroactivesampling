package cache

import (
	"container/list"
	"sync"
)

type InterestCache struct {
	mu      sync.Mutex
	cap     int
	entries map[string]*list.Element
	lru     list.List
}

func New(capacity int) *InterestCache {
	return &InterestCache{
		cap:     capacity,
		entries: make(map[string]*list.Element),
	}
}

func (c *InterestCache) Add(traceID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[traceID]; ok {
		c.lru.MoveToFront(el)
		return
	}
	el := c.lru.PushFront(traceID)
	c.entries[traceID] = el
	if c.lru.Len() > c.cap {
		back := c.lru.Back()
		c.lru.Remove(back)
		delete(c.entries, back.Value.(string))
	}
}

func (c *InterestCache) Has(traceID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[traceID]
	if !ok {
		return false
	}
	c.lru.MoveToFront(el)
	return true
}

func (c *InterestCache) Delete(traceID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[traceID]; ok {
		c.lru.Remove(el)
		delete(c.entries, traceID)
	}
}
