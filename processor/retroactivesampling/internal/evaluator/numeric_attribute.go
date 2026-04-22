package evaluator

import (
	"math"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"
)

type numericAttributeFilter struct {
	key         string
	minValue    *int64
	maxValue    *int64
	logger      *zap.Logger
	invertMatch bool
}

func NewNumericAttributeFilter(logger *zap.Logger, key string, minValue, maxValue *int64, invertMatch bool) Evaluator {
	return &numericAttributeFilter{key: key, minValue: minValue, maxValue: maxValue, logger: logger, invertMatch: invertMatch}
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

	if naf.invertMatch {
		return invertHasResourceOrSpanWithCondition(t,
			func(r pcommon.Resource) bool {
				if v, ok := r.Attributes().Get(naf.key); ok {
					return !inRange(v.Int())
				}
				return true
			},
			func(span ptrace.Span) bool {
				if v, ok := span.Attributes().Get(naf.key); ok {
					return !inRange(v.Int())
				}
				return true
			},
		), nil
	}
	return hasResourceOrSpanWithCondition(t,
		func(r pcommon.Resource) bool {
			if v, ok := r.Attributes().Get(naf.key); ok {
				return inRange(v.Int())
			}
			return false
		},
		func(span ptrace.Span) bool {
			if v, ok := span.Attributes().Get(naf.key); ok {
				return inRange(v.Int())
			}
			return false
		},
	), nil
}
