package evaluator

import (
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"
)

type booleanAttributeFilter struct {
	key    string
	value  bool
	logger *zap.Logger
}

func NewBooleanAttributeFilter(logger *zap.Logger, key string, value bool) Evaluator {
	return &booleanAttributeFilter{key: key, value: value, logger: logger}
}

func (baf *booleanAttributeFilter) Evaluate(t ptrace.Traces) (Decision, error) {
	return hasResourceOrSpanWithCondition(t,
		func(r pcommon.Resource) bool {
			v, ok := r.Attributes().Get(baf.key)
			return ok && v.Bool() == baf.value
		},
		func(span ptrace.Span) bool {
			v, ok := span.Attributes().Get(baf.key)
			return ok && v.Bool() == baf.value
		},
	), nil
}
