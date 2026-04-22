package evaluator

import (
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

func hasResourceOrSpanWithCondition(
	td ptrace.Traces,
	shouldSampleResource func(pcommon.Resource) bool,
	shouldSampleSpan func(ptrace.Span) bool,
) Decision {
	for _, rs := range td.ResourceSpans().All() {
		if shouldSampleResource(rs.Resource()) {
			return Sampled
		}
		for _, ss := range rs.ScopeSpans().All() {
			for _, span := range ss.Spans().All() {
				if shouldSampleSpan(span) {
					return Sampled
				}
			}
		}
	}
	return NotSampled
}

func hasSpanWithCondition(td ptrace.Traces, shouldSample func(ptrace.Span) bool) Decision {
	for _, rs := range td.ResourceSpans().All() {
		for _, ss := range rs.ScopeSpans().All() {
			for _, span := range ss.Spans().All() {
				if shouldSample(span) {
					return Sampled
				}
			}
		}
	}
	return NotSampled
}
