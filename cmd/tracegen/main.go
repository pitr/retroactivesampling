package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/stats"
)

var (
	endpoints  = flag.String("endpoints", "localhost:4317,localhost:4318", "comma separated OTLP gRPC endpoints")
	rate       = flag.Float64("rate", 10, "traces/sec")
	svcCount   = flag.Int("services", 10, "number of services")
	reportFreq = flag.Int("report-rate", 10, "report frequency in seconds")
)

type counter struct{ n atomic.Int64 }

func (c *counter) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context   { return ctx }
func (c *counter) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context { return ctx }
func (c *counter) HandleConn(context.Context, stats.ConnStats)                       {}
func (c *counter) HandleRPC(_ context.Context, s stats.RPCStats) {
	if p, ok := s.(*stats.OutPayload); ok {
		c.n.Add(int64(p.WireLength))
	}
}

func main() {
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	counter := &counter{}
	tracers, providers := setup(ctx, strings.Split(*endpoints, ","), *svcCount, counter)
	defer func() {
		for _, tp := range providers {
			_ = tp.Shutdown(ctx)
		}
	}()

	go func() {
		t := time.NewTicker(time.Duration(*reportFreq) * time.Second)
		for range t.C {
			fmt.Println("out:", prettyRate(counter.n.Swap(0), int64(*reportFreq)))
		}
	}()

	tick := time.NewTicker(time.Duration(float64(time.Second) / *rate))
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			go emit(ctx, tracers)
		case <-ctx.Done():
			return
		}
	}
}

func prettyRate(bytes, secs int64) string {
	bps := float64(bytes) / float64(secs)
	switch {
	case bps >= 1e9:
		return fmt.Sprintf("%.1f GB/s", bps/1e9)
	case bps >= 1e6:
		return fmt.Sprintf("%.1f MB/s", bps/1e6)
	case bps >= 1e3:
		return fmt.Sprintf("%.1f KB/s", bps/1e3)
	default:
		return fmt.Sprintf("%.0f B/s", bps)
	}
}

func setup(ctx context.Context, endpoints []string, n int, counter *counter) ([]trace.Tracer, []*sdktrace.TracerProvider) {
	tracers := make([]trace.Tracer, n)
	providers := make([]*sdktrace.TracerProvider, n)
	for i := range n {
		exp, err := otlptracegrpc.New(ctx,
			otlptracegrpc.WithEndpoint(endpoints[i%len(endpoints)]),
			otlptracegrpc.WithInsecure(),
			otlptracegrpc.WithDialOption(
				grpc.WithStatsHandler(counter),
				grpc.WithConnectParams(grpc.ConnectParams{
					Backoff: backoff.Config{
						BaseDelay:  500 * time.Millisecond,
						Multiplier: 1.5,
						Jitter:     0.1,
						MaxDelay:   5 * time.Second,
					},
					MinConnectTimeout: 5 * time.Second,
				}),
				grpc.WithKeepaliveParams(keepalive.ClientParameters{
					Time:                10 * time.Second,
					Timeout:             5 * time.Second,
					PermitWithoutStream: true,
				}),
			))
		if err != nil {
			log.Fatalf("failed to create exporter for %s: %v", endpoints[i%len(endpoints)], err)
		}
		res, _ := resource.New(ctx, resource.WithAttributes(semconv.ServiceName(fmt.Sprintf("service-%d", i))))
		tp := sdktrace.NewTracerProvider(
			sdktrace.WithSpanProcessor(sdktrace.NewBatchSpanProcessor(exp)),
			sdktrace.WithResource(res))
		providers[i], tracers[i] = tp, tp.Tracer("tracegen")
	}
	return tracers, providers
}

func emit(ctx context.Context, tracers []trace.Tracer) {
	isErr := rand.Float64() < 0.01
	errSvc := rand.IntN(len(tracers))

	ctx, root := tracers[0].Start(ctx, "request")
	defer root.End()
	if isErr && errSvc == 0 {
		root.SetStatus(codes.Error, "injected error")
	}

	for i := 1; i < len(tracers); i++ {
		_, s := tracers[i].Start(ctx, fmt.Sprintf("svc%d.handle", i))
		if isErr && errSvc == i {
			s.SetStatus(codes.Error, "injected error")
		}
		s.End()
	}
}
