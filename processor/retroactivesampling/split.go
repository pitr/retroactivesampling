package retroactivesampling

import (
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// groupByTrace splits a ptrace.Traces into per-trace-ID sub-Traces, preserving
// ResourceSpans and ScopeSpans structure.
func groupByTrace(td ptrace.Traces) map[pcommon.TraceID]ptrace.Traces {
	type rsKey struct {
		tid   pcommon.TraceID
		rsIdx int
	}
	type ssKey struct {
		tid         pcommon.TraceID
		rsIdx, ssIdx int
	}

	out := make(map[pcommon.TraceID]ptrace.Traces)
	rsMap := make(map[rsKey]ptrace.ResourceSpans)
	ssMap := make(map[ssKey]ptrace.ScopeSpans)

	for rsIdx, rs := range td.ResourceSpans().All() {
		for ssIdx, ss := range rs.ScopeSpans().All() {
			for _, span := range ss.Spans().All() {
				tid := span.TraceID()
				sk := ssKey{tid, rsIdx, ssIdx}
				outSS, ok := ssMap[sk]
				if !ok {
					t, ok := out[tid]
					if !ok {
						t = ptrace.NewTraces()
						out[tid] = t
					}
					rk := rsKey{tid, rsIdx}
					outRS, ok := rsMap[rk]
					if !ok {
						outRS = t.ResourceSpans().AppendEmpty()
						rs.Resource().CopyTo(outRS.Resource())
						outRS.SetSchemaUrl(rs.SchemaUrl())
						rsMap[rk] = outRS
					}
					outSS = outRS.ScopeSpans().AppendEmpty()
					ss.Scope().CopyTo(outSS.Scope())
					outSS.SetSchemaUrl(ss.SchemaUrl())
					ssMap[sk] = outSS
				}
				span.CopyTo(outSS.Spans().AppendEmpty())
			}
		}
	}

	return out
}
