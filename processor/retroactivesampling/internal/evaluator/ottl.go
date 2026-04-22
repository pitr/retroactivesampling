package evaluator

import (
	"context"
	"errors"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlspan"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlspanevent"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/ottlfuncs"
)

type ottlConditionFilter struct {
	sampleSpanExpr      *ottl.ConditionSequence[*ottlspan.TransformContext]
	sampleSpanEventExpr *ottl.ConditionSequence[*ottlspanevent.TransformContext]
}

func newBoolExprForSpan(conditions []string, errMode ottl.ErrorMode, set component.TelemetrySettings) (*ottl.ConditionSequence[*ottlspan.TransformContext], error) {
	funcs := ottlfuncs.StandardConverters[*ottlspan.TransformContext]()
	isRoot := ottlfuncs.NewIsRootSpanFactoryNew()
	funcs[isRoot.Name()] = isRoot
	parser, err := ottlspan.NewParser(funcs, set)
	if err != nil {
		return nil, err
	}
	conds, err := parser.ParseConditions(conditions)
	if err != nil {
		return nil, err
	}
	seq := ottlspan.NewConditionSequence(conds, set, ottlspan.WithConditionSequenceErrorMode(errMode))
	return &seq, nil
}

func newBoolExprForSpanEvent(conditions []string, errMode ottl.ErrorMode, set component.TelemetrySettings) (*ottl.ConditionSequence[*ottlspanevent.TransformContext], error) {
	funcs := ottlfuncs.StandardConverters[*ottlspanevent.TransformContext]()
	parser, err := ottlspanevent.NewParser(funcs, set)
	if err != nil {
		return nil, err
	}
	conds, err := parser.ParseConditions(conditions)
	if err != nil {
		return nil, err
	}
	seq := ottlspanevent.NewConditionSequence(conds, set, ottlspanevent.WithConditionSequenceErrorMode(errMode))
	return &seq, nil
}

func NewOTTLConditionFilter(settings component.TelemetrySettings, spanConditions, spanEventConditions []string, errMode ottl.ErrorMode) (Evaluator, error) {
	if len(spanConditions) == 0 && len(spanEventConditions) == 0 {
		return nil, errors.New("expected at least one OTTL condition to filter on")
	}
	f := &ottlConditionFilter{}
	var err error
	if len(spanConditions) > 0 {
		if f.sampleSpanExpr, err = newBoolExprForSpan(spanConditions, errMode, settings); err != nil {
			return nil, err
		}
	}
	if len(spanEventConditions) > 0 {
		if f.sampleSpanEventExpr, err = newBoolExprForSpanEvent(spanEventConditions, errMode, settings); err != nil {
			return nil, err
		}
	}
	return f, nil
}

func (ocf *ottlConditionFilter) Evaluate(t ptrace.Traces) (Decision, error) {
	ctx := context.Background()
	for _, rs := range t.ResourceSpans().All() {
		for _, ss := range rs.ScopeSpans().All() {
			for _, span := range ss.Spans().All() {
				if ocf.sampleSpanExpr != nil {
					tCtx := ottlspan.NewTransformContextPtr(rs, ss, span)
					ok, err := ocf.sampleSpanExpr.Eval(ctx, tCtx)
					tCtx.Close()
					if err != nil {
						return NotSampled, err
					}
					if ok {
						return Sampled, nil
					}
				}
				if ocf.sampleSpanEventExpr != nil {
					for _, spanEvent := range span.Events().All() {
						tCtx := ottlspanevent.NewTransformContextPtr(rs, ss, span, spanEvent)
						ok, err := ocf.sampleSpanEventExpr.Eval(ctx, tCtx)
						tCtx.Close()
						if err != nil {
							return NotSampled, err
						}
						if ok {
							return Sampled, nil
						}
					}
				}
			}
		}
	}
	return NotSampled, nil
}
