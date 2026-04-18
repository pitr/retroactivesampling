package cache_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"retroactivesampling/processor/retroactivesampling/internal/cache"
)

func TestHasReturnsTrueAfterAdd(t *testing.T) {
	c := cache.New(time.Minute)
	c.Add("trace-1")
	assert.True(t, c.Has("trace-1"))
}

func TestHasReturnsFalseForUnknown(t *testing.T) {
	c := cache.New(time.Minute)
	assert.False(t, c.Has("trace-unknown"))
}

func TestHasReturnsFalseAfterExpiry(t *testing.T) {
	c := cache.New(50 * time.Millisecond)
	c.Add("trace-2")
	time.Sleep(80 * time.Millisecond)
	assert.False(t, c.Has("trace-2"))
}
