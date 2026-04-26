package cache_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/collector/pdata/pcommon"

	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/cache"
)

func tid(s string) pcommon.TraceID {
	var t pcommon.TraceID
	copy(t[:], s)
	return t
}

func TestHasReturnsTrueAfterAdd(t *testing.T) {
	c := cache.New(10)
	c.Add(tid("trace-1"))
	assert.True(t, c.Has(tid("trace-1")))
}

func TestHasReturnsFalseForUnknown(t *testing.T) {
	c := cache.New(10)
	assert.False(t, c.Has(tid("trace-unknown")))
}

func TestLRUEvictsOldestWhenAtCapacity(t *testing.T) {
	c := cache.New(2)
	c.Add(tid("trace-1"))
	c.Add(tid("trace-2"))
	c.Add(tid("trace-3")) // evicts trace-1
	assert.False(t, c.Has(tid("trace-1")), "oldest entry should be evicted")
	assert.True(t, c.Has(tid("trace-2")))
	assert.True(t, c.Has(tid("trace-3")))
}

func TestHasUpdatesLRUPosition(t *testing.T) {
	c := cache.New(2)
	c.Add(tid("trace-1"))
	c.Add(tid("trace-2"))
	c.Has(tid("trace-1"))  // moves trace-1 to front; trace-2 becomes LRU tail
	c.Add(tid("trace-3"))  // evicts trace-2
	assert.True(t, c.Has(tid("trace-1")), "recently accessed entry should survive")
	assert.False(t, c.Has(tid("trace-2")), "LRU tail should be evicted")
	assert.True(t, c.Has(tid("trace-3")))
}

func TestAddExistingMovesToFront(t *testing.T) {
	c := cache.New(2)
	c.Add(tid("trace-1"))
	c.Add(tid("trace-2"))
	c.Add(tid("trace-1")) // re-add moves to front; trace-2 is now LRU tail
	c.Add(tid("trace-3")) // evicts trace-2
	assert.True(t, c.Has(tid("trace-1")))
	assert.False(t, c.Has(tid("trace-2")), "trace-2 should be evicted as LRU")
	assert.True(t, c.Has(tid("trace-3")))
}
