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

	mu    sync.Mutex
	drops map[string]*time.Timer
	wg    sync.WaitGroup
}

func newProcessor(logger *zap.Logger, cfg *Config, next consumer.Traces) (*retroactiveProcessor, error) {
	buf, err := buffer.New(cfg.BufferDir, cfg.MaxBufferBytes)
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
		ic:     cache.New(cfg.DropTTL),
		eval:   chain,
		cfg:    cfg,
		drops:  make(map[string]*time.Timer),
	}
	p.coord = coord.New(cfg.CoordinatorEndpoint, p.onDecision, logger)
	return p, nil
}

func (p *retroactiveProcessor) Shutdown(_ context.Context) error {
	p.coord.Close()
	p.ic.Close()

	p.mu.Lock()
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
		if p.eval.Evaluate(spans) {
			p.ingestInteresting(traceID, spans)
			continue
		}
		if err := p.buf.WriteWithEviction(traceID, spans, time.Now()); err != nil {
			p.logger.Error("buffer full after eviction", zap.String("trace_id", traceID), zap.Error(err))
			spans.ResourceSpans().MoveAndAppendTo(out.ResourceSpans())
			continue
		}
		p.startDropTimer(traceID)
	}
	if out.SpanCount() == 0 {
		return out, processorhelper.ErrSkipProcessingData
	}
	return out, nil
}

func (p *retroactiveProcessor) ingestInteresting(traceID string, current ptrace.Traces) {
	p.mu.Lock()
	if t, ok := p.drops[traceID]; ok {
		if t.Stop() {
			p.wg.Done()
		}
		delete(p.drops, traceID)
	}
	p.mu.Unlock()

	p.ic.Add(traceID)
	p.coord.Notify(traceID)

	buffered, ok, err := p.buf.Read(traceID)
	if err != nil {
		p.logger.Warn("read buffer for interesting trace", zap.String("trace_id", traceID), zap.Error(err))
		if err2 := p.next.ConsumeTraces(context.Background(), current); err2 != nil {
			p.logger.Error("ingest interesting trace", zap.String("trace_id", traceID), zap.Error(err2))
		}
		_ = p.buf.Delete(traceID)
		return
	}
	if ok {
		current.ResourceSpans().MoveAndAppendTo(buffered.ResourceSpans())
		current = buffered
	}
	if err := p.next.ConsumeTraces(context.Background(), current); err != nil {
		p.logger.Error("ingest interesting trace", zap.String("trace_id", traceID), zap.Error(err))
	}
	_ = p.buf.Delete(traceID)
}

func (p *retroactiveProcessor) startDropTimer(traceID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.drops[traceID]; ok {
		return
	}
	p.wg.Add(1)
	p.drops[traceID] = time.AfterFunc(p.cfg.DropTTL, func() { defer p.wg.Done(); p.onDropTimeout(traceID) })
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
	t, hadDropTimer := p.drops[traceID]
	if hadDropTimer {
		if t.Stop() {
			p.wg.Done()
		}
		delete(p.drops, traceID)
	}
	p.mu.Unlock()

	if !hadDropTimer {
		return
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
