package evaluator_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl"
	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/evaluator"
)

func makeTraceWithAttr(key, val string) ptrace.Traces {
	td := ptrace.NewTraces()
	span := td.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	span.Attributes().PutStr(key, val)
	return td
}

func TestOTTL_SpanAttributeMatch(t *testing.T) {
	ev, err := evaluator.NewOTTLConditionFilter(
		componenttest.NewNopTelemetrySettings(),
		[]string{`attributes["env"] == "prod"`},
		nil,
		ottl.IgnoreError,
	)
	require.NoError(t, err)
	d, err := ev.Evaluate(makeTraceWithAttr("env", "prod"))
	require.NoError(t, err)
	assert.Equal(t, evaluator.Sampled, d)
}

func TestOTTL_SpanAttributeNoMatch(t *testing.T) {
	ev, err := evaluator.NewOTTLConditionFilter(
		componenttest.NewNopTelemetrySettings(),
		[]string{`attributes["env"] == "prod"`},
		nil,
		ottl.IgnoreError,
	)
	require.NoError(t, err)
	d, err := ev.Evaluate(makeTraceWithAttr("env", "staging"))
	require.NoError(t, err)
	assert.Equal(t, evaluator.NotSampled, d)
}

func TestOTTL_EmptyConditionsError(t *testing.T) {
	_, err := evaluator.NewOTTLConditionFilter(
		componenttest.NewNopTelemetrySettings(),
		nil, nil, ottl.IgnoreError,
	)
	assert.Error(t, err)
}
