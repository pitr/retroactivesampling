package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/stats"
)

var (
	endpoint   = flag.String("endpoint", "localhost:4317", "OTLP gRPC endpoint")
	rate       = flag.Float64("rate", 3, "traces/sec")
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

	ctx := context.Background()
	counter := &counter{}
	tracers, providers := setup(ctx, *endpoint, *svcCount, counter)
	defer func() {
		for _, tp := range providers {
			tp.Shutdown(ctx)
		}
	}()

	go func() {
		t := time.NewTicker(time.Duration(*reportFreq) * time.Second)
		for range t.C {
			fmt.Printf("out: %d B/s\n", counter.n.Swap(0)/int64(*reportFreq))
		}
	}()

	tick := time.NewTicker(time.Duration(float64(time.Second) / *rate))
	defer tick.Stop()
	for range tick.C {
		go emit(ctx, tracers)
	}
}

func setup(ctx context.Context, endpoint string, n int, counter *counter) ([]trace.Tracer, []*sdktrace.TracerProvider) {
	tracers := make([]trace.Tracer, n)
	providers := make([]*sdktrace.TracerProvider, n)
	for i := range n {
		exp, _ := otlptracegrpc.New(ctx,
			otlptracegrpc.WithEndpoint(endpoint),
			otlptracegrpc.WithInsecure(),
			otlptracegrpc.WithDialOption(grpc.WithStatsHandler(counter)))
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
