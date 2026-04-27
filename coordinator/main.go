package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"pitr.ca/retroactivesampling/coordinator/memory"
	"pitr.ca/retroactivesampling/coordinator/proxy"
	"pitr.ca/retroactivesampling/coordinator/redis"
	"pitr.ca/retroactivesampling/coordinator/server"
)

func fatal(msg string, args ...any) {
	slog.Error(msg, args...)
	os.Exit(1)
}

func main() {
	cfgPath := flag.String("config", "coordinator.yaml", "path to config file")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fatal("load config", "err", err)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel})))

	if cfg.GRPCListen == "" {
		fatal("grpc_listen is required")
	}

	activeMode, err := cfg.Mode.active()
	if err != nil {
		fatal("config mode", "err", err)
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
			slog.Info("metrics listening", "addr", cfg.MetricsListen)
			if err := http.ListenAndServe(cfg.MetricsListen, mux); err != nil {
				slog.Error("metrics server", "err", err)
			}
		}()
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var ps PubSub
	srv := server.New(func(traceID []byte) {
		if ctx.Err() != nil {
			return
		}
		if novel, err := ps.Publish(ctx, traceID); err != nil {
			slog.Error("publish", "trace_id", fmt.Sprintf("%x", traceID), "err", err)
		} else if novel {
			interestingTraces.Add(1)
		}
	}, bytesIn, bytesOut, droppedSends, sendErrors)

	switch m := activeMode.(type) {
	case *SingleConfig:
		if m.DecidedKeyTTL == 0 {
			fatal("single mode: decided_key_ttl is required")
		}
		slog.Info("running in single-node mode")
		ps = memory.New(m.DecidedKeyTTL, srv.Broadcast)
	case *DistributedConfig:
		if m.DecidedKeyTTL == 0 {
			fatal("distributed mode: decided_key_ttl is required")
		}
		if m.RedisPrimary.Endpoint == "" {
			fatal("distributed mode: redis_primary.endpoint is required")
		}
		var replicaCfg *redis.Config
		if len(m.RedisReplicas) > 0 {
			rc := m.RedisReplicas[rand.Intn(len(m.RedisReplicas))]
			replicaCfg = &rc
			slog.Info("subscribing to Redis replica", "addr", rc.Endpoint)
		}
		ps, err = redis.New(m.RedisPrimary, replicaCfg, m.DecidedKeyTTL, srv.Broadcast)
		if err != nil {
			fatal("redis", "err", err)
		}
	case *proxy.ClientConfig:
		slog.Info("running in proxy mode", "endpoint", m.Endpoint)
		ps, err = proxy.New(*m, srv.Broadcast)
		if err != nil {
			fatal("proxy", "err", err)
		}
	default:
		fatal("unknown mode type", "type", fmt.Sprintf("%T", activeMode))
	}

	tlsCreds, err := cfg.TLS.Credentials()
	if err != nil {
		fatal("server tls", "err", err)
	}
	if err := srv.Start(ctx, cfg.GRPCListen, cfg.ShutdownTimeout, tlsCreds); err != nil {
		fatal("serve", "err", err)
	}
	_ = ps.Close()
	slog.Info("shutdown complete")
}
