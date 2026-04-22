package evaluator

import "go.opentelemetry.io/collector/pdata/ptrace"

type Decision uint8

const (
	NotSampled  Decision = iota
	Sampled              // interesting — notify coordinator
	SampledLocal         // interesting — skip coordinator; all collectors make the same decision
	Dropped              // halt chain, do not sample
)

type Evaluator interface {
	Evaluate(ptrace.Traces) (Decision, error)
}

// Chain evaluates policies in order; halts on first non-NotSampled or error.
type Chain []Evaluator

func (c Chain) Evaluate(t ptrace.Traces) (Decision, error) {
	for _, e := range c {
		d, err := e.Evaluate(t)
		if err != nil {
			return NotSampled, err
		}
		if d != NotSampled {
			return d, nil
		}
	}
	return NotSampled, nil
}
