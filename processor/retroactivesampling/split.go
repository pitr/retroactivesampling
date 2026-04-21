package retroactivesampling

import "go.opentelemetry.io/collector/pdata/ptrace"

// groupByTrace splits a ptrace.Traces into per-trace-ID sub-Traces, preserving
// ResourceSpans and ScopeSpans structure.
func groupByTrace(td ptrace.Traces) map[string]ptrace.Traces {
	out := make(map[string]ptrace.Traces)
	for _, rs := range td.ResourceSpans().All() {
		for _, ss := range rs.ScopeSpans().All() {
			for _, span := range ss.Spans().All() {
				tid := span.TraceID().String()
				t, ok := out[tid]
				if !ok {
					t = ptrace.NewTraces()
					out[tid] = t
				}
				newRS := findResourceSpans(t, rs)
				newSS := findScopeSpans(newRS, ss)
				span.CopyTo(newSS.Spans().AppendEmpty())
			}
		}
	}
	return out
}

func findResourceSpans(t ptrace.Traces, src ptrace.ResourceSpans) ptrace.ResourceSpans {
	for _, rs := range t.ResourceSpans().All() {
		if rs.Resource().Attributes().Equal(src.Resource().Attributes()) {
			return rs
		}
	}
	rs := t.ResourceSpans().AppendEmpty()
	src.Resource().CopyTo(rs.Resource())
	rs.SetSchemaUrl(src.SchemaUrl())
	return rs
}

func findScopeSpans(rs ptrace.ResourceSpans, src ptrace.ScopeSpans) ptrace.ScopeSpans {
	for _, ss := range rs.ScopeSpans().All() {
		if ss.Scope().Name() == src.Scope().Name() && ss.Scope().Version() == src.Scope().Version() {
			return ss
		}
	}
	ss := rs.ScopeSpans().AppendEmpty()
	src.Scope().CopyTo(ss.Scope())
	ss.SetSchemaUrl(src.SchemaUrl())
	return ss
}
