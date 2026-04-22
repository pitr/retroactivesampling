package evaluator

import (
	"go.opentelemetry.io/collector/pdata/ptrace"
	oteltrace "go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

type traceFlags struct{ logger *zap.Logger }

func NewTraceFlags(logger *zap.Logger) Evaluator {
	return &traceFlags{logger: logger}
}

func (tf *traceFlags) Evaluate(t ptrace.Traces) (Decision, error) {
	tf.logger.Debug("Evaluating spans in trace-flags filter")
	return hasSpanWithCondition(t, func(span ptrace.Span) bool {
		return (byte(span.Flags()) & byte(oteltrace.FlagsSampled)) != 0
	}), nil
}
