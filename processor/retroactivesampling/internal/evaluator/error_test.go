package evaluator_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/evaluator"
)

func makeTraces(statusCode ptrace.StatusCode, durationMs int64) ptrace.Traces {
	td := ptrace.NewTraces()
	span := td.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	span.Status().SetCode(statusCode)
	now := time.Now()
	span.SetStartTimestamp(pcommon.NewTimestampFromTime(now))
	span.SetEndTimestamp(pcommon.NewTimestampFromTime(now.Add(time.Duration(durationMs) * time.Millisecond)))
	return td
}

func TestErrorEvaluator_ErrorSpan(t *testing.T) {
	e := &evaluator.ErrorEvaluator{}
	assert.True(t, e.Evaluate(makeTraces(ptrace.StatusCodeError, 10)))
}

func TestErrorEvaluator_OkSpan(t *testing.T) {
	e := &evaluator.ErrorEvaluator{}
	assert.False(t, e.Evaluate(makeTraces(ptrace.StatusCodeOk, 10)))
}
