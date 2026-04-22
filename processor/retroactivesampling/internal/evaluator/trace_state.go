package evaluator

import (
	"go.opentelemetry.io/collector/pdata/ptrace"
	oteltrace "go.opentelemetry.io/otel/trace"
)

type traceStateFilter struct {
	key     string
	matcher func(string) bool
}

func NewTraceStateFilter(key string, values []string) Evaluator {
	valuesMap := make(map[string]struct{})
	for _, v := range values {
		if v != "" && len(key)+len(v) < 256 {
			valuesMap[v] = struct{}{}
		}
	}
	return &traceStateFilter{
		key:     key,
		matcher: func(s string) bool { _, ok := valuesMap[s]; return ok },
	}
}

func (tsf *traceStateFilter) Evaluate(t ptrace.Traces) (Decision, error) {
	return hasSpanWithCondition(t, func(span ptrace.Span) bool {
		ts, err := oteltrace.ParseTraceState(span.TraceState().AsRaw())
		if err != nil {
			return false
		}
		return tsf.matcher(ts.Get(tsf.key))
	}), nil
}
