package evaluator

import (
	"time"

	"go.opentelemetry.io/collector/pdata/ptrace"
)

type LatencyEvaluator struct {
	Threshold time.Duration
}

func (e *LatencyEvaluator) Evaluate(t ptrace.Traces) bool {
	for _, rs := range t.ResourceSpans().All() {
		for _, ss := range rs.ScopeSpans().All() {
			for _, span := range ss.Spans().All() {
				d := span.EndTimestamp().AsTime().Sub(span.StartTimestamp().AsTime())
				if d >= e.Threshold {
					return true
				}
			}
		}
	}
	return false
}
