package main

import (
	"context"

	"pitr.ca/retroactivesampling/coordinator/memory"
	"pitr.ca/retroactivesampling/coordinator/redis"
	"pitr.ca/retroactivesampling/coordinator/upstream"
)

type PubSub interface {
	Publish(ctx context.Context, traceID []byte) (bool, error)
	Subscribe(ctx context.Context, handler func([]byte)) error
	Close() error
}

var (
	_ PubSub = (*redis.PubSub)(nil)
	_ PubSub = (*memory.PubSub)(nil)
	_ PubSub = (*upstream.PubSub)(nil)
)
