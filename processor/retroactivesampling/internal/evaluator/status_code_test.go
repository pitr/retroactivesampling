package evaluator_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"

	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/evaluator"
)

func makeSpan(status ptrace.StatusCode, durationMs int64) ptrace.Traces {
	td := ptrace.NewTraces()
	span := td.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	span.Status().SetCode(status)
	now := time.Now()
	span.SetStartTimestamp(pcommon.NewTimestampFromTime(now))
	span.SetEndTimestamp(pcommon.NewTimestampFromTime(now.Add(time.Duration(durationMs) * time.Millisecond)))
	return td
}

func TestStatusCodeFilter_ErrorMatch(t *testing.T) {
	ev, err := evaluator.NewStatusCodeFilter(zap.NewNop(), []string{"ERROR"})
	require.NoError(t, err)
	d, err := ev.Evaluate(makeSpan(ptrace.StatusCodeError, 10))
	require.NoError(t, err)
	assert.Equal(t, evaluator.Sampled, d)
}

func TestStatusCodeFilter_OkNoMatch(t *testing.T) {
	ev, err := evaluator.NewStatusCodeFilter(zap.NewNop(), []string{"ERROR"})
	require.NoError(t, err)
	d, err := ev.Evaluate(makeSpan(ptrace.StatusCodeOk, 10))
	require.NoError(t, err)
	assert.Equal(t, evaluator.NotSampled, d)
}

func TestStatusCodeFilter_MultipleCodesMatch(t *testing.T) {
	ev, err := evaluator.NewStatusCodeFilter(zap.NewNop(), []string{"ERROR", "UNSET"})
	require.NoError(t, err)
	d, err := ev.Evaluate(makeSpan(ptrace.StatusCodeUnset, 10))
	require.NoError(t, err)
	assert.Equal(t, evaluator.Sampled, d)
}

func TestStatusCodeFilter_UnknownCodeError(t *testing.T) {
	_, err := evaluator.NewStatusCodeFilter(zap.NewNop(), []string{"INVALID"})
	assert.Error(t, err)
}

func TestStatusCodeFilter_EmptyError(t *testing.T) {
	_, err := evaluator.NewStatusCodeFilter(zap.NewNop(), []string{})
	assert.Error(t, err)
}
