package main

import (
	"container/list"
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"math"
	"math/bits"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	v1 "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/stats"
)

var (
	dst         = flag.String("dst", "localhost:4317,localhost:4318", "comma separated OTLP gRPC destination servers")
	server      = flag.String("listen", "0.0.0.0:9000", "OTLP gRPC endpoint to listen to (no TLS!)")
	metricsAddr = flag.String("metrics", "0.0.0.0:2112", "Prometheus metrics HTTP endpoint address")
	rate        = flag.Float64("rate", 10, "traces/sec")
	svcCount    = flag.Int("services", 10, "number of services per trace")
	waitTime    = flag.Int("wait", 20, "seconds to wait for a trace before giving up")
	stopTime    = flag.Int("stop", 0, "seconds to run for, 0 means forever")

	BytesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "tracegen_bytes_total", Help: "Bytes out transferred."}, []string{"direction"})
	SpansGot   = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "tracegen_spans_received_total", Help: "Spans received."}, []string{"reason"})
	TracesSent = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "tracegen_traces_sent_total", Help: "Total traces emitted."}, []string{"error"})
	TracesGot  = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "tracegen_traces_received_total", Help: "Traces received."}, []string{"completeness"})

	traces  = map[uint32]*list.Element{}
	tracesQ list.List
	mu      sync.Mutex

	code = (uint8)(rand.Uint32N(math.MaxUint8)) // random code for the current session
)

type traceID struct {
	id    uint32 // actual ID
	code  uint8  // unique code for this session, to weed out foreign spans
	isErr bool   // this trace should be error
}

// check that traceID is within TraceID size
var _ [16 - unsafe.Sizeof(traceID{})]byte

type counter struct{ in, out prometheus.Counter }

func (c *counter) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context   { return ctx }
func (c *counter) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context { return ctx }
func (c *counter) HandleConn(context.Context, stats.ConnStats)                       {}
func (c *counter) HandleRPC(_ context.Context, s stats.RPCStats) {
	if p, ok := s.(*stats.OutPayload); ok && c.out != nil {
		c.out.Add(float64(p.WireLength))
	}
	if p, ok := s.(*stats.InPayload); ok && c.in != nil {
		c.in.Add(float64(p.WireLength))
	}
}

type traceSlot struct {
	id    uint32 // actual ID
	age   int64  // unix time in seconds
	spans uint64 // bits per span (service 5 is in bit 5)
}

type traceListener struct {
	v1.UnimplementedTraceServiceServer
}

func (l *traceListener) Export(_ context.Context, req *v1.ExportTraceServiceRequest) (*v1.ExportTraceServiceResponse, error) {
	var foreign, nonError, corrupted, dup, late, valid float64
	for _, rs := range req.GetResourceSpans() {
		for _, ss := range rs.GetScopeSpans() {
			for _, s := range ss.GetSpans() {
				trace := *(*traceID)(unsafe.Pointer(&s.GetTraceId()[0]))
				if trace.code != code {
					foreign++
					continue
				}
				if !trace.isErr {
					nonError++
					continue
				}
				var mask uint64
				for _, a := range s.GetAttributes() {
					if a.Key == "my.seq" {
						seq := a.Value.GetIntValue()
						if seq < 0 || seq >= int64(*svcCount) {
							corrupted++
							continue
						}
						mask = uint64(1) << seq
					}
				}
				if mask == 0 {
					corrupted++
					continue
				}

				// TODO add more validation, span should be exactly the same as what we sent

				mu.Lock()
				e, ok := traces[trace.id]
				if !ok {
					mu.Unlock()
					late++
					continue
				}

				slot := e.Value.(*traceSlot)
				if slot.spans&mask != 0 {
					mu.Unlock()
					dup++
					continue
				} else {
					slot.spans |= mask
				}
				mu.Unlock()
				valid++
			}
		}
	}
	SpansGot.WithLabelValues("foreign").Add(foreign)     // came from another source
	SpansGot.WithLabelValues("non-error").Add(nonError)  // received a span from a non-error trace
	SpansGot.WithLabelValues("corrupted").Add(corrupted) // corrupted span, something is wrong
	SpansGot.WithLabelValues("dup").Add(dup)             // same span received more than once
	SpansGot.WithLabelValues("late").Add(late)           // span received, but took longer than -wait argument
	SpansGot.WithLabelValues("expected").Add(valid)

	return &v1.ExportTraceServiceResponse{}, nil
}

type ctxKey int

const isErrKey ctxKey = iota

type TraceIDGen struct{ counter atomic.Uint32 }

func (g *TraceIDGen) NewIDs(ctx context.Context) (tid trace.TraceID, sid trace.SpanID) {
	isErr, ok := ctx.Value(isErrKey).(bool)
	if !ok {
		log.Fatal("TraceIDGen used without setting context key")
	}
	*(*traceID)(unsafe.Pointer(&tid[0])) = traceID{id: g.counter.Add(1), code: code, isErr: isErr}
	binary.BigEndian.PutUint64(sid[:], rand.Uint64())
	return
}

func (g *TraceIDGen) NewSpanID(_ context.Context, _ trace.TraceID) trace.SpanID {
	sid := trace.SpanID{}
	for {
		binary.NativeEndian.PutUint64(sid[:], rand.Uint64())
		if sid.IsValid() {
			break
		}
	}
	return sid
}

func main() {
	flag.Parse()

	if *svcCount > 64 {
		log.Fatal("service count above 64 is not supported due to bit twiddling")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if *stopTime > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(*stopTime)*time.Second)
		defer cancel()
	}

	idGen := &TraceIDGen{}

	startMetrics(ctx, *metricsAddr) // &listener.bytes, outBytes
	startListener(ctx, *server)
	log.Println("server up, sleeping...")

	time.Sleep(5 * time.Second) // sleep before starting

	log.Println("starting...")

	tracers, providers := setup(ctx, strings.Split(*dst, ","), *svcCount, idGen)
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, tp := range providers {
			_ = tp.Shutdown(sctx)
		}
	}()

	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				full, part, none := sweep()
				TracesGot.WithLabelValues("full").Add(float64(full))
				TracesGot.WithLabelValues("partial").Add(float64(part))
				TracesGot.WithLabelValues("none").Add(float64(none))
			case <-ctx.Done():
				return
			}
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
				go emit(ctx, tracers)
			}
		case <-ctx.Done():
			return
		}
	}
}

func startListener(ctx context.Context, addr string) {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("listener: %v", err)
	}
	gs := grpc.NewServer(grpc.StatsHandler(&counter{in: BytesTotal.WithLabelValues("in")}))
	v1.RegisterTraceServiceServer(gs, &traceListener{})
	go func() {
		<-ctx.Done()
		gs.GracefulStop()
	}()
	go func() {
		log.Printf("grpc listening addr=%s\n", addr)
		if err := gs.Serve(lis); err != nil {
			log.Printf("listener stopped: %v", err)
		}
	}()
}

func startMetrics(ctx context.Context, addr string) {
	prometheus.MustRegister(BytesTotal, SpansGot, TracesGot, TracesSent)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	go func() {
		log.Printf("metrics listening addr=%s\n", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("metrics server: %v", err)
		}
	}()
}

func setup(ctx context.Context, dst []string, n int, idGen *TraceIDGen) ([]trace.Tracer, []*sdktrace.TracerProvider) {
	tracers := make([]trace.Tracer, n)
	providers := make([]*sdktrace.TracerProvider, n)
	for i := range n {
		exp, err := otlptracegrpc.New(ctx,
			otlptracegrpc.WithEndpoint(dst[i%len(dst)]),
			otlptracegrpc.WithInsecure(),
			otlptracegrpc.WithDialOption(
				grpc.WithStatsHandler(&counter{out: BytesTotal.WithLabelValues("out")}),
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
		res, err := resource.New(ctx, resource.WithAttributes(semconv.ServiceName(fmt.Sprintf("service-%d", i))))
		if err != nil {
			log.Fatalf("failed to create exporter for service-%d: %v", i, err)
		}
		tp := sdktrace.NewTracerProvider(
			sdktrace.WithSpanProcessor(sdktrace.NewBatchSpanProcessor(exp)),
			sdktrace.WithResource(res),
			sdktrace.WithIDGenerator(idGen),
		)
		providers[i], tracers[i] = tp, tp.Tracer("tracegen")
	}
	return tracers, providers
}

func emit(ctx context.Context, tracers []trace.Tracer) {
	isErr := rand.Float64() < 0.01
	errSvc := rand.IntN(len(tracers))

	TracesSent.WithLabelValues(strconv.FormatBool(isErr)).Add(1)

	ctx = context.WithValue(ctx, isErrKey, isErr)

	ctx, root := tracers[0].Start(ctx, "request")
	defer root.End()

	t := root.SpanContext().TraceID()
	tid := *(*traceID)(unsafe.Pointer(&t[0]))
	if isErr {
		mu.Lock()
		traces[tid.id] = tracesQ.PushFront(&traceSlot{id: tid.id, age: time.Now().Unix()})
		mu.Unlock()
	}

	if isErr && errSvc == 0 {
		root.SetStatus(codes.Error, "injected error")
	}
	root.SetAttributes(attribute.Int("my.seq", 0))

	for i := 1; i < len(tracers); i++ {
		_, s := tracers[i].Start(ctx, fmt.Sprintf("svc%d.handle", i))
		s.SetAttributes(attribute.Int("my.seq", i))
		if isErr && errSvc == i {
			s.SetStatus(codes.Error, "injected error")
		}
		s.End()
	}
}

// garbage collect old traces, track stats
func sweep() (full, part, none int) {
	now := time.Now().Unix()
	mu.Lock()
	defer mu.Unlock()

	for {
		e := tracesQ.Back()
		if e == nil {
			return
		}
		s := e.Value.(*traceSlot)
		if now-s.age < (int64)(*waitTime) {
			return
		}
		delete(traces, s.id)
		switch bits.OnesCount64(s.spans) {
		case *svcCount:
			full++
		case 0:
			none++
		default:
			part++
		}
		tracesQ.Remove(e)
	}
}
