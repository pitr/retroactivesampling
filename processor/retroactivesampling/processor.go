package processor

import (
	"context"
	"time"

	"go.opentelemetry.io/collector/component"
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
}

func newProcessor(set component.TelemetrySettings, cfg *Config, next consumer.Traces) (*retroactiveProcessor, error) {
	ic := cache.New(cfg.MaxInterestCacheEntries)
	buf, err := buffer.New(cfg.BufferFile, cfg.MaxBufferBytes)
	if err != nil {
		return nil, err
	}
	chain, err := evaluator.Build(cfg.Rules)
	if err != nil {
		buf.Close()
		return nil, err
	}
	p := &retroactiveProcessor{
		logger: set.Logger,
		next:   next,
		buf:    buf,
		ic:     ic,
		eval:   chain,
	}
	p.coord = coord.New(cfg.CoordinatorEndpoint, p.onDecision, set.Logger)
	return p, nil
}

func (p *retroactiveProcessor) Shutdown(_ context.Context) error {
	p.coord.Close()
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
	}
	if out.SpanCount() == 0 {
		return out, processorhelper.ErrSkipProcessingData
	}
	return out, nil
}

func (p *retroactiveProcessor) ingestInteresting(traceID string, current ptrace.Traces) {
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

func (p *retroactiveProcessor) onDecision(traceID string) {
	p.logger.Debug("coordinator decision received", zap.String("trace_id", traceID))
	p.ic.Add(traceID)
	traces, ok, err := p.buf.Read(traceID)
	if err != nil {
		p.logger.Warn("coordinator decision: error fetching buffered trace", zap.String("trace_id", traceID), zap.Error(err))
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
