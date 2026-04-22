package evaluator

import "go.opentelemetry.io/collector/pdata/ptrace"

type dropPolicy struct {
	subpolicies []Evaluator
}

func NewDrop(subpolicies []Evaluator) Evaluator {
	return &dropPolicy{subpolicies: subpolicies}
}

func (c *dropPolicy) Evaluate(t ptrace.Traces) (Decision, error) {
	ok, err := allSubsMatch(c.subpolicies, t)
	if !ok || err != nil {
		return NotSampled, err
	}
	return Dropped, nil
}
