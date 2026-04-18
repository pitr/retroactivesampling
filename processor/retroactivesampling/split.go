package processor

import "go.opentelemetry.io/collector/pdata/ptrace"

// groupByTrace splits a ptrace.Traces into per-trace-ID sub-Traces, preserving
// ResourceSpans and ScopeSpans structure.
func groupByTrace(td ptrace.Traces) map[string]ptrace.Traces {
	out := make(map[string]ptrace.Traces)
	for i := 0; i < td.ResourceSpans().Len(); i++ {
		rs := td.ResourceSpans().At(i)
		for j := 0; j < rs.ScopeSpans().Len(); j++ {
			ss := rs.ScopeSpans().At(j)
			for k := 0; k < ss.Spans().Len(); k++ {
				span := ss.Spans().At(k)
				tid := span.TraceID().String()

				t, ok := out[tid]
				if !ok {
					t = ptrace.NewTraces()
					out[tid] = t
				}

				// Find or create matching ResourceSpans
				newRS := findResourceSpans(t, rs)
				// Find or create matching ScopeSpans
				newSS := findScopeSpans(newRS, ss)
				span.CopyTo(newSS.Spans().AppendEmpty())
			}
		}
	}
	return out
}

func findResourceSpans(t ptrace.Traces, src ptrace.ResourceSpans) ptrace.ResourceSpans {
	for i := 0; i < t.ResourceSpans().Len(); i++ {
		rs := t.ResourceSpans().At(i)
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
	for i := 0; i < rs.ScopeSpans().Len(); i++ {
		ss := rs.ScopeSpans().At(i)
		if ss.Scope().Name() == src.Scope().Name() && ss.Scope().Version() == src.Scope().Version() {
			return ss
		}
	}
	ss := rs.ScopeSpans().AppendEmpty()
	src.Scope().CopyTo(ss.Scope())
	ss.SetSchemaUrl(src.SchemaUrl())
	return ss
}
