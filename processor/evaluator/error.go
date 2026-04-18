package evaluator

import "go.opentelemetry.io/collector/pdata/ptrace"

type ErrorEvaluator struct{}

func (e *ErrorEvaluator) Evaluate(t ptrace.Traces) bool {
	for i := 0; i < t.ResourceSpans().Len(); i++ {
		for j := 0; j < t.ResourceSpans().At(i).ScopeSpans().Len(); j++ {
			ss := t.ResourceSpans().At(i).ScopeSpans().At(j)
			for k := 0; k < ss.Spans().Len(); k++ {
				if ss.Spans().At(k).Status().Code() == ptrace.StatusCodeError {
					return true
				}
			}
		}
	}
	return false
}
