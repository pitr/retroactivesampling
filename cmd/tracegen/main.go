package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand/v2"
	"net"
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
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/stats"
)

var (
	dst          = flag.String("dst", "localhost:4317,localhost:4318", "comma separated OTLP gRPC destination servers")
	server       = flag.String("serve", "0.0.0.0:5000", "OTLP gRPC server to listen to")
	rate         = flag.Float64("rate", 10, "traces/sec")
	svcCount     = flag.Int("services", 10, "number of services per trace")
	reportFreq   = flag.Int("report-rate", 10, "print report frequency in seconds")
	traceTimeout = flag.Int("trace-timeout", 30, "seconds before an un-completed trace is declared incomplete")
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

type inCounter struct{ n atomic.Int64 }

func (c *inCounter) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context   { return ctx }
func (c *inCounter) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context { return ctx }
func (c *inCounter) HandleConn(context.Context, stats.ConnStats)                       {}
func (c *inCounter) HandleRPC(_ context.Context, s stats.RPCStats) {
	if p, ok := s.(*stats.InPayload); ok {
		c.n.Add(int64(p.WireLength))
	}
}

type traceListener struct {
	coltracepb.UnimplementedTraceServiceServer
	ring     *traceRing
	svcCount int
	bytesIn  inCounter
}

func (l *traceListener) Export(_ context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	now := time.Now().UnixNano()
	for _, rs := range req.GetResourceSpans() {
		for _, ss := range rs.GetScopeSpans() {
			for _, s := range ss.GetSpans() {
				tid := s.GetTraceId()
				if len(tid) != 16 {
					continue
				}
				for _, b := range tid[:8] {
					if b != 0 {
						log.Printf("foreign span traceID prefix: %x", tid[:8])
						goto nextSpan
					}
				}
				{
					seqID := binary.BigEndian.Uint64(tid[8:])
					slot := &l.ring.slots[seqID&l.ring.mask]
					slot.firstSeen.CompareAndSwap(0, now)
					slot.spanCount.Add(1)
				}
			nextSpan:
			}
		}
	}
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

func startListener(ctx context.Context, addr string, l *traceListener) {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("listener: %v", err)
	}
	gs := grpc.NewServer(grpc.StatsHandler(&l.bytesIn))
	coltracepb.RegisterTraceServiceServer(gs, l)
	go func() {
		<-ctx.Done()
		gs.GracefulStop()
	}()
	go func() {
		if err := gs.Serve(lis); err != nil {
			log.Printf("listener stopped: %v", err)
		}
	}()
}

type seqIDGen struct{ counter atomic.Uint64 }

func (g *seqIDGen) NewIDs(_ context.Context) (trace.TraceID, trace.SpanID) {
	seq := g.counter.Add(1) - 1
	var tid trace.TraceID
	binary.BigEndian.PutUint64(tid[8:], seq)
	var sid trace.SpanID
	binary.BigEndian.PutUint64(sid[:], rand.Uint64())
	return tid, sid
}

func (g *seqIDGen) NewSpanID(_ context.Context, _ trace.TraceID) trace.SpanID {
	var sid trace.SpanID
	binary.BigEndian.PutUint64(sid[:], rand.Uint64())
	return sid
}

type traceSlot struct {
	generatedAt atomic.Int64 // unix nanos; 0 = unused
	firstSeen   atomic.Int64 // unix nanos; 0 = not yet received
	spanCount   atomic.Int32
	isError     atomic.Bool
}

type traceRing struct {
	slots []traceSlot
	mask  uint64
}

func nextPow2(n uint64) uint64 {
	if n == 0 {
		return 1
	}
	n--
	n |= n >> 1
	n |= n >> 2
	n |= n >> 4
	n |= n >> 8
	n |= n >> 16
	n |= n >> 32
	return n + 1
}

func newRing(rate float64, timeoutSecs int) *traceRing {
	n := uint64(rate) * uint64(timeoutSecs) * 4
	if n < 1024 {
		n = 1024
	}
	n = nextPow2(n)
	return &traceRing{slots: make([]traceSlot, n), mask: n - 1}
}

func main() {
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	outBytes := &counter{}
	idGen := &seqIDGen{}
	ring := newRing(*rate, *traceTimeout)
	listener := &traceListener{ring: ring, svcCount: *svcCount}
	startListener(ctx, *server, listener)
	tracers, providers := setup(ctx, strings.Split(*dst, ","), *svcCount, outBytes, idGen)
	defer func() {
		for _, tp := range providers {
			_ = tp.Shutdown(ctx)
		}
	}()

	go func() {
		t := time.NewTicker(time.Duration(*reportFreq) * time.Second)
		for range t.C {
			fmt.Println("out:", prettyRate(outBytes.n.Swap(0), int64(*reportFreq)))
		}
	}()

	// Batch multiple emits per tick: OS timer fires at most ~1000x/sec.
	const timerHz = 1000.0
	batchSize := int(math.Ceil(*rate / timerHz))
	tickInterval := time.Duration(float64(time.Second) / *rate * float64(batchSize))

	tick := time.NewTicker(tickInterval)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			for range batchSize {
				go emit(ctx, tracers, ring)
			}
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

func setup(ctx context.Context, dst []string, n int, counter *counter, idGen *seqIDGen) ([]trace.Tracer, []*sdktrace.TracerProvider) {
	tracers := make([]trace.Tracer, n)
	providers := make([]*sdktrace.TracerProvider, n)
	for i := range n {
		exp, err := otlptracegrpc.New(ctx,
			otlptracegrpc.WithEndpoint(dst[i%len(dst)]),
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
			log.Fatalf("failed to create exporter for %s: %v", dst[i%len(dst)], err)
		}
		res, _ := resource.New(ctx, resource.WithAttributes(semconv.ServiceName(fmt.Sprintf("service-%d", i))))
		tp := sdktrace.NewTracerProvider(
			sdktrace.WithSpanProcessor(sdktrace.NewBatchSpanProcessor(exp)),
			sdktrace.WithResource(res),
			sdktrace.WithIDGenerator(idGen),
		)
		providers[i], tracers[i] = tp, tp.Tracer("tracegen")
	}
	return tracers, providers
}

func emit(ctx context.Context, tracers []trace.Tracer, ring *traceRing) {
	isErr := rand.Float64() < 0.01
	errSvc := rand.IntN(len(tracers))

	ctx, root := tracers[0].Start(ctx, "request")
	defer root.End()

	tid := root.SpanContext().TraceID()
	seqID := binary.BigEndian.Uint64(tid[8:])
	slot := &ring.slots[seqID&ring.mask]
	slot.isError.Store(isErr)
	slot.generatedAt.Store(time.Now().UnixNano())

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
