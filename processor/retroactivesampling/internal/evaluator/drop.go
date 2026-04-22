package evaluator

import (
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"
)

type dropPolicy struct {
	subpolicies []Evaluator
	logger      *zap.Logger
}

func NewDrop(logger *zap.Logger, subpolicies []Evaluator) Evaluator {
	return &dropPolicy{subpolicies: subpolicies, logger: logger}
}

func (c *dropPolicy) Evaluate(t ptrace.Traces) (Decision, error) {
	ok, err := allSubsMatch(c.subpolicies, t)
	if !ok || err != nil {
		return NotSampled, err
	}
	return Dropped, nil
}
