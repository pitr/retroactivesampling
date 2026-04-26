package cache

import (
	"container/list"
	"sync"

	"go.opentelemetry.io/collector/pdata/pcommon"
)

type InterestCache struct {
	mu      sync.Mutex
	cap     int
	entries map[pcommon.TraceID]*list.Element
	lru     list.List
}

func New(capacity int) *InterestCache {
	return &InterestCache{
		cap:     capacity,
		entries: make(map[pcommon.TraceID]*list.Element),
	}
}

func (c *InterestCache) Add(traceID pcommon.TraceID) {
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
		delete(c.entries, back.Value.(pcommon.TraceID))
	}
}

func (c *InterestCache) Has(traceID pcommon.TraceID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[traceID]
	if !ok {
		return false
	}
	c.lru.MoveToFront(el)
	return true
}
