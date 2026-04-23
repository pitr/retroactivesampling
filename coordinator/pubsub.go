package main

import "context"

type PubSub interface {
	Publish(ctx context.Context, traceID string) (bool, error)
	Subscribe(ctx context.Context, handler func(string)) error
	Close() error
}
