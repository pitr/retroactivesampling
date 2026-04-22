package evaluator_test

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"

	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/evaluator"
)

func makeTraceWithID(traceIDHex string) ptrace.Traces {
	td := ptrace.NewTraces()
	span := td.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	var tid pcommon.TraceID
	b, _ := hex.DecodeString(traceIDHex)
	copy(tid[:], b)
	span.SetTraceID(tid)
	return td
}

func TestProbabilistic_100Percent(t *testing.T) {
	ev := evaluator.NewProbabilisticSampler(zap.NewNop(), "", 100)
	d, err := ev.Evaluate(makeTraceWithID("aabbccdd11111111aabbccdd11111111"))
	require.NoError(t, err)
	assert.Equal(t, evaluator.SampledLocal, d)
}

func TestProbabilistic_0Percent(t *testing.T) {
	ev := evaluator.NewProbabilisticSampler(zap.NewNop(), "", 0)
	d, err := ev.Evaluate(makeTraceWithID("aabbccdd11111111aabbccdd11111111"))
	require.NoError(t, err)
	assert.Equal(t, evaluator.NotSampled, d)
}

func TestProbabilistic_EmptyTrace(t *testing.T) {
	ev := evaluator.NewProbabilisticSampler(zap.NewNop(), "", 100)
	d, err := ev.Evaluate(ptrace.NewTraces())
	require.NoError(t, err)
	assert.Equal(t, evaluator.NotSampled, d)
}

func TestProbabilistic_ReturnsSampledLocal(t *testing.T) {
	// 100% sampler must return SampledLocal, not Sampled.
	ev := evaluator.NewProbabilisticSampler(zap.NewNop(), "", 100)
	d, err := ev.Evaluate(makeTraceWithID("aabbccdd22222222aabbccdd22222222"))
	require.NoError(t, err)
	assert.Equal(t, evaluator.SampledLocal, d, "probabilistic must return SampledLocal to skip coordinator broadcast")
}
