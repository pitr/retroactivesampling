package evaluator

import (
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"
)

type booleanAttributeFilter struct {
	key         string
	value       bool
	logger      *zap.Logger
	invertMatch bool
}

func NewBooleanAttributeFilter(logger *zap.Logger, key string, value, invertMatch bool) Evaluator {
	return &booleanAttributeFilter{key: key, value: value, logger: logger, invertMatch: invertMatch}
}

func (baf *booleanAttributeFilter) Evaluate(t ptrace.Traces) (Decision, error) {
	matches := func(v bool) bool { return v == baf.value }
	if baf.invertMatch {
		return invertHasResourceOrSpanWithCondition(t,
			func(r pcommon.Resource) bool {
				if v, ok := r.Attributes().Get(baf.key); ok {
					return !matches(v.Bool())
				}
				return true
			},
			func(span ptrace.Span) bool {
				if v, ok := span.Attributes().Get(baf.key); ok {
					return !matches(v.Bool())
				}
				return true
			},
		), nil
	}
	return hasResourceOrSpanWithCondition(t,
		func(r pcommon.Resource) bool {
			if v, ok := r.Attributes().Get(baf.key); ok {
				return matches(v.Bool())
			}
			return false
		},
		func(span ptrace.Span) bool {
			if v, ok := span.Attributes().Get(baf.key); ok {
				return matches(v.Bool())
			}
			return false
		},
	), nil
}
