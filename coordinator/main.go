package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"

	gen "retroactivesampling/gen"
	rds "retroactivesampling/coordinator/redis"
	"retroactivesampling/coordinator/server"
)

func main() {
	cfgPath := flag.String("config", "coordinator.yaml", "path to config file")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	ps := rds.New(cfg.RedisAddr, cfg.DecidedKeyTTL)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	srv := server.New(func(traceID string) {
		ok, err := ps.Publish(ctx, traceID)
		if err != nil {
			log.Printf("publish error: %v", err)
			return
		}
		if !ok {
			return // duplicate, already broadcast by another instance
		}
	})

	// Subscribe to Redis; broadcast decisions from other coordinator instances
	go func() {
		_ = ps.Subscribe(ctx, func(traceID string) {
			srv.Broadcast(traceID, true)
		})
	}()

	lis, err := net.Listen("tcp", cfg.GRPCListen)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	gs := grpc.NewServer()
	gen.RegisterCoordinatorServer(gs, srv)

	go func() {
		<-ctx.Done()
		gs.GracefulStop()
		_ = ps.Close()
	}()

	log.Printf("coordinator listening on %s", cfg.GRPCListen)
	if err := gs.Serve(lis); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
