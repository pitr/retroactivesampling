package evaluator_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"

	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/evaluator"
)

func TestLatency_AboveThreshold(t *testing.T) {
	ev := evaluator.NewLatency(zap.NewNop(), 5000, 0)
	d, err := ev.Evaluate(makeSpan(ptrace.StatusCodeOk, 6000))
	require.NoError(t, err)
	assert.Equal(t, evaluator.Sampled, d)
}

func TestLatency_BelowThreshold(t *testing.T) {
	ev := evaluator.NewLatency(zap.NewNop(), 5000, 0)
	d, err := ev.Evaluate(makeSpan(ptrace.StatusCodeOk, 1000))
	require.NoError(t, err)
	assert.Equal(t, evaluator.NotSampled, d)
}

func TestLatency_AtThreshold(t *testing.T) {
	ev := evaluator.NewLatency(zap.NewNop(), 5000, 0)
	d, err := ev.Evaluate(makeSpan(ptrace.StatusCodeOk, 5000))
	require.NoError(t, err)
	assert.Equal(t, evaluator.Sampled, d)
}

func TestLatency_UpperBound(t *testing.T) {
	ev := evaluator.NewLatency(zap.NewNop(), 1000, 3000)
	d, err := ev.Evaluate(makeSpan(ptrace.StatusCodeOk, 2000))
	require.NoError(t, err)
	assert.Equal(t, evaluator.Sampled, d)
}

func TestLatency_AboveUpperBound(t *testing.T) {
	ev := evaluator.NewLatency(zap.NewNop(), 1000, 3000)
	d, err := ev.Evaluate(makeSpan(ptrace.StatusCodeOk, 5000))
	require.NoError(t, err)
	assert.Equal(t, evaluator.NotSampled, d)
}
