package evaluator

import "go.opentelemetry.io/collector/pdata/ptrace"

type andPolicy struct {
	subpolicies []Evaluator
}

func NewAnd(subpolicies []Evaluator) Evaluator {
	return &andPolicy{subpolicies: subpolicies}
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
