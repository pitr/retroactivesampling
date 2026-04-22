package evaluator

import "go.opentelemetry.io/collector/pdata/ptrace"

type notPolicy struct {
	subPolicyEvaluator Evaluator
}

func NewNot(subPolicyEvaluator Evaluator) Evaluator {
	return &notPolicy{subPolicyEvaluator: subPolicyEvaluator}
}

func (n *notPolicy) Evaluate(t ptrace.Traces) (Decision, error) {
	d, err := n.subPolicyEvaluator.Evaluate(t)
	if err != nil {
		return NotSampled, err
	}
	switch d {
	case Sampled, SampledLocal:
		return NotSampled, nil
	case NotSampled:
		return Sampled, nil
	default:
		return d, nil
	}
}
