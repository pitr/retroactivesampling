package cache_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/cache"
)

func TestHasReturnsTrueAfterAdd(t *testing.T) {
	c := cache.New(10)
	c.Add("trace-1")
	assert.True(t, c.Has("trace-1"))
}

func TestHasReturnsFalseForUnknown(t *testing.T) {
	c := cache.New(10)
	assert.False(t, c.Has("trace-unknown"))
}

func TestDeleteRemovesEntry(t *testing.T) {
	c := cache.New(10)
	c.Add("trace-1")
	c.Delete("trace-1")
	assert.False(t, c.Has("trace-1"))
}

func TestDeleteNonExistentIsNoOp(t *testing.T) {
	c := cache.New(10)
	c.Delete("trace-unknown") // must not panic
}

func TestLRUEvictsOldestWhenAtCapacity(t *testing.T) {
	c := cache.New(2)
	c.Add("trace-1")
	c.Add("trace-2")
	c.Add("trace-3") // evicts trace-1
	assert.False(t, c.Has("trace-1"), "oldest entry should be evicted")
	assert.True(t, c.Has("trace-2"))
	assert.True(t, c.Has("trace-3"))
}

func TestHasUpdatesLRUPosition(t *testing.T) {
	c := cache.New(2)
	c.Add("trace-1")
	c.Add("trace-2")
	c.Has("trace-1")  // moves trace-1 to front; trace-2 becomes LRU tail
	c.Add("trace-3")  // evicts trace-2
	assert.True(t, c.Has("trace-1"), "recently accessed entry should survive")
	assert.False(t, c.Has("trace-2"), "LRU tail should be evicted")
	assert.True(t, c.Has("trace-3"))
}

func TestAddExistingMovesToFront(t *testing.T) {
	c := cache.New(2)
	c.Add("trace-1")
	c.Add("trace-2")
	c.Add("trace-1") // re-add moves to front; trace-2 is now LRU tail
	c.Add("trace-3") // evicts trace-2
	assert.True(t, c.Has("trace-1"))
	assert.False(t, c.Has("trace-2"), "trace-2 should be evicted as LRU")
	assert.True(t, c.Has("trace-3"))
}
