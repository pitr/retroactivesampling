package processor

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/processor/processorhelper"
	"go.uber.org/zap"

	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/buffer"
	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/cache"
	coord "pitr.ca/retroactivesampling/processor/retroactivesampling/internal/coordinator"
	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/evaluator"
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
	wg     sync.WaitGroup
}

func newProcessor(logger *zap.Logger, cfg *Config, next consumer.Traces) (*retroactiveProcessor, error) {
	buf, err := buffer.New(cfg.BufferDBPath, cfg.MaxBufferedTraces)
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
	p.coord = coord.New(cfg.CoordinatorEndpoint, p.onDecision, logger)
	return p, nil
}

func (p *retroactiveProcessor) Shutdown(_ context.Context) error {
	p.coord.Close()
	p.ic.Close()

	p.mu.Lock()
	for _, t := range p.timers {
		if t.Stop() {
			p.wg.Done()
		}
	}
	p.timers = make(map[string]*time.Timer)
	for _, t := range p.drops {
		if t.Stop() {
			p.wg.Done()
		}
	}
	p.drops = make(map[string]*time.Timer)
	p.mu.Unlock()

	p.wg.Wait()
	return p.buf.Close()
}

func (p *retroactiveProcessor) processTraces(_ context.Context, td ptrace.Traces) (ptrace.Traces, error) {
	out := ptrace.NewTraces()
	for traceID, spans := range groupByTrace(td) {
		if p.ic.Has(traceID) {
			spans.ResourceSpans().MoveAndAppendTo(out.ResourceSpans())
			continue
		}
		if err := p.buf.WriteWithEviction(traceID, spans, time.Now()); err != nil {
			p.logger.Error("buffer full after eviction", zap.String("trace_id", traceID), zap.Error(err))
			spans.ResourceSpans().MoveAndAppendTo(out.ResourceSpans())
			continue
		}
		p.resetBufferTimer(traceID)
	}
	if out.SpanCount() == 0 {
		return out, processorhelper.ErrSkipProcessingData
	}
	return out, nil
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
	p.wg.Add(1)
	p.timers[traceID] = time.AfterFunc(p.cfg.BufferTTL, func() { defer p.wg.Done(); p.onBufferTimeout(traceID) })
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
		p.logger.Debug("buffer timeout: locally interesting, notifying coordinator", zap.String("trace_id", traceID))
		_ = p.buf.Delete(traceID) // delete first — prevents onDecision from double-ingesting
		if err := p.next.ConsumeTraces(context.Background(), traces); err != nil {
			p.logger.Error("ingest interesting trace", zap.String("trace_id", traceID), zap.Error(err))
		}
		p.ic.Add(traceID)
		p.coord.Notify(traceID)
		return
	}
	// Not interesting locally — start drop timer and wait for coordinator
	p.logger.Debug("buffer timeout: not locally interesting, waiting for coordinator decision", zap.String("trace_id", traceID), zap.Duration("drop_ttl", p.cfg.DropTTL))
	p.mu.Lock()
	p.wg.Add(1)
	p.drops[traceID] = time.AfterFunc(p.cfg.DropTTL, func() { defer p.wg.Done(); p.onDropTimeout(traceID) })
	p.mu.Unlock()
}

func (p *retroactiveProcessor) onDropTimeout(traceID string) {
	p.mu.Lock()
	delete(p.drops, traceID)
	p.mu.Unlock()
	_ = p.buf.Delete(traceID)
}

func (p *retroactiveProcessor) onDecision(traceID string, keep bool) {
	p.logger.Debug("coordinator decision received", zap.String("trace_id", traceID), zap.Bool("keep", keep))
	p.mu.Lock()
	_, hadDropTimer := p.drops[traceID]
	if t, ok := p.drops[traceID]; ok {
		if t.Stop() {
			p.wg.Done()
		}
		delete(p.drops, traceID)
	}
	p.mu.Unlock()

	if !hadDropTimer {
		return // broadcast for a trace we never buffered (or already expired); nothing to do
	}

	if !keep {
		_ = p.buf.Delete(traceID)
		return
	}
	p.ic.Add(traceID)
	traces, ok, err := p.buf.Read(traceID)
	if err != nil {
		p.logger.Warn("coordinator said keep but got error fetching it", zap.String("trace_id", traceID), zap.Error(err))
		return
	}
	if !ok {
		return
	}
	if err := p.next.ConsumeTraces(context.Background(), traces); err != nil {
		p.logger.Error("ingest coordinator-decided trace", zap.String("trace_id", traceID), zap.Error(err))
	}
	_ = p.buf.Delete(traceID)
}
