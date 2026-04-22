package evaluator

import (
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"
)

type notPolicy struct {
	logger             *zap.Logger
	subPolicyEvaluator Evaluator
}

func NewNot(logger *zap.Logger, subPolicyEvaluator Evaluator) Evaluator {
	return &notPolicy{logger: logger, subPolicyEvaluator: subPolicyEvaluator}
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
