package evaluator_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/evaluator"
)

func TestAnd_AllMatch(t *testing.T) {
	and := evaluator.NewAnd([]evaluator.Evaluator{
		stub{d: evaluator.Sampled},
		stub{d: evaluator.SampledLocal},
	})
	d, err := and.Evaluate(ptrace.NewTraces())
	require.NoError(t, err)
	assert.Equal(t, evaluator.Sampled, d)
}

func TestAnd_OneNotSampledShortCircuits(t *testing.T) {
	and := evaluator.NewAnd([]evaluator.Evaluator{
		stub{d: evaluator.Sampled},
		stub{d: evaluator.NotSampled},
		stub{d: evaluator.Sampled},
	})
	d, err := and.Evaluate(ptrace.NewTraces())
	require.NoError(t, err)
	assert.Equal(t, evaluator.NotSampled, d)
}

func TestAnd_NeverReturnsSampledLocal(t *testing.T) {
	and := evaluator.NewAnd([]evaluator.Evaluator{
		stub{d: evaluator.SampledLocal},
		stub{d: evaluator.SampledLocal},
	})
	d, err := and.Evaluate(ptrace.NewTraces())
	require.NoError(t, err)
	assert.Equal(t, evaluator.Sampled, d, "and must always return Sampled (never SampledLocal)")
}

func TestNot_InvertsSampled(t *testing.T) {
	not := evaluator.NewNot(stub{d: evaluator.Sampled})
	d, err := not.Evaluate(ptrace.NewTraces())
	require.NoError(t, err)
	assert.Equal(t, evaluator.NotSampled, d)
}

func TestNot_InvertsNotSampled(t *testing.T) {
	not := evaluator.NewNot(stub{d: evaluator.NotSampled})
	d, err := not.Evaluate(ptrace.NewTraces())
	require.NoError(t, err)
	assert.Equal(t, evaluator.Sampled, d)
}

func TestNot_InvertsSampledLocal(t *testing.T) {
	not := evaluator.NewNot(stub{d: evaluator.SampledLocal})
	d, err := not.Evaluate(ptrace.NewTraces())
	require.NoError(t, err)
	assert.Equal(t, evaluator.NotSampled, d)
}

func TestDrop_AllMatchDrops(t *testing.T) {
	drop := evaluator.NewDrop([]evaluator.Evaluator{
		stub{d: evaluator.Sampled},
		stub{d: evaluator.Sampled},
	})
	d, err := drop.Evaluate(ptrace.NewTraces())
	require.NoError(t, err)
	assert.Equal(t, evaluator.Dropped, d)
}

func TestDrop_OneNotSampledPasses(t *testing.T) {
	drop := evaluator.NewDrop([]evaluator.Evaluator{
		stub{d: evaluator.Sampled},
		stub{d: evaluator.NotSampled},
	})
	d, err := drop.Evaluate(ptrace.NewTraces())
	require.NoError(t, err)
	assert.Equal(t, evaluator.NotSampled, d)
}

func TestAnd_SubDropped_ReturnsError(t *testing.T) {
	and := evaluator.NewAnd([]evaluator.Evaluator{
		stub{d: evaluator.Sampled},
		stub{d: evaluator.Dropped},
	})
	_, err := and.Evaluate(ptrace.NewTraces())
	require.Error(t, err)
}

func TestNot_SubDropped_ReturnsError(t *testing.T) {
	not := evaluator.NewNot(stub{d: evaluator.Dropped})
	_, err := not.Evaluate(ptrace.NewTraces())
	require.Error(t, err)
}

func TestDrop_SubDropped_ReturnsError(t *testing.T) {
	drop := evaluator.NewDrop([]evaluator.Evaluator{
		stub{d: evaluator.Dropped},
	})
	_, err := drop.Evaluate(ptrace.NewTraces())
	require.Error(t, err)
}

func TestDrop_HaltsChain(t *testing.T) {
	drop := evaluator.NewDrop([]evaluator.Evaluator{stub{d: evaluator.Sampled}})
	c := evaluator.Chain{drop, stub{d: evaluator.Sampled}}
	d, err := c.Evaluate(ptrace.NewTraces())
	require.NoError(t, err)
	assert.Equal(t, evaluator.Dropped, d)
}
