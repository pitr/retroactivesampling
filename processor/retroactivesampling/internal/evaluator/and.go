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

// Evaluate returns Sampled if all sub-policies return non-NotSampled.
// Always returns Sampled (never SampledLocal) — AND result depends on
// which spans were seen, so coordinator notification is always needed.
func (c *andPolicy) Evaluate(t ptrace.Traces) (Decision, error) {
	for _, sub := range c.subpolicies {
		d, err := sub.Evaluate(t)
		if err != nil {
			return NotSampled, err
		}
		if d == NotSampled {
			return NotSampled, nil
		}
	}
	return Sampled, nil
}
