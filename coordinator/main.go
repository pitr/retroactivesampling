package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
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

	bytesIn := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "coordinator_grpc_bytes_received_total",
		Help: "Total bytes received from processors via gRPC.",
	})
	bytesOut := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "coordinator_grpc_bytes_sent_total",
		Help: "Total bytes sent to processors via gRPC.",
	})
	interestingTraces := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "coordinator_interesting_traces_total",
		Help: "Total unique interesting traces seen by this coordinator.",
	})
	droppedSends := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "coordinator_broadcast_drops_total",
		Help: "Total keep decisions dropped because a processor stream's send buffer was full.",
	})
	sendErrors := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "coordinator_broadcast_send_errors_total",
		Help: "Total errors returned by stream.Send when broadcasting keep decisions.",
	})
	prometheus.MustRegister(bytesIn, bytesOut, interestingTraces, droppedSends, sendErrors)

	if cfg.MetricsListen != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		go func() {
			log.Printf("metrics listening on %s", cfg.MetricsListen)
			if err := http.ListenAndServe(cfg.MetricsListen, mux); err != nil {
				log.Printf("metrics server: %v", err)
			}
		}()
	}

	ps := rds.New(cfg.RedisAddr, cfg.DecidedKeyTTL)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	srv := server.New(func(traceID string) {
		if ctx.Err() != nil {
			return
		}
		if novel, err := ps.Publish(ctx, traceID); err != nil {
			log.Printf("publish %s: %v", traceID, err)
		} else if novel {
			interestingTraces.Add(1)
		}
	}, bytesIn, bytesOut, droppedSends, sendErrors)

	// All coordinators (including this one) subscribe to the Redis channel.
	// When this instance publishes a trace ID, it arrives back via Subscribe
	// and is broadcast to all connected processors here — this ensures a single
	// code path handles all decisions regardless of which instance originated them.
	go func() {
		if err := ps.Subscribe(ctx, func(traceID string) {
			srv.Broadcast(traceID)
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
		log.Printf("shutting down")
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
			log.Printf("graceful stop timed out, forcing")
			gs.Stop()
		}
		_ = ps.Close()
		log.Printf("shutdown complete")
	}()

	log.Printf("coordinator listening on %s", cfg.GRPCListen)
	if err := gs.Serve(lis); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
