package evaluator_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/evaluator"
)

func policy(typ evaluator.PolicyType) evaluator.PolicyCfg {
	p := evaluator.PolicyCfg{}
	p.Name = string(typ)
	p.Type = typ
	return p
}

func TestBuild_AlwaysSample(t *testing.T) {
	chain, err := evaluator.Build(componenttest.NewNopTelemetrySettings(), []evaluator.PolicyCfg{policy(evaluator.AlwaysSample)})
	require.NoError(t, err)
	d, err := chain.Evaluate(ptrace.NewTraces())
	require.NoError(t, err)
	assert.Equal(t, evaluator.SampledLocal, d)
}

func TestBuild_StatusCode(t *testing.T) {
	p := policy(evaluator.StatusCode)
	p.StatusCodeCfg = evaluator.StatusCodeCfg{StatusCodes: []string{"ERROR"}}
	chain, err := evaluator.Build(componenttest.NewNopTelemetrySettings(), []evaluator.PolicyCfg{p})
	require.NoError(t, err)
	d, err := chain.Evaluate(makeSpan(ptrace.StatusCodeError, 100))
	require.NoError(t, err)
	assert.Equal(t, evaluator.Sampled, d)
}

func TestBuild_Probabilistic(t *testing.T) {
	p := policy(evaluator.Probabilistic)
	p.ProbabilisticCfg = evaluator.ProbabilisticCfg{SamplingPercentage: 100}
	chain, err := evaluator.Build(componenttest.NewNopTelemetrySettings(), []evaluator.PolicyCfg{p})
	require.NoError(t, err)
	d, err := chain.Evaluate(makeTraceWithID("aabbccdd11111111aabbccdd11111111"))
	require.NoError(t, err)
	assert.Equal(t, evaluator.SampledLocal, d)
}

func TestBuild_UnsupportedType(t *testing.T) {
	for _, typ := range []evaluator.PolicyType{evaluator.SpanCount, evaluator.RateLimiting, evaluator.BytesLimiting, evaluator.Composite} {
		_, err := evaluator.Build(componenttest.NewNopTelemetrySettings(), []evaluator.PolicyCfg{policy(typ)})
		assert.Error(t, err, "policy type %q should return an error", typ)
	}
}

func TestBuild_UnknownType(t *testing.T) {
	p := policy(evaluator.PolicyType("unknown_type"))
	_, err := evaluator.Build(componenttest.NewNopTelemetrySettings(), []evaluator.PolicyCfg{p})
	assert.Error(t, err)
}

func TestBuild_AndCompound(t *testing.T) {
	p := evaluator.PolicyCfg{}
	p.Name = "and-test"
	p.Type = evaluator.And
	sub := evaluator.AndSubPolicyCfg{}
	sub.Name = "always"
	sub.Type = evaluator.AlwaysSample
	p.AndCfg = evaluator.AndCfg{SubPolicyCfg: []evaluator.AndSubPolicyCfg{sub}}
	chain, err := evaluator.Build(componenttest.NewNopTelemetrySettings(), []evaluator.PolicyCfg{p})
	require.NoError(t, err)
	d, err := chain.Evaluate(ptrace.NewTraces())
	require.NoError(t, err)
	assert.Equal(t, evaluator.Sampled, d)
}
