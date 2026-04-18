package evaluator

import (
	"time"

	"go.opentelemetry.io/collector/pdata/ptrace"
)

type LatencyEvaluator struct {
	Threshold time.Duration
}

func (e *LatencyEvaluator) Evaluate(t ptrace.Traces) bool {
	for i := 0; i < t.ResourceSpans().Len(); i++ {
		for j := 0; j < t.ResourceSpans().At(i).ScopeSpans().Len(); j++ {
			ss := t.ResourceSpans().At(i).ScopeSpans().At(j)
			for k := 0; k < ss.Spans().Len(); k++ {
				span := ss.Spans().At(k)
				d := span.EndTimestamp().AsTime().Sub(span.StartTimestamp().AsTime())
				if d >= e.Threshold {
					return true
				}
			}
		}
	}
	return false
}
