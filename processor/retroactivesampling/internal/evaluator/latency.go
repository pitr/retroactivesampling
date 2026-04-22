package evaluator

import (
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"
)

type latency struct {
	logger           *zap.Logger
	thresholdMs      int64
	upperThresholdMs int64
}

func NewLatency(logger *zap.Logger, thresholdMs, upperThresholdMs int64) Evaluator {
	return &latency{logger: logger, thresholdMs: thresholdMs, upperThresholdMs: upperThresholdMs}
}

func (l *latency) Evaluate(t ptrace.Traces) (Decision, error) {
	l.logger.Debug("Evaluating spans in latency filter")
	var minTime, maxTime pcommon.Timestamp
	return hasSpanWithCondition(t, func(span ptrace.Span) bool {
		if minTime == 0 {
			minTime, maxTime = span.StartTimestamp(), span.EndTimestamp()
		} else {
			minTime = min(minTime, span.StartTimestamp())
			maxTime = max(maxTime, span.EndTimestamp())
		}
		d := maxTime.AsTime().Sub(minTime.AsTime())
		if l.upperThresholdMs == 0 {
			return d.Milliseconds() >= l.thresholdMs
		}
		return l.thresholdMs < d.Milliseconds() && d.Milliseconds() <= l.upperThresholdMs
	}), nil
}
