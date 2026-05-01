package retroactivesampling

import (
	"context"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configgrpc"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/processor/processorhelper"
	"go.uber.org/zap"

	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/buffer"
	coord "pitr.ca/retroactivesampling/processor/retroactivesampling/internal/coordinator"
	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/evaluator"
	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/metadata"
)

type retroactiveProcessor struct {
	logger      *zap.Logger
	next        consumer.Traces
	buf         *buffer.SpanBuffer
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

	p := &retroactiveProcessor{
		logger:      set.Logger,
		next:        next,
		telemetry:   tb,
		grpcCfg:     cfg.CoordinatorGRPC,
		telSettings: set,
	}

	buf, err := buffer.New(
		cfg.BufferFile,
		cfg.MaxBufferBytes,
		cfg.ChunkSize,
		cfg.DecisionWaitTime,
		p.onMatch,
		func(d time.Duration) {
			tb.RetroactiveSamplingBufferSpanAgeOnEviction.Record(context.Background(), int64(d.Seconds()))
		},
	)
	if err != nil {
		tb.Shutdown()
		return nil, err
	}
	p.buf = buf

	p.eval, err = evaluator.Build(set, cfg.Policies)
	if err != nil {
		_ = buf.Close()
		tb.Shutdown()
		return nil, err
	}
	return p, nil
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
		if p.buf.HasInterest(tid) {
			spans.ResourceSpans().MoveAndAppendTo(out.ResourceSpans())
			continue
		}
		d, err := p.eval.Evaluate(spans)
		if err != nil {
			p.logger.Warn("policy evaluation error", zap.Stringer("trace_id", tid), zap.Error(err))
		}
		switch d {
		case evaluator.Sampled:
			spans.ResourceSpans().MoveAndAppendTo(out.ResourceSpans())
			p.buf.AddInterest(tid)
			if p.coord != nil {
				p.coord.Notify(tid)
			}
			continue
		case evaluator.SampledLocal:
			spans.ResourceSpans().MoveAndAppendTo(out.ResourceSpans())
			p.buf.AddInterest(tid)
			continue
		}
		m := ptrace.ProtoMarshaler{}
		data, err := m.MarshalTraces(spans)
		if err != nil {
			p.logger.Error("marshal spans for buffer, dropping", zap.Stringer("trace_id", tid), zap.Error(err))
			continue
		}
		if err := p.buf.Write(tid, data, now); err != nil {
			p.logger.Error("could not store trace locally, dropping", zap.Stringer("trace_id", tid), zap.Error(err))
		}
	}
	if err := p.buf.Flush(); err != nil {
		p.logger.Error("buffer flush failed", zap.Error(err))
	}
	if out.SpanCount() == 0 {
		return out, processorhelper.ErrSkipProcessingData
	}
	return out, nil
}

func (p *retroactiveProcessor) onMatch(traceID pcommon.TraceID, data []byte) {
	u := ptrace.ProtoUnmarshaler{}
	t, err := u.UnmarshalTraces(data)
	if err != nil {
		p.logger.Warn("unmarshal buffered trace", zap.Stringer("trace_id", traceID), zap.Error(err))
		return
	}
	if err := p.next.ConsumeTraces(context.Background(), t); err != nil {
		p.logger.Error("could not send spans from local disk", zap.Stringer("trace_id", traceID), zap.Error(err))
	}
}

func (p *retroactiveProcessor) onDecision(traceID pcommon.TraceID) {
	p.logger.Debug("coordinator decision received", zap.Stringer("trace_id", traceID))
	p.buf.AddInterest(traceID)
}
