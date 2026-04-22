package evaluator

import (
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"
)

type alwaysSample struct{ logger *zap.Logger }

func NewAlwaysSample(logger *zap.Logger) Evaluator {
	return &alwaysSample{logger: logger}
}

func (as *alwaysSample) Evaluate(ptrace.Traces) (Decision, error) {
	as.logger.Debug("Evaluating spans in always-sample filter")
	return SampledLocal, nil
}
