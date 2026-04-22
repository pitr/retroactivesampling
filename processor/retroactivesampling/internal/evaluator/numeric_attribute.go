package evaluator

import (
	"math"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

type numericAttributeFilter struct {
	key      string
	minValue *int64
	maxValue *int64
}

func NewNumericAttributeFilter(key string, minValue, maxValue *int64) Evaluator {
	return &numericAttributeFilter{key: key, minValue: minValue, maxValue: maxValue}
}

func (naf *numericAttributeFilter) Evaluate(t ptrace.Traces) (Decision, error) {
	minVal := int64(math.MinInt64)
	if naf.minValue != nil {
		minVal = *naf.minValue
	}
	maxVal := int64(math.MaxInt64)
	if naf.maxValue != nil {
		maxVal = *naf.maxValue
	}
	inRange := func(v int64) bool { return v >= minVal && v <= maxVal }
	return hasResourceOrSpanWithCondition(t,
		func(r pcommon.Resource) bool {
			v, ok := r.Attributes().Get(naf.key)
			return ok && inRange(v.Int())
		},
		func(span ptrace.Span) bool {
			v, ok := span.Attributes().Get(naf.key)
			return ok && inRange(v.Int())
		},
	), nil
}
