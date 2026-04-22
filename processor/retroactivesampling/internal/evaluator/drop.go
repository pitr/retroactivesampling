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

// Evaluate returns Dropped (halting the chain) if all sub-policies match.
// Returns NotSampled (continuing the chain) if any sub-policy does not match.
func (c *dropPolicy) Evaluate(t ptrace.Traces) (Decision, error) {
	for _, sub := range c.subpolicies {
		d, err := sub.Evaluate(t)
		if err != nil {
			return NotSampled, err
		}
		if d == NotSampled {
			return NotSampled, nil
		}
	}
	return Dropped, nil
}
