package evaluator

import "go.opentelemetry.io/collector/pdata/ptrace"

type ErrorEvaluator struct{}

func (e *ErrorEvaluator) Evaluate(t ptrace.Traces) bool {
	for _, rs := range t.ResourceSpans().All() {
		for _, ss := range rs.ScopeSpans().All() {
			for _, span := range ss.Spans().All() {
				if span.Status().Code() == ptrace.StatusCodeError {
					return true
				}
			}
		}
	}
	return false
}
