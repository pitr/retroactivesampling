package processor

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"

	"retroactivesampling/processor/retroactivesampling/internal/buffer"
	"retroactivesampling/processor/retroactivesampling/internal/cache"
	coord "retroactivesampling/processor/retroactivesampling/internal/coordinator"
	"retroactivesampling/processor/retroactivesampling/internal/evaluator"
)

type retroactiveProcessor struct {
	logger *zap.Logger
	next   consumer.Traces
	buf    *buffer.SpanBuffer
	ic     *cache.InterestCache
	eval   evaluator.Evaluator
	coord  *coord.Client
	cfg    *Config

	mu     sync.Mutex
	timers map[string]*time.Timer // buffer_ttl timers
	drops  map[string]*time.Timer // drop_ttl timers
}

func newProcessor(logger *zap.Logger, cfg *Config, next consumer.Traces) (*retroactiveProcessor, error) {
	buf, err := buffer.New(cfg.BufferDBPath)
	if err != nil {
		return nil, err
	}
	chain, err := evaluator.Build(cfg.Rules)
	if err != nil {
		buf.Close()
		return nil, err
	}
	p := &retroactiveProcessor{
		logger: logger,
		next:   next,
		buf:    buf,
		ic:     cache.New(cfg.InterestCacheTTL),
		eval:   chain,
		cfg:    cfg,
		timers: make(map[string]*time.Timer),
		drops:  make(map[string]*time.Timer),
	}
	p.coord = coord.New(cfg.CoordinatorEndpoint, p.onDecision)
	return p, nil
}

func (p *retroactiveProcessor) Start(_ context.Context, _ component.Host) error { return nil }

func (p *retroactiveProcessor) Shutdown(_ context.Context) error {
	p.coord.Close()

	p.mu.Lock()
	for _, t := range p.timers {
		t.Stop()
	}
	p.timers = make(map[string]*time.Timer)
	for _, t := range p.drops {
		t.Stop()
	}
	p.drops = make(map[string]*time.Timer)
	p.mu.Unlock()

	return p.buf.Close()
}

func (p *retroactiveProcessor) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: false}
}

func (p *retroactiveProcessor) ConsumeTraces(ctx context.Context, td ptrace.Traces) error {
	for traceID, spans := range groupByTrace(td) {
		if p.ic.Has(traceID) {
			if err := p.next.ConsumeTraces(ctx, spans); err != nil {
				return err
			}
			continue
		}
		if err := p.buf.WriteWithEviction(traceID, spans, time.Now()); err != nil {
			p.logger.Error("buffer full after eviction", zap.String("trace_id", traceID), zap.Error(err))
			if err2 := p.next.ConsumeTraces(ctx, spans); err2 != nil {
				return err2
			}
			continue
		}
		p.resetBufferTimer(traceID)
	}
	return nil
}

func (p *retroactiveProcessor) resetBufferTimer(traceID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t, ok := p.timers[traceID]; ok {
		// AfterFunc timers have no channel to drain; Reset is safe to call even if the
		// callback is mid-flight (unlike channel-based timers which require draining).
		t.Reset(p.cfg.BufferTTL)
		return
	}
	p.timers[traceID] = time.AfterFunc(p.cfg.BufferTTL, func() { p.onBufferTimeout(traceID) })
}

func (p *retroactiveProcessor) onBufferTimeout(traceID string) {
	p.mu.Lock()
	delete(p.timers, traceID)
	p.mu.Unlock()

	traces, ok, err := p.buf.Read(traceID)
	if err != nil || !ok {
		return
	}
	if p.eval.Evaluate(traces) {
		_ = p.buf.Delete(traceID)   // delete first — prevents onDecision from double-ingesting
		if err := p.next.ConsumeTraces(context.Background(), traces); err != nil {
			p.logger.Error("ingest interesting trace", zap.String("trace_id", traceID), zap.Error(err))
		}
		p.ic.Add(traceID)
		p.coord.Notify(traceID)
		return
	}
	// Not interesting locally — start drop timer and wait for coordinator
	p.mu.Lock()
	id := traceID
	p.drops[id] = time.AfterFunc(p.cfg.DropTTL, func() { p.onDropTimeout(id) })
	p.mu.Unlock()
}

func (p *retroactiveProcessor) onDropTimeout(traceID string) {
	p.mu.Lock()
	delete(p.drops, traceID)
	p.mu.Unlock()
	_ = p.buf.Delete(traceID)
}

func (p *retroactiveProcessor) onDecision(traceID string, keep bool) {
	p.mu.Lock()
	if t, ok := p.drops[traceID]; ok {
		t.Stop()
		delete(p.drops, traceID)
	}
	p.mu.Unlock()

	p.ic.Add(traceID)
	if !keep {
		_ = p.buf.Delete(traceID)
		return
	}
	traces, ok, err := p.buf.Read(traceID)
	if err != nil || !ok {
		return
	}
	if err := p.next.ConsumeTraces(context.Background(), traces); err != nil {
		p.logger.Error("ingest coordinator-decided trace", zap.String("trace_id", traceID), zap.Error(err))
	}
	_ = p.buf.Delete(traceID)
}
