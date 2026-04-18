package evaluator_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"retroactivesampling/processor/retroactivesampling/internal/evaluator"
)

func TestLatencyEvaluator_AboveThreshold(t *testing.T) {
	e := &evaluator.LatencyEvaluator{Threshold: 5 * time.Second}
	assert.True(t, e.Evaluate(makeTraces(ptrace.StatusCodeOk, 6000)))
}

func TestLatencyEvaluator_BelowThreshold(t *testing.T) {
	e := &evaluator.LatencyEvaluator{Threshold: 5 * time.Second}
	assert.False(t, e.Evaluate(makeTraces(ptrace.StatusCodeOk, 1000)))
}

func TestLatencyEvaluator_AtThreshold(t *testing.T) {
	e := &evaluator.LatencyEvaluator{Threshold: 5 * time.Second}
	assert.True(t, e.Evaluate(makeTraces(ptrace.StatusCodeOk, 5000)))
}
