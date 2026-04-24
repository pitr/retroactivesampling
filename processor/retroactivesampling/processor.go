package retroactivesampling

import (
	"context"
	"encoding/hex"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configgrpc"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/processor/processorhelper"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"

	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/buffer"
	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/cache"
	coord "pitr.ca/retroactivesampling/processor/retroactivesampling/internal/coordinator"
	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/evaluator"
	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/metadata"
)

type retroactiveProcessor struct {
	logger      *zap.Logger
	next        consumer.Traces
	buf         *buffer.SpanBuffer
	ic          *cache.InterestCache
	eval        evaluator.Evaluator
	coord       *coord.Client
	telemetry   *metadata.TelemetryBuilder
	grpcCfg     configgrpc.ClientConfig
	telSettings component.TelemetrySettings
}

func newProcessor(set component.TelemetrySettings, cfg *Config, next consumer.Traces) (*retroactiveProcessor, error) {
	tb, err := metadata.NewTelemetryBuilder(set)
	if err != nil {
		return nil, err
	}
	obs := func(d time.Duration) {
		tb.RetroactiveSamplingBufferSpanAgeOnEviction.Record(context.Background(), d.Milliseconds())
	}
	ic := cache.New(cfg.MaxInterestCacheEntries)
	buf, err := buffer.New(cfg.BufferFile, cfg.MaxBufferBytes, obs)
	if err != nil {
		tb.Shutdown()
		return nil, err
	}
	chain, err := evaluator.Build(set, cfg.Policies)
	if err != nil {
		_ = buf.Close()
		tb.Shutdown()
		return nil, err
	}
	if err := tb.RegisterRetroactiveSamplingBufferLiveBytesCallback(func(_ context.Context, o metric.Int64Observer) error {
		o.Observe(buf.LiveBytes())
		return nil
	}); err != nil {
		_ = buf.Close()
		tb.Shutdown()
		return nil, err
	}
	if err := tb.RegisterRetroactiveSamplingBufferOrphanedBytesCallback(func(_ context.Context, o metric.Int64Observer) error {
		o.Observe(buf.OrphanedBytes())
		return nil
	}); err != nil {
		_ = buf.Close()
		tb.Shutdown()
		return nil, err
	}
	return &retroactiveProcessor{
		logger:      set.Logger,
		next:        next,
		buf:         buf,
		ic:          ic,
		eval:        chain,
		telemetry:   tb,
		grpcCfg:     cfg.CoordinatorGRPC,
		telSettings: set,
	}, nil
}

func (p *retroactiveProcessor) Start(_ context.Context, host component.Host) error {
	p.coord = coord.New(p.grpcCfg, host, p.telSettings, p.onDecision, p.logger)
	return nil
}

func (p *retroactiveProcessor) Shutdown(_ context.Context) error {
	if p.coord != nil {
		p.coord.Close()
	}
	err := p.buf.Close()
	p.telemetry.Shutdown()
	return err
}

func (p *retroactiveProcessor) processTraces(_ context.Context, td ptrace.Traces) (ptrace.Traces, error) {
	out := ptrace.NewTraces()
	now := time.Now()
	for tid, spans := range groupByTrace(td) {
		if p.ic.Has(tid.String()) {
			spans.ResourceSpans().MoveAndAppendTo(out.ResourceSpans())
			continue
		}
		d, err := p.eval.Evaluate(spans)
		if err != nil {
			p.logger.Warn("policy evaluation error", zap.String("trace_id", tid.String()), zap.Error(err))
		}
		switch d {
		case evaluator.Sampled:
			p.ingestInteresting(tid, spans)
			continue
		case evaluator.SampledLocal:
			p.ingestLocal(tid, spans)
			continue
		}
		if err := p.buf.WriteWithEviction([16]byte(tid), spans, now); err != nil {
			p.logger.Error("buffer full after eviction", zap.String("trace_id", tid.String()), zap.Error(err))
			spans.ResourceSpans().MoveAndAppendTo(out.ResourceSpans())
			continue
		}
	}
	if out.SpanCount() == 0 {
		return out, processorhelper.ErrSkipProcessingData
	}
	return out, nil
}

func (p *retroactiveProcessor) ingestInteresting(tid pcommon.TraceID, current ptrace.Traces) {
	tidStr := tid.String()
	p.ic.Add(tidStr)
	if p.coord != nil {
		p.coord.Notify(tidStr)
	}
	p.ingestTrace(tid, current)
}

func (p *retroactiveProcessor) ingestLocal(tid pcommon.TraceID, current ptrace.Traces) {
	p.ic.Add(tid.String())
	p.ingestTrace(tid, current)
}

func (p *retroactiveProcessor) ingestTrace(tid pcommon.TraceID, current ptrace.Traces) {
	buffered, ok, err := p.buf.ReadAndDelete([16]byte(tid))
	if err != nil {
		p.logger.Warn("read buffer", zap.String("trace_id", tid.String()), zap.Error(err))
		if err2 := p.next.ConsumeTraces(context.Background(), current); err2 != nil {
			p.logger.Error("ingest trace", zap.String("trace_id", tid.String()), zap.Error(err2))
		}
		return
	}
	if ok {
		current.ResourceSpans().MoveAndAppendTo(buffered.ResourceSpans())
		current = buffered
	}
	if err := p.next.ConsumeTraces(context.Background(), current); err != nil {
		p.logger.Error("ingest trace", zap.String("trace_id", tid.String()), zap.Error(err))
	}
}

func (p *retroactiveProcessor) onDecision(traceID string) {
	p.logger.Debug("coordinator decision received", zap.String("trace_id", traceID))
	var raw [16]byte
	n, err := hex.Decode(raw[:], []byte(traceID))
	if err != nil || n != 16 {
		p.logger.Warn("coordinator decision: invalid trace ID", zap.String("trace_id", traceID), zap.Error(err))
		return
	}
	p.ic.Add(traceID)
	traces, ok, err := p.buf.ReadAndDelete(raw)
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
}
