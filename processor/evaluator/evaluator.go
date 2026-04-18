package evaluator

import "go.opentelemetry.io/collector/pdata/ptrace"

type Evaluator interface {
	Evaluate(ptrace.Traces) bool
}

// Chain evaluates each Evaluator in order; returns true on first match (OR logic).
type Chain []Evaluator

func (c Chain) Evaluate(t ptrace.Traces) bool {
	for _, e := range c {
		if e.Evaluate(t) {
			return true
		}
	}
	return false
}
