package evaluator

import (
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"
)

type andPolicy struct {
	subpolicies []Evaluator
	logger      *zap.Logger
}

func NewAnd(logger *zap.Logger, subpolicies []Evaluator) Evaluator {
	return &andPolicy{subpolicies: subpolicies, logger: logger}
}

// Always returns Sampled (never SampledLocal) — AND result depends on
// which spans were seen, so coordinator notification is always needed.
func (c *andPolicy) Evaluate(t ptrace.Traces) (Decision, error) {
	ok, err := allSubsMatch(c.subpolicies, t)
	if !ok || err != nil {
		return NotSampled, err
	}
	return Sampled, nil
}
