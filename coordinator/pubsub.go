package main

import (
	"context"

	"pitr.ca/retroactivesampling/coordinator/memory"
	"pitr.ca/retroactivesampling/coordinator/proxy"
	"pitr.ca/retroactivesampling/coordinator/redis"
)

type PubSub interface {
	Publish(ctx context.Context, traceID []byte) (bool, error)
	Close() error
}

var (
	_ PubSub = (*redis.PubSub)(nil)
	_ PubSub = (*memory.PubSub)(nil)
	_ PubSub = (*proxy.PubSub)(nil)
)
