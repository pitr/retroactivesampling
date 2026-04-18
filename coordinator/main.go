package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"

	gen "pitr.ca/retroactivesampling/proto"
	rds "pitr.ca/retroactivesampling/coordinator/redis"
	"pitr.ca/retroactivesampling/coordinator/server"
)

func main() {
	cfgPath := flag.String("config", "coordinator.yaml", "path to config file")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	if cfg.GRPCListen == "" {
		log.Fatal("grpc_listen is required")
	}
	if cfg.RedisAddr == "" {
		log.Fatal("redis_addr is required")
	}
	if cfg.DecidedKeyTTL == 0 {
		log.Fatal("decided_key_ttl is required")
	}

	ps := rds.New(cfg.RedisAddr, cfg.DecidedKeyTTL)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	srv := server.New(func(traceID string) {
		ok, err := ps.Publish(ctx, traceID)
		if err != nil {
			log.Printf("publish %s: %v", traceID, err)
			return
		}
		if !ok {
			return // duplicate, already broadcast by another instance
		}
	})

	// All coordinators (including this one) subscribe to the Redis channel.
	// When this instance publishes a trace ID, it arrives back via Subscribe
	// and is broadcast to all connected processors here — this ensures a single
	// code path handles all decisions regardless of which instance originated them.
	go func() {
		if err := ps.Subscribe(ctx, func(traceID string) {
			srv.Broadcast(traceID, true)
		}); err != nil {
			log.Printf("redis subscribe error: %v", err)
		}
	}()

	lis, err := net.Listen("tcp", cfg.GRPCListen)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	gs := grpc.NewServer()
	gen.RegisterCoordinatorServer(gs, srv)

	go func() {
		<-ctx.Done()
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		done := make(chan struct{})
		go func() {
			gs.GracefulStop()
			close(done)
		}()
		select {
		case <-done:
		case <-stopCtx.Done():
			gs.Stop()
		}
		_ = ps.Close()
	}()

	log.Printf("coordinator listening on %s", cfg.GRPCListen)
	if err := gs.Serve(lis); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
