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
	for i := 0; i < td.ResourceSpans().Len(); i++ {
		rs := td.ResourceSpans().At(i)
		if shouldSampleResource(rs.Resource()) {
			return Sampled
		}
		if hasInstrumentationLibrarySpanWithCondition(rs.ScopeSpans(), shouldSampleSpan, false) {
			return Sampled
		}
	}
	return NotSampled
}

// invertHasResourceOrSpanWithCondition returns Sampled when the condition is absent/non-matching
// on every resource and span. Used for invert_match policies.
func invertHasResourceOrSpanWithCondition(
	td ptrace.Traces,
	shouldSampleResource func(pcommon.Resource) bool,
	shouldSampleSpan func(ptrace.Span) bool,
) Decision {
	for i := 0; i < td.ResourceSpans().Len(); i++ {
		rs := td.ResourceSpans().At(i)
		if !shouldSampleResource(rs.Resource()) {
			return NotSampled
		}
		if !hasInstrumentationLibrarySpanWithCondition(rs.ScopeSpans(), shouldSampleSpan, true) {
			return NotSampled
		}
	}
	return Sampled
}

func hasSpanWithCondition(td ptrace.Traces, shouldSample func(ptrace.Span) bool) Decision {
	for i := 0; i < td.ResourceSpans().Len(); i++ {
		if hasInstrumentationLibrarySpanWithCondition(td.ResourceSpans().At(i).ScopeSpans(), shouldSample, false) {
			return Sampled
		}
	}
	return NotSampled
}

func hasInstrumentationLibrarySpanWithCondition(ilss ptrace.ScopeSpansSlice, check func(ptrace.Span) bool, invert bool) bool {
	for i := 0; i < ilss.Len(); i++ {
		ils := ilss.At(i)
		for j := 0; j < ils.Spans().Len(); j++ {
			if r := check(ils.Spans().At(j)); r != invert {
				return r
			}
		}
	}
	return invert
}
