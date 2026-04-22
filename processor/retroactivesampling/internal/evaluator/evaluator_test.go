package evaluator_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/evaluator"
)

type stub struct{ d evaluator.Decision; err error }

func (s stub) Evaluate(ptrace.Traces) (evaluator.Decision, error) { return s.d, s.err }

func TestChain_FirstSampledWins(t *testing.T) {
	c := evaluator.Chain{stub{d: evaluator.NotSampled}, stub{d: evaluator.Sampled}, stub{d: evaluator.NotSampled}}
	d, err := c.Evaluate(ptrace.NewTraces())
	require.NoError(t, err)
	assert.Equal(t, evaluator.Sampled, d)
}

func TestChain_SampledLocalWins(t *testing.T) {
	c := evaluator.Chain{stub{d: evaluator.NotSampled}, stub{d: evaluator.SampledLocal}}
	d, err := c.Evaluate(ptrace.NewTraces())
	require.NoError(t, err)
	assert.Equal(t, evaluator.SampledLocal, d)
}

func TestChain_DroppedHaltsBeforeSampled(t *testing.T) {
	c := evaluator.Chain{stub{d: evaluator.Dropped}, stub{d: evaluator.Sampled}}
	d, err := c.Evaluate(ptrace.NewTraces())
	require.NoError(t, err)
	assert.Equal(t, evaluator.Dropped, d)
}

func TestChain_ErrorHalts(t *testing.T) {
	boom := errors.New("boom")
	c := evaluator.Chain{stub{err: boom}, stub{d: evaluator.Sampled}}
	d, err := c.Evaluate(ptrace.NewTraces())
	assert.Equal(t, evaluator.NotSampled, d)
	assert.ErrorIs(t, err, boom)
}

func TestChain_AllNotSampled(t *testing.T) {
	c := evaluator.Chain{stub{d: evaluator.NotSampled}, stub{d: evaluator.NotSampled}}
	d, err := c.Evaluate(ptrace.NewTraces())
	require.NoError(t, err)
	assert.Equal(t, evaluator.NotSampled, d)
}

func TestChain_Empty(t *testing.T) {
	var c evaluator.Chain
	d, err := c.Evaluate(ptrace.NewTraces())
	require.NoError(t, err)
	assert.Equal(t, evaluator.NotSampled, d)
}
