package retroactivesampling

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

func makeTraceID(b byte) pcommon.TraceID {
	var id pcommon.TraceID
	id[0] = b
	return id
}

func makeSpan(traceID pcommon.TraceID, name string) ptrace.Span {
	s := ptrace.NewSpan()
	s.SetTraceID(traceID)
	s.SetName(name)
	return s
}

// buildTraces creates a ptrace.Traces with one ResourceSpans, one ScopeSpans,
// and the given spans appended.
func buildTraces(resourceAttr string, scopeName string, spans []ptrace.Span) ptrace.Traces {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", resourceAttr)
	ss := rs.ScopeSpans().AppendEmpty()
	ss.Scope().SetName(scopeName)
	for _, sp := range spans {
		sp.CopyTo(ss.Spans().AppendEmpty())
	}
	return td
}

func TestGroupByTrace_SingleSpan(t *testing.T) {
	tid := makeTraceID(1)
	td := buildTraces("svc-a", "scope-a", []ptrace.Span{makeSpan(tid, "op")})

	out := groupByTrace(td)

	require.Len(t, out, 1)
	traces, ok := out[tid.String()]
	require.True(t, ok)
	assert.Equal(t, 1, traces.SpanCount())
}

func TestGroupByTrace_DifferentTraces(t *testing.T) {
	t1, t2 := makeTraceID(1), makeTraceID(2)
	td := buildTraces("svc-a", "scope-a", []ptrace.Span{
		makeSpan(t1, "op1"),
		makeSpan(t2, "op2"),
	})

	out := groupByTrace(td)

	require.Len(t, out, 2)
	assert.Equal(t, 1, out[t1.String()].SpanCount())
	assert.Equal(t, 1, out[t2.String()].SpanCount())
}

func TestGroupByTrace_SameTrace_MergesSpans(t *testing.T) {
	tid := makeTraceID(1)
	td := buildTraces("svc-a", "scope-a", []ptrace.Span{
		makeSpan(tid, "op1"),
		makeSpan(tid, "op2"),
	})

	out := groupByTrace(td)

	require.Len(t, out, 1)
	assert.Equal(t, 2, out[tid.String()].SpanCount())
}

func TestGroupByTrace_PreservesResourceAndScope(t *testing.T) {
	tid := makeTraceID(1)
	td := buildTraces("svc-a", "scope-a", []ptrace.Span{makeSpan(tid, "op")})

	out := groupByTrace(td)

	traces := out[tid.String()]
	require.Equal(t, 1, traces.ResourceSpans().Len())
	rs := traces.ResourceSpans().At(0)
	v, ok := rs.Resource().Attributes().Get("service.name")
	require.True(t, ok)
	assert.Equal(t, "svc-a", v.Str())
	require.Equal(t, 1, rs.ScopeSpans().Len())
	assert.Equal(t, "scope-a", rs.ScopeSpans().At(0).Scope().Name())
}

func TestGroupByTrace_MultipleResources_SameTrace(t *testing.T) {
	tid := makeTraceID(1)

	td := ptrace.NewTraces()
	for _, svc := range []string{"svc-a", "svc-b"} {
		rs := td.ResourceSpans().AppendEmpty()
		rs.Resource().Attributes().PutStr("service.name", svc)
		ss := rs.ScopeSpans().AppendEmpty()
		ss.Scope().SetName("scope")
		makeSpan(tid, "op").CopyTo(ss.Spans().AppendEmpty())
	}

	out := groupByTrace(td)

	require.Len(t, out, 1)
	traces := out[tid.String()]
	assert.Equal(t, 2, traces.ResourceSpans().Len())
	assert.Equal(t, 2, traces.SpanCount())
}
