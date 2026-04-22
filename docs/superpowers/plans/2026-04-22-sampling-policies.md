# Sampling Policies Import — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Import tail-sampling policies from opentelemetry-collector-contrib so the processor accepts the upstream `policies:` YAML format with identical evaluation behavior.

**Architecture:** Copy config structs and policy implementations from `internal/sampling/` upstream, adapt to our `Evaluator` interface (drop `ctx`/`traceID`/`*TraceData`, add `Decision` type), wire up a new `Build(settings, []PolicyCfg)` registry, and update the processor call site. The `probabilistic` policy returns `SampledLocal` to skip coordinator broadcast.

**Tech Stack:** Go, opentelemetry-collector-contrib (cloned locally), `pkg/ottl`, `internal/filter`, `github.com/golang/groupcache/lru`

---

## File Map

| Action | Path | Role |
|---|---|---|
| Replace | `internal/evaluator/evaluator.go` | `Decision` type, `Evaluator` interface, `Chain` logic |
| Create | `internal/evaluator/config.go` | Policy config structs copied from upstream |
| Create | `internal/evaluator/util.go` | `hasSpanWithCondition` and `hasResourceOrSpanWithCondition` helpers |
| Replace | `internal/evaluator/latency.go` | Upstream latency policy adapted |
| Delete | `internal/evaluator/error.go` | Replaced by `status_code.go` |
| Create | `internal/evaluator/always_sample.go` | always_sample policy |
| Create | `internal/evaluator/status_code.go` | status_code policy (replaces error_status) |
| Create | `internal/evaluator/trace_flags.go` | trace_flags policy |
| Create | `internal/evaluator/trace_state.go` | trace_state policy |
| Create | `internal/evaluator/string_attribute.go` | string_attribute policy with regex/LRU |
| Create | `internal/evaluator/numeric_attribute.go` | numeric_attribute policy |
| Create | `internal/evaluator/boolean_attribute.go` | boolean_attribute policy |
| Create | `internal/evaluator/probabilistic.go` | probabilistic policy (returns SampledLocal) |
| Create | `internal/evaluator/and.go` | and compound policy |
| Create | `internal/evaluator/not.go` | not compound policy |
| Create | `internal/evaluator/drop.go` | drop compound policy (halts chain) |
| Create | `internal/evaluator/ottl.go` | ottl_condition policy |
| Replace | `internal/evaluator/registry.go` | `Build(settings, []PolicyCfg)` replacing old `Build([]RuleConfig)` |
| Replace | `config.go` | `Policies []PolicyCfg` replacing `Rules []RuleConfig` |
| Modify | `processor.go` | Switch on Decision, add `ingestLocal` |
| Delete | `internal/evaluator/error_test.go` | Covered by new status_code tests |
| Replace | `internal/evaluator/latency_test.go` | Updated for new interface |
| Create | `internal/evaluator/evaluator_test.go` | Chain behavior tests |
| Create | `internal/evaluator/status_code_test.go` | status_code policy tests |
| Create | `internal/evaluator/probabilistic_test.go` | probabilistic + SampledLocal tests |
| Create | `internal/evaluator/compound_test.go` | and/not/drop tests |
| Create | `internal/evaluator/ottl_test.go` | ottl_condition tests |
| Create | `internal/evaluator/registry_test.go` | Build function tests |
| Modify | `processor_test.go` | Use `Policies` config, add probabilistic test |

---

## Task 1: Update `evaluator.go` — Decision type and Chain

**Files:**
- Replace: `processor/retroactivesampling/internal/evaluator/evaluator.go`
- Create: `processor/retroactivesampling/internal/evaluator/evaluator_test.go`

- [ ] **Step 1: Write the failing tests**

Create `processor/retroactivesampling/internal/evaluator/evaluator_test.go`:

```go
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
```

- [ ] **Step 2: Run to confirm failure**

```bash
cd processor/retroactivesampling && go test ./internal/evaluator/ -run TestChain -v 2>&1 | head -20
```

Expected: compile error — `evaluator.Decision`, `evaluator.Sampled`, etc. undefined.

- [ ] **Step 3: Replace `evaluator.go`**

```go
package evaluator

import "go.opentelemetry.io/collector/pdata/ptrace"

type Decision uint8

const (
	NotSampled  Decision = iota
	Sampled              // interesting — notify coordinator
	SampledLocal         // interesting — skip coordinator (deterministic by trace ID)
	Dropped              // halt chain, do not sample
)

type Evaluator interface {
	Evaluate(ptrace.Traces) (Decision, error)
}

// Chain evaluates policies in order; halts on first non-NotSampled or error.
type Chain []Evaluator

func (c Chain) Evaluate(t ptrace.Traces) (Decision, error) {
	for _, e := range c {
		d, err := e.Evaluate(t)
		if err != nil {
			return NotSampled, err
		}
		if d != NotSampled {
			return d, nil
		}
	}
	return NotSampled, nil
}
```

- [ ] **Step 4: Run tests — expect compile errors in other files (they still use old interface)**

```bash
cd processor/retroactivesampling && go build ./internal/evaluator/ 2>&1 | head -20
```

Expected: errors about `Evaluate(ptrace.Traces) bool` — old implementations don't match new interface. That's expected at this stage.

- [ ] **Step 5: Run only the new chain tests (they should pass once we fix the package)**

The chain tests will fail to compile because old `error.go` and `latency.go` still define the old interface. We will fix them in subsequent tasks. Skip running until Task 4.

- [ ] **Step 6: Commit**

```bash
git add processor/retroactivesampling/internal/evaluator/evaluator.go \
        processor/retroactivesampling/internal/evaluator/evaluator_test.go
git commit -m "feat(evaluator): add Decision type and update Chain interface"
```

---

## Task 2: Copy config structs from upstream

**Files:**
- Create: `processor/retroactivesampling/internal/evaluator/config.go`

No tests needed — pure data types.

- [ ] **Step 1: Clone the upstream repo** (if not already done)

```bash
git clone --depth=1 --filter=blob:none --sparse \
  https://github.com/open-telemetry/opentelemetry-collector-contrib.git /tmp/otelcontrib
git -C /tmp/otelcontrib sparse-checkout set processor/tailsamplingprocessor pkg/ottl internal/filter
git -C /tmp/otelcontrib checkout
```

Expected: repo cloned to `/tmp/otelcontrib`.

- [ ] **Step 2: Create `config.go`**

Copy the relevant types from `/tmp/otelcontrib/processor/tailsamplingprocessor/config.go`. Do **not** copy `Config`, `CompositeCfg`, `CompositeSubPolicyCfg`, `RateAllocationCfg`, `DecisionCacheConfig`, or the `Validate` method. Do **not** import `ottl` — use `string` for `ErrorMode` temporarily; the OTTL dep is added in Task 8.

Create `processor/retroactivesampling/internal/evaluator/config.go`:

```go
package evaluator

// PolicyType indicates the type of sampling policy.
type PolicyType string

const (
	AlwaysSample     PolicyType = "always_sample"
	Latency          PolicyType = "latency"
	NumericAttribute PolicyType = "numeric_attribute"
	Probabilistic    PolicyType = "probabilistic"
	StatusCode       PolicyType = "status_code"
	StringAttribute  PolicyType = "string_attribute"
	RateLimiting     PolicyType = "rate_limiting"
	And              PolicyType = "and"
	Not              PolicyType = "not"
	Drop             PolicyType = "drop"
	SpanCount        PolicyType = "span_count"
	TraceState       PolicyType = "trace_state"
	BooleanAttribute PolicyType = "boolean_attribute"
	OTTLCondition    PolicyType = "ottl_condition"
	BytesLimiting    PolicyType = "bytes_limiting"
	TraceFlags       PolicyType = "trace_flags"
	Composite        PolicyType = "composite"
)

type sharedPolicyCfg struct {
	Name                string              `mapstructure:"name"`
	Type                PolicyType          `mapstructure:"type"`
	LatencyCfg          LatencyCfg          `mapstructure:"latency"`
	NumericAttributeCfg NumericAttributeCfg `mapstructure:"numeric_attribute"`
	ProbabilisticCfg    ProbabilisticCfg    `mapstructure:"probabilistic"`
	StatusCodeCfg       StatusCodeCfg       `mapstructure:"status_code"`
	StringAttributeCfg  StringAttributeCfg  `mapstructure:"string_attribute"`
	RateLimitingCfg     RateLimitingCfg     `mapstructure:"rate_limiting"`
	BytesLimitingCfg    BytesLimitingCfg    `mapstructure:"bytes_limiting"`
	SpanCountCfg        SpanCountCfg        `mapstructure:"span_count"`
	TraceStateCfg       TraceStateCfg       `mapstructure:"trace_state"`
	BooleanAttributeCfg BooleanAttributeCfg `mapstructure:"boolean_attribute"`
	OTTLConditionCfg    OTTLConditionCfg    `mapstructure:"ottl_condition"`
}

// PolicyCfg holds the common configuration to all policies.
type PolicyCfg struct {
	sharedPolicyCfg `mapstructure:",squash"`
	AndCfg          AndCfg  `mapstructure:"and"`
	NotCfg          NotCfg  `mapstructure:"not"`
	DropCfg         DropCfg `mapstructure:"drop"`
}

// AndSubPolicyCfg holds configuration for policies under and/drop.
type AndSubPolicyCfg struct {
	sharedPolicyCfg `mapstructure:",squash"`
}

// NotSubPolicyCfg holds configuration for the policy under not.
type NotSubPolicyCfg struct {
	sharedPolicyCfg `mapstructure:",squash"`
}

type AndCfg struct {
	SubPolicyCfg []AndSubPolicyCfg `mapstructure:"and_sub_policy"`
	_            struct{}
}

type NotCfg struct {
	SubPolicy NotSubPolicyCfg `mapstructure:"not_sub_policy"`
}

type DropCfg struct {
	SubPolicyCfg []AndSubPolicyCfg `mapstructure:"drop_sub_policy"`
	_            struct{}
}

type LatencyCfg struct {
	ThresholdMs      int64 `mapstructure:"threshold_ms"`
	UpperThresholdMs int64 `mapstructure:"upper_threshold_ms"`
	_                struct{}
}

type NumericAttributeCfg struct {
	Key         string `mapstructure:"key"`
	MinValue    int64  `mapstructure:"min_value"`
	MaxValue    int64  `mapstructure:"max_value"`
	InvertMatch bool   `mapstructure:"invert_match"`
}

type ProbabilisticCfg struct {
	HashSalt           string  `mapstructure:"hash_salt"`
	SamplingPercentage float64 `mapstructure:"sampling_percentage"`
	_                  struct{}
}

type StatusCodeCfg struct {
	StatusCodes []string `mapstructure:"status_codes"`
	_           struct{}
}

type StringAttributeCfg struct {
	Key                  string   `mapstructure:"key"`
	Values               []string `mapstructure:"values"`
	EnabledRegexMatching bool     `mapstructure:"enabled_regex_matching"`
	CacheMaxSize         int      `mapstructure:"cache_max_size"`
	InvertMatch          bool     `mapstructure:"invert_match"`
}

type RateLimitingCfg struct {
	SpansPerSecond int64 `mapstructure:"spans_per_second"`
	_              struct{}
}

type BytesLimitingCfg struct {
	BytesPerSecond int64 `mapstructure:"bytes_per_second"`
	BurstCapacity  int64 `mapstructure:"burst_capacity"`
}

type SpanCountCfg struct {
	MinSpans int32 `mapstructure:"min_spans"`
	MaxSpans int32 `mapstructure:"max_spans"`
	_        struct{}
}

type BooleanAttributeCfg struct {
	Key         string `mapstructure:"key"`
	Value       bool   `mapstructure:"value"`
	InvertMatch bool   `mapstructure:"invert_match"`
}

type TraceStateCfg struct {
	Key    string   `mapstructure:"key"`
	Values []string `mapstructure:"values"`
}

// OTTLConditionCfg uses string for ErrorMode to avoid the ottl dep here.
// It is cast to ottl.ErrorMode in ottl.go at construction time.
type OTTLConditionCfg struct {
	ErrorMode           string   `mapstructure:"error_mode"`
	SpanConditions      []string `mapstructure:"span"`
	SpanEventConditions []string `mapstructure:"spanevent"`
	_                   struct{}
}
```

- [ ] **Step 3: Verify it compiles**

```bash
cd processor/retroactivesampling && go build ./internal/evaluator/ 2>&1 | grep -v "error.go\|latency.go" | head -20
```

Expected: only errors from old `error.go` and `latency.go` (old Evaluate signature).

- [ ] **Step 4: Commit**

```bash
git add processor/retroactivesampling/internal/evaluator/config.go
git commit -m "feat(evaluator): add upstream-compatible policy config structs"
```

---

## Task 3: Add `util.go` — span iteration helpers

**Files:**
- Create: `processor/retroactivesampling/internal/evaluator/util.go`

Adapted from `/tmp/otelcontrib/processor/tailsamplingprocessor/internal/sampling/util.go`. We drop the inverted-decision feature gate and always use `Sampled`/`NotSampled`.

- [ ] **Step 1: Create `util.go`**

```go
package evaluator

import (
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

func hasResourceOrSpanWithCondition(
	td ptrace.Traces,
	shouldSampleResource func(pcommon.Resource) bool,
	shouldSampleSpan func(ptrace.Span) bool,
) Decision {
	for i := 0; i < td.ResourceSpans().Len(); i++ {
		rs := td.ResourceSpans().At(i)
		if shouldSampleResource(rs.Resource()) {
			return Sampled
		}
		if hasInstrumentationLibrarySpanWithCondition(rs.ScopeSpans(), shouldSampleSpan, false) {
			return Sampled
		}
	}
	return NotSampled
}

// invertHasResourceOrSpanWithCondition returns Sampled when the condition is absent/non-matching
// on every resource and span. Used for invert_match policies.
func invertHasResourceOrSpanWithCondition(
	td ptrace.Traces,
	shouldSampleResource func(pcommon.Resource) bool,
	shouldSampleSpan func(ptrace.Span) bool,
) Decision {
	for i := 0; i < td.ResourceSpans().Len(); i++ {
		rs := td.ResourceSpans().At(i)
		if !shouldSampleResource(rs.Resource()) {
			return NotSampled
		}
		if !hasInstrumentationLibrarySpanWithCondition(rs.ScopeSpans(), shouldSampleSpan, true) {
			return NotSampled
		}
	}
	return Sampled
}

func hasSpanWithCondition(td ptrace.Traces, shouldSample func(ptrace.Span) bool) Decision {
	for i := 0; i < td.ResourceSpans().Len(); i++ {
		if hasInstrumentationLibrarySpanWithCondition(td.ResourceSpans().At(i).ScopeSpans(), shouldSample, false) {
			return Sampled
		}
	}
	return NotSampled
}

func hasInstrumentationLibrarySpanWithCondition(ilss ptrace.ScopeSpansSlice, check func(ptrace.Span) bool, invert bool) bool {
	for i := 0; i < ilss.Len(); i++ {
		ils := ilss.At(i)
		for j := 0; j < ils.Spans().Len(); j++ {
			if r := check(ils.Spans().At(j)); r != invert {
				return r
			}
		}
	}
	return invert
}
```

- [ ] **Step 2: Commit**

```bash
git add processor/retroactivesampling/internal/evaluator/util.go
git commit -m "feat(evaluator): add span iteration helpers adapted from upstream"
```

---

## Task 4: Simple leaf policies — always_sample, latency, status_code, trace_flags, trace_state

**Files:**
- Create: `internal/evaluator/always_sample.go`
- Replace: `internal/evaluator/latency.go`
- Create: `internal/evaluator/status_code.go`
- Create: `internal/evaluator/trace_flags.go`
- Create: `internal/evaluator/trace_state.go`
- Delete: `internal/evaluator/error.go`
- Replace: `internal/evaluator/latency_test.go`
- Delete: `internal/evaluator/error_test.go`
- Create: `internal/evaluator/status_code_test.go`

- [ ] **Step 1: Write tests for status_code (new) and updated latency**

Create `processor/retroactivesampling/internal/evaluator/status_code_test.go`:

```go
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
```

Replace `processor/retroactivesampling/internal/evaluator/latency_test.go`:

```go
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
```

- [ ] **Step 2: Run to confirm failure**

```bash
cd processor/retroactivesampling && go test ./internal/evaluator/ -run "TestStatusCode|TestLatency" -v 2>&1 | head -20
```

Expected: compile errors — `NewStatusCodeFilter`, `NewLatency` not defined.

- [ ] **Step 3: Create `always_sample.go`**

```go
package evaluator

import (
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"
)

type alwaysSample struct{ logger *zap.Logger }

func NewAlwaysSample(logger *zap.Logger) Evaluator {
	return &alwaysSample{logger: logger}
}

func (as *alwaysSample) Evaluate(ptrace.Traces) (Decision, error) {
	as.logger.Debug("Evaluating spans in always-sample filter")
	return Sampled, nil
}
```

- [ ] **Step 4: Replace `latency.go`**

```go
package evaluator

import (
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"
)

type latency struct {
	logger           *zap.Logger
	thresholdMs      int64
	upperThresholdMs int64
}

func NewLatency(logger *zap.Logger, thresholdMs, upperThresholdMs int64) Evaluator {
	return &latency{logger: logger, thresholdMs: thresholdMs, upperThresholdMs: upperThresholdMs}
}

func (l *latency) Evaluate(t ptrace.Traces) (Decision, error) {
	l.logger.Debug("Evaluating spans in latency filter")
	var minTime, maxTime pcommon.Timestamp
	return hasSpanWithCondition(t, func(span ptrace.Span) bool {
		if minTime == 0 || span.StartTimestamp() < minTime {
			minTime = span.StartTimestamp()
		}
		if maxTime == 0 || span.EndTimestamp() > maxTime {
			maxTime = span.EndTimestamp()
		}
		d := maxTime.AsTime().Sub(minTime.AsTime())
		if l.upperThresholdMs == 0 {
			return d.Milliseconds() >= l.thresholdMs
		}
		return l.thresholdMs < d.Milliseconds() && d.Milliseconds() <= l.upperThresholdMs
	}), nil
}
```

- [ ] **Step 5: Create `status_code.go`**

```go
package evaluator

import (
	"errors"
	"fmt"

	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"
)

type statusCodeFilter struct {
	logger      *zap.Logger
	statusCodes []ptrace.StatusCode
}

func NewStatusCodeFilter(logger *zap.Logger, statusCodeStrings []string) (Evaluator, error) {
	if len(statusCodeStrings) == 0 {
		return nil, errors.New("expected at least one status code to filter on")
	}
	codes := make([]ptrace.StatusCode, len(statusCodeStrings))
	for i, s := range statusCodeStrings {
		switch s {
		case "OK":
			codes[i] = ptrace.StatusCodeOk
		case "ERROR":
			codes[i] = ptrace.StatusCodeError
		case "UNSET":
			codes[i] = ptrace.StatusCodeUnset
		default:
			return nil, fmt.Errorf("unknown status code %q, supported: OK, ERROR, UNSET", s)
		}
	}
	return &statusCodeFilter{logger: logger, statusCodes: codes}, nil
}

func (r *statusCodeFilter) Evaluate(t ptrace.Traces) (Decision, error) {
	r.logger.Debug("Evaluating spans in status code filter")
	return hasSpanWithCondition(t, func(span ptrace.Span) bool {
		for _, c := range r.statusCodes {
			if span.Status().Code() == c {
				return true
			}
		}
		return false
	}), nil
}
```

- [ ] **Step 6: Create `trace_flags.go`**

```go
package evaluator

import (
	oteltrace "go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"
)

type traceFlags struct{ logger *zap.Logger }

func NewTraceFlags(logger *zap.Logger) Evaluator {
	return &traceFlags{logger: logger}
}

func (tf *traceFlags) Evaluate(t ptrace.Traces) (Decision, error) {
	tf.logger.Debug("Evaluating spans in trace-flags filter")
	return hasSpanWithCondition(t, func(span ptrace.Span) bool {
		return (byte(span.Flags()) & byte(oteltrace.FlagsSampled)) != 0
	}), nil
}
```

- [ ] **Step 7: Create `trace_state.go`**

```go
package evaluator

import (
	oteltrace "go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"
)

type traceStateFilter struct {
	key     string
	logger  *zap.Logger
	matcher func(string) bool
}

func NewTraceStateFilter(logger *zap.Logger, key string, values []string) Evaluator {
	valuesMap := make(map[string]struct{})
	for _, v := range values {
		if v != "" && len(key)+len(v) < 256 {
			valuesMap[v] = struct{}{}
		}
	}
	return &traceStateFilter{
		key:    key,
		logger: logger,
		matcher: func(s string) bool { _, ok := valuesMap[s]; return ok },
	}
}

func (tsf *traceStateFilter) Evaluate(t ptrace.Traces) (Decision, error) {
	return hasSpanWithCondition(t, func(span ptrace.Span) bool {
		ts, err := oteltrace.ParseTraceState(span.TraceState().AsRaw())
		if err != nil {
			return false
		}
		return tsf.matcher(ts.Get(tsf.key))
	}), nil
}
```

- [ ] **Step 8: Delete old files**

```bash
rm processor/retroactivesampling/internal/evaluator/error.go \
   processor/retroactivesampling/internal/evaluator/error_test.go
```

- [ ] **Step 9: Run tests**

```bash
cd processor/retroactivesampling && go test ./internal/evaluator/ -run "TestStatusCode|TestLatency|TestChain" -v 2>&1
```

Expected: all tests PASS.

- [ ] **Step 10: Commit**

```bash
git add processor/retroactivesampling/internal/evaluator/
git commit -m "feat(evaluator): add always_sample, latency, status_code, trace_flags, trace_state policies"
```

---

## Task 5: Attribute policies — string_attribute, numeric_attribute, boolean_attribute

**Files:**
- Create: `internal/evaluator/string_attribute.go`
- Create: `internal/evaluator/numeric_attribute.go`
- Create: `internal/evaluator/boolean_attribute.go`

- [ ] **Step 1: Add groupcache dependency**

```bash
cd processor/retroactivesampling && go get github.com/golang/groupcache@latest
```

Expected: `go.mod` and `go.sum` updated.

- [ ] **Step 2: Create `string_attribute.go`**

Adapted from `/tmp/otelcontrib/processor/tailsamplingprocessor/internal/sampling/string_tag_filter.go`. The logic is identical; only the interface changes.

```go
package evaluator

import (
	"fmt"
	"regexp"

	"github.com/golang/groupcache/lru"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"
)

const defaultCacheSize = 128

type stringAttributeFilter struct {
	key         string
	logger      *zap.Logger
	matcher     func(string) bool
	invertMatch bool
}

func NewStringAttributeFilter(logger *zap.Logger, key string, values []string, regexMatchEnabled bool, evictSize int, invertMatch bool) (Evaluator, error) {
	if regexMatchEnabled {
		if evictSize <= 0 {
			evictSize = defaultCacheSize
		}
		filterList, err := compileFilters(values)
		if err != nil {
			return nil, err
		}
		cache := lru.New(evictSize)
		return &stringAttributeFilter{
			key:    key,
			logger: logger,
			matcher: func(s string) bool {
				if v, ok := cache.Get(s); ok {
					return v.(bool)
				}
				for _, r := range filterList {
					if r.MatchString(s) {
						cache.Add(s, true)
						return true
					}
				}
				cache.Add(s, false)
				return false
			},
			invertMatch: invertMatch,
		}, nil
	}
	valuesMap := make(map[string]struct{})
	for _, v := range values {
		if v != "" {
			valuesMap[v] = struct{}{}
		}
	}
	return &stringAttributeFilter{
		key:         key,
		logger:      logger,
		matcher:     func(s string) bool { _, ok := valuesMap[s]; return ok },
		invertMatch: invertMatch,
	}, nil
}

func (saf *stringAttributeFilter) Evaluate(t ptrace.Traces) (Decision, error) {
	saf.logger.Debug("Evaluating spans in string-tag filter")
	if saf.invertMatch {
		return invertHasResourceOrSpanWithCondition(t,
			func(r pcommon.Resource) bool {
				if v, ok := r.Attributes().Get(saf.key); ok {
					return !saf.matcher(v.Str())
				}
				return true
			},
			func(span ptrace.Span) bool {
				if v, ok := span.Attributes().Get(saf.key); ok && v.Str() != "" {
					return !saf.matcher(v.Str())
				}
				return true
			},
		), nil
	}
	return hasResourceOrSpanWithCondition(t,
		func(r pcommon.Resource) bool {
			if v, ok := r.Attributes().Get(saf.key); ok {
				return saf.matcher(v.Str())
			}
			return false
		},
		func(span ptrace.Span) bool {
			if v, ok := span.Attributes().Get(saf.key); ok && v.Str() != "" {
				return saf.matcher(v.Str())
			}
			return false
		},
	), nil
}

func compileFilters(exprs []string) ([]*regexp.Regexp, error) {
	list := make([]*regexp.Regexp, 0, len(exprs))
	for _, e := range exprs {
		r, err := regexp.Compile(e)
		if err != nil {
			return nil, fmt.Errorf("invalid regex `%s`: %w", e, err)
		}
		list = append(list, r)
	}
	return list, nil
}
```

- [ ] **Step 3: Create `numeric_attribute.go`**

```go
package evaluator

import (
	"math"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"
)

type numericAttributeFilter struct {
	key         string
	minValue    *int64
	maxValue    *int64
	logger      *zap.Logger
	invertMatch bool
}

// NewNumericAttributeFilter creates a policy evaluator for numeric attribute ranges.
// At least one of minValue or maxValue must be non-nil.
func NewNumericAttributeFilter(logger *zap.Logger, key string, minValue, maxValue *int64, invertMatch bool) Evaluator {
	return &numericAttributeFilter{key: key, minValue: minValue, maxValue: maxValue, logger: logger, invertMatch: invertMatch}
}

func (naf *numericAttributeFilter) Evaluate(t ptrace.Traces) (Decision, error) {
	minVal := int64(math.MinInt64)
	if naf.minValue != nil {
		minVal = *naf.minValue
	}
	maxVal := int64(math.MaxInt64)
	if naf.maxValue != nil {
		maxVal = *naf.maxValue
	}
	inRange := func(v int64) bool { return v >= minVal && v <= maxVal }

	if naf.invertMatch {
		return invertHasResourceOrSpanWithCondition(t,
			func(r pcommon.Resource) bool {
				if v, ok := r.Attributes().Get(naf.key); ok {
					return !inRange(v.Int())
				}
				return true
			},
			func(span ptrace.Span) bool {
				if v, ok := span.Attributes().Get(naf.key); ok {
					return !inRange(v.Int())
				}
				return true
			},
		), nil
	}
	return hasResourceOrSpanWithCondition(t,
		func(r pcommon.Resource) bool {
			if v, ok := r.Attributes().Get(naf.key); ok {
				return inRange(v.Int())
			}
			return false
		},
		func(span ptrace.Span) bool {
			if v, ok := span.Attributes().Get(naf.key); ok {
				return inRange(v.Int())
			}
			return false
		},
	), nil
}
```

- [ ] **Step 4: Create `boolean_attribute.go`**

```go
package evaluator

import (
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"
)

type booleanAttributeFilter struct {
	key         string
	value       bool
	logger      *zap.Logger
	invertMatch bool
}

func NewBooleanAttributeFilter(logger *zap.Logger, key string, value, invertMatch bool) Evaluator {
	return &booleanAttributeFilter{key: key, value: value, logger: logger, invertMatch: invertMatch}
}

func (baf *booleanAttributeFilter) Evaluate(t ptrace.Traces) (Decision, error) {
	matches := func(v bool) bool { return v == baf.value }
	if baf.invertMatch {
		return invertHasResourceOrSpanWithCondition(t,
			func(r pcommon.Resource) bool {
				if v, ok := r.Attributes().Get(baf.key); ok {
					return !matches(v.Bool())
				}
				return true
			},
			func(span ptrace.Span) bool {
				if v, ok := span.Attributes().Get(baf.key); ok {
					return !matches(v.Bool())
				}
				return true
			},
		), nil
	}
	return hasResourceOrSpanWithCondition(t,
		func(r pcommon.Resource) bool {
			if v, ok := r.Attributes().Get(baf.key); ok {
				return matches(v.Bool())
			}
			return false
		},
		func(span ptrace.Span) bool {
			if v, ok := span.Attributes().Get(baf.key); ok {
				return matches(v.Bool())
			}
			return false
		},
	), nil
}
```

- [ ] **Step 5: Verify compilation**

```bash
cd processor/retroactivesampling && go build ./internal/evaluator/ 2>&1
```

Expected: no errors.

- [ ] **Step 6: Commit**

```bash
git add processor/retroactivesampling/internal/evaluator/ \
        processor/retroactivesampling/go.mod \
        processor/retroactivesampling/go.sum
git commit -m "feat(evaluator): add string_attribute, numeric_attribute, boolean_attribute policies"
```

---

## Task 6: Probabilistic policy

**Files:**
- Create: `internal/evaluator/probabilistic.go`
- Create: `internal/evaluator/probabilistic_test.go`

- [ ] **Step 1: Write failing test**

Create `processor/retroactivesampling/internal/evaluator/probabilistic_test.go`:

```go
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
```

- [ ] **Step 2: Run to confirm failure**

```bash
cd processor/retroactivesampling && go test ./internal/evaluator/ -run TestProbabilistic -v 2>&1 | head -10
```

Expected: compile error — `NewProbabilisticSampler` undefined.

- [ ] **Step 3: Create `probabilistic.go`**

```go
package evaluator

import (
	"hash/fnv"
	"math"
	"math/big"

	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"
)

const defaultHashSalt = "default-hash-seed"

type probabilisticSampler struct {
	logger    *zap.Logger
	threshold uint64
	hashSalt  string
}

func NewProbabilisticSampler(logger *zap.Logger, hashSalt string, samplingPercentage float64) Evaluator {
	if hashSalt == "" {
		hashSalt = defaultHashSalt
	}
	return &probabilisticSampler{
		logger:    logger,
		threshold: calculateThreshold(samplingPercentage / 100),
		hashSalt:  hashSalt,
	}
}

// Evaluate extracts trace ID from the first span (guaranteed present by groupByTrace)
// and returns SampledLocal — coordinator broadcast is skipped since all collectors
// with the same config make the same decision for a given trace ID.
func (s *probabilisticSampler) Evaluate(t ptrace.Traces) (Decision, error) {
	s.logger.Debug("Evaluating spans in probabilistic filter")
	rs := t.ResourceSpans()
	if rs.Len() == 0 {
		return NotSampled, nil
	}
	ss := rs.At(0).ScopeSpans()
	if ss.Len() == 0 {
		return NotSampled, nil
	}
	spans := ss.At(0).Spans()
	if spans.Len() == 0 {
		return NotSampled, nil
	}
	tid := spans.At(0).TraceID()
	if hashTraceID(s.hashSalt, tid[:]) <= s.threshold {
		return SampledLocal, nil
	}
	return NotSampled, nil
}

func calculateThreshold(ratio float64) uint64 {
	boundary := new(big.Float).SetInt(new(big.Int).SetUint64(math.MaxUint64))
	res, _ := boundary.Mul(boundary, big.NewFloat(ratio)).Uint64()
	return res
}

func hashTraceID(salt string, b []byte) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(salt))
	_, _ = h.Write(b)
	return h.Sum64()
}
```

- [ ] **Step 4: Run tests**

```bash
cd processor/retroactivesampling && go test ./internal/evaluator/ -run TestProbabilistic -v 2>&1
```

Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add processor/retroactivesampling/internal/evaluator/probabilistic.go \
        processor/retroactivesampling/internal/evaluator/probabilistic_test.go
git commit -m "feat(evaluator): add probabilistic policy returning SampledLocal"
```

---

## Task 7: Compound policies — and, not, drop

**Files:**
- Create: `internal/evaluator/and.go`
- Create: `internal/evaluator/not.go`
- Create: `internal/evaluator/drop.go`
- Create: `internal/evaluator/compound_test.go`

- [ ] **Step 1: Write failing tests**

Create `processor/retroactivesampling/internal/evaluator/compound_test.go`:

```go
package evaluator_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"

	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/evaluator"
)

func TestAnd_AllMatch(t *testing.T) {
	and := evaluator.NewAnd(zap.NewNop(), []evaluator.Evaluator{
		stub{d: evaluator.Sampled},
		stub{d: evaluator.SampledLocal},
	})
	d, err := and.Evaluate(ptrace.NewTraces())
	require.NoError(t, err)
	assert.Equal(t, evaluator.Sampled, d)
}

func TestAnd_OneNotSampledShortCircuits(t *testing.T) {
	and := evaluator.NewAnd(zap.NewNop(), []evaluator.Evaluator{
		stub{d: evaluator.Sampled},
		stub{d: evaluator.NotSampled},
		stub{d: evaluator.Sampled},
	})
	d, err := and.Evaluate(ptrace.NewTraces())
	require.NoError(t, err)
	assert.Equal(t, evaluator.NotSampled, d)
}

func TestAnd_NeverReturnsSampledLocal(t *testing.T) {
	and := evaluator.NewAnd(zap.NewNop(), []evaluator.Evaluator{
		stub{d: evaluator.SampledLocal},
		stub{d: evaluator.SampledLocal},
	})
	d, err := and.Evaluate(ptrace.NewTraces())
	require.NoError(t, err)
	assert.Equal(t, evaluator.Sampled, d, "and must always return Sampled (never SampledLocal)")
}

func TestNot_InvertsSampled(t *testing.T) {
	not := evaluator.NewNot(zap.NewNop(), stub{d: evaluator.Sampled})
	d, err := not.Evaluate(ptrace.NewTraces())
	require.NoError(t, err)
	assert.Equal(t, evaluator.NotSampled, d)
}

func TestNot_InvertsNotSampled(t *testing.T) {
	not := evaluator.NewNot(zap.NewNop(), stub{d: evaluator.NotSampled})
	d, err := not.Evaluate(ptrace.NewTraces())
	require.NoError(t, err)
	assert.Equal(t, evaluator.Sampled, d)
}

func TestNot_InvertsSampledLocal(t *testing.T) {
	not := evaluator.NewNot(zap.NewNop(), stub{d: evaluator.SampledLocal})
	d, err := not.Evaluate(ptrace.NewTraces())
	require.NoError(t, err)
	assert.Equal(t, evaluator.NotSampled, d)
}

func TestDrop_AllMatchDrops(t *testing.T) {
	drop := evaluator.NewDrop(zap.NewNop(), []evaluator.Evaluator{
		stub{d: evaluator.Sampled},
		stub{d: evaluator.Sampled},
	})
	d, err := drop.Evaluate(ptrace.NewTraces())
	require.NoError(t, err)
	assert.Equal(t, evaluator.Dropped, d)
}

func TestDrop_OneNotSampledPasses(t *testing.T) {
	drop := evaluator.NewDrop(zap.NewNop(), []evaluator.Evaluator{
		stub{d: evaluator.Sampled},
		stub{d: evaluator.NotSampled},
	})
	d, err := drop.Evaluate(ptrace.NewTraces())
	require.NoError(t, err)
	assert.Equal(t, evaluator.NotSampled, d)
}

func TestDrop_HaltsChain(t *testing.T) {
	// Drop in a chain halts subsequent policies.
	drop := evaluator.NewDrop(zap.NewNop(), []evaluator.Evaluator{stub{d: evaluator.Sampled}})
	c := evaluator.Chain{drop, stub{d: evaluator.Sampled}}
	d, err := c.Evaluate(ptrace.NewTraces())
	require.NoError(t, err)
	assert.Equal(t, evaluator.Dropped, d)
}
```

- [ ] **Step 2: Run to confirm failure**

```bash
cd processor/retroactivesampling && go test ./internal/evaluator/ -run "TestAnd|TestNot|TestDrop" -v 2>&1 | head -15
```

Expected: compile error — `NewAnd`, `NewNot`, `NewDrop` undefined.

- [ ] **Step 3: Create `and.go`**

```go
package evaluator

import (
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"
)

type andPolicy struct {
	subpolicies []Evaluator
	logger      *zap.Logger
}

func NewAnd(logger *zap.Logger, subpolicies []Evaluator) Evaluator {
	return &andPolicy{subpolicies: subpolicies, logger: logger}
}

// Evaluate returns Sampled if all sub-policies return non-NotSampled.
// Always returns Sampled (never SampledLocal) — AND result depends on
// which spans were seen, so coordinator notification is always needed.
func (c *andPolicy) Evaluate(t ptrace.Traces) (Decision, error) {
	for _, sub := range c.subpolicies {
		d, err := sub.Evaluate(t)
		if err != nil {
			return NotSampled, err
		}
		if d == NotSampled {
			return NotSampled, nil
		}
	}
	return Sampled, nil
}
```

- [ ] **Step 4: Create `not.go`**

```go
package evaluator

import (
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"
)

type notPolicy struct {
	logger             *zap.Logger
	subPolicyEvaluator Evaluator
}

func NewNot(logger *zap.Logger, subPolicyEvaluator Evaluator) Evaluator {
	return &notPolicy{logger: logger, subPolicyEvaluator: subPolicyEvaluator}
}

func (n *notPolicy) Evaluate(t ptrace.Traces) (Decision, error) {
	d, err := n.subPolicyEvaluator.Evaluate(t)
	if err != nil {
		return NotSampled, err
	}
	switch d {
	case Sampled, SampledLocal:
		return NotSampled, nil
	case NotSampled:
		return Sampled, nil
	default:
		return d, nil
	}
}
```

- [ ] **Step 5: Create `drop.go`**

```go
package evaluator

import (
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"
)

type dropPolicy struct {
	subpolicies []Evaluator
	logger      *zap.Logger
}

func NewDrop(logger *zap.Logger, subpolicies []Evaluator) Evaluator {
	return &dropPolicy{subpolicies: subpolicies, logger: logger}
}

// Evaluate returns Dropped (halting the chain) if all sub-policies match.
// Returns NotSampled (continuing the chain) if any sub-policy does not match.
func (c *dropPolicy) Evaluate(t ptrace.Traces) (Decision, error) {
	for _, sub := range c.subpolicies {
		d, err := sub.Evaluate(t)
		if err != nil {
			return NotSampled, err
		}
		if d == NotSampled {
			return NotSampled, nil
		}
	}
	return Dropped, nil
}
```

- [ ] **Step 6: Run tests**

```bash
cd processor/retroactivesampling && go test ./internal/evaluator/ -run "TestAnd|TestNot|TestDrop" -v 2>&1
```

Expected: all PASS.

- [ ] **Step 7: Commit**

```bash
git add processor/retroactivesampling/internal/evaluator/and.go \
        processor/retroactivesampling/internal/evaluator/not.go \
        processor/retroactivesampling/internal/evaluator/drop.go \
        processor/retroactivesampling/internal/evaluator/compound_test.go
git commit -m "feat(evaluator): add and, not, drop compound policies"
```

---

## Task 8: OTTL policy

**Files:**
- Create: `internal/evaluator/ottl.go`
- Create: `internal/evaluator/ottl_test.go`

- [ ] **Step 1: Add OTTL dependencies**

```bash
cd processor/retroactivesampling && \
  go get github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl@latest && \
  go get github.com/open-telemetry/opentelemetry-collector-contrib/internal/filter@latest
```

Expected: `go.mod` and `go.sum` updated with both modules.

- [ ] **Step 2: Update `OTTLConditionCfg` in `config.go` to use `ottl.ErrorMode`**

Replace the `OTTLConditionCfg` struct in `config.go`:

```go
import "github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl"
```

And change:

```go
// OTTLConditionCfg holds the configurable setting to create an OTTL condition filter.
type OTTLConditionCfg struct {
	ErrorMode           ottl.ErrorMode `mapstructure:"error_mode"`
	SpanConditions      []string       `mapstructure:"span"`
	SpanEventConditions []string       `mapstructure:"spanevent"`
	_                   struct{}
}
```

- [ ] **Step 3: Write failing test**

Create `processor/retroactivesampling/internal/evaluator/ottl_test.go`:

```go
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
```

- [ ] **Step 4: Run to confirm failure**

```bash
cd processor/retroactivesampling && go test ./internal/evaluator/ -run TestOTTL -v 2>&1 | head -10
```

Expected: compile error — `NewOTTLConditionFilter` undefined.

- [ ] **Step 5: Create `ottl.go`**

Adapted from `/tmp/otelcontrib/processor/tailsamplingprocessor/internal/sampling/ottl.go`. Replace `trace.ReceivedBatches` with the `t ptrace.Traces` argument; drop ctx/traceID params.

```go
package evaluator

import (
	"context"
	"errors"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"

	"github.com/open-telemetry/opentelemetry-collector-contrib/internal/filter/filterottl"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlspan"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlspanevent"
)

type ottlConditionFilter struct {
	sampleSpanExpr      *ottl.ConditionSequence[*ottlspan.TransformContext]
	sampleSpanEventExpr *ottl.ConditionSequence[*ottlspanevent.TransformContext]
	errorMode           ottl.ErrorMode
	logger              *zap.Logger
}

func NewOTTLConditionFilter(settings component.TelemetrySettings, spanConditions, spanEventConditions []string, errMode ottl.ErrorMode) (Evaluator, error) {
	if len(spanConditions) == 0 && len(spanEventConditions) == 0 {
		return nil, errors.New("expected at least one OTTL condition to filter on")
	}
	f := &ottlConditionFilter{errorMode: errMode, logger: settings.Logger}
	var err error
	if len(spanConditions) > 0 {
		if f.sampleSpanExpr, err = filterottl.NewBoolExprForSpan(spanConditions, filterottl.StandardSpanFuncs(), errMode, settings); err != nil {
			return nil, err
		}
	}
	if len(spanEventConditions) > 0 {
		if f.sampleSpanEventExpr, err = filterottl.NewBoolExprForSpanEvent(spanEventConditions, filterottl.StandardSpanEventFuncs(), errMode, settings); err != nil {
			return nil, err
		}
	}
	return f, nil
}

func (ocf *ottlConditionFilter) Evaluate(t ptrace.Traces) (Decision, error) {
	ctx := context.Background()
	for i := 0; i < t.ResourceSpans().Len(); i++ {
		rs := t.ResourceSpans().At(i)
		for j := 0; j < rs.ScopeSpans().Len(); j++ {
			ss := rs.ScopeSpans().At(j)
			for k := 0; k < ss.Spans().Len(); k++ {
				span := ss.Spans().At(k)
				if ocf.sampleSpanExpr != nil {
					tCtx := ottlspan.NewTransformContextPtr(rs, ss, span)
					ok, err := ocf.sampleSpanExpr.Eval(ctx, tCtx)
					tCtx.Close()
					if err != nil {
						return NotSampled, err
					}
					if ok {
						return Sampled, nil
					}
				}
				if ocf.sampleSpanEventExpr != nil {
					for l := 0; l < span.Events().Len(); l++ {
						tCtx := ottlspanevent.NewTransformContextPtr(rs, ss, span, span.Events().At(l))
						ok, err := ocf.sampleSpanEventExpr.Eval(ctx, tCtx)
						tCtx.Close()
						if err != nil {
							return NotSampled, err
						}
						if ok {
							return Sampled, nil
						}
					}
				}
			}
		}
	}
	return NotSampled, nil
}
```

- [ ] **Step 6: Run tests**

```bash
cd processor/retroactivesampling && go test ./internal/evaluator/ -run TestOTTL -v 2>&1
```

Expected: all PASS.

- [ ] **Step 7: Commit**

```bash
git add processor/retroactivesampling/internal/evaluator/config.go \
        processor/retroactivesampling/internal/evaluator/ottl.go \
        processor/retroactivesampling/internal/evaluator/ottl_test.go \
        processor/retroactivesampling/go.mod \
        processor/retroactivesampling/go.sum
git commit -m "feat(evaluator): add ottl_condition policy with OTTL dependencies"
```

---

## Task 9: Update `registry.go`

**Files:**
- Replace: `internal/evaluator/registry.go`
- Create: `internal/evaluator/registry_test.go`

- [ ] **Step 1: Write failing tests**

Create `processor/retroactivesampling/internal/evaluator/registry_test.go`:

```go
package evaluator_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"pitr.ca/retroactivesampling/processor/retroactivesampling/internal/evaluator"
)

func TestBuild_AlwaysSample(t *testing.T) {
	chain, err := evaluator.Build(componenttest.NewNopTelemetrySettings(), []evaluator.PolicyCfg{
		{sharedPolicyCfg: evaluator.ExportedSharedPolicyCfg(evaluator.AlwaysSample, "p1")},
	})
	require.NoError(t, err)
	d, err := chain.Evaluate(ptrace.NewTraces())
	require.NoError(t, err)
	assert.Equal(t, evaluator.Sampled, d)
}
```

Wait — `sharedPolicyCfg` is unexported. The test needs to construct a `PolicyCfg`. Let me use the exported fields instead.

```go
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
	assert.Equal(t, evaluator.Sampled, d)
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
	p.AndCfg = evaluator.AndCfg{
		SubPolicyCfg: []evaluator.AndSubPolicyCfg{
			func() evaluator.AndSubPolicyCfg {
				s := evaluator.AndSubPolicyCfg{}
				s.Name = "always"
				s.Type = evaluator.AlwaysSample
				return s
			}(),
		},
	}
	chain, err := evaluator.Build(componenttest.NewNopTelemetrySettings(), []evaluator.PolicyCfg{p})
	require.NoError(t, err)
	d, err := chain.Evaluate(ptrace.NewTraces())
	require.NoError(t, err)
	assert.Equal(t, evaluator.Sampled, d)
}
```

Note: `sharedPolicyCfg` is embedded with `mapstructure:",squash"` but it is unexported. For tests we need to set `Name` and `Type`. Since `PolicyCfg` embeds `sharedPolicyCfg` (unexported), we can't set it directly from test code. We need to either export `sharedPolicyCfg` or provide a helper. The simplest fix: export it as `SharedPolicyCfg` (rename in `config.go`).

- [ ] **Step 2: Export `sharedPolicyCfg` → `SharedPolicyCfg` in `config.go`**

In `config.go`, rename `sharedPolicyCfg` to `SharedPolicyCfg` and update all references. Also update `PolicyCfg`, `AndSubPolicyCfg`, `NotSubPolicyCfg` to embed `SharedPolicyCfg`.

```bash
cd processor/retroactivesampling && \
  sed -i '' 's/sharedPolicyCfg/SharedPolicyCfg/g' internal/evaluator/config.go
```

Verify the diff:
```bash
grep "SharedPolicyCfg" processor/retroactivesampling/internal/evaluator/config.go | head -10
```

Expected: `SharedPolicyCfg` appears in struct definitions and embedding.

- [ ] **Step 3: Update registry tests to use exported name**

The test helper `policy(typ)` sets `p.Type = typ` — with `SharedPolicyCfg` exported this works via `p.SharedPolicyCfg.Type = typ`. But since it's embedded, `p.Type` still works. Verify compilation compiles before continuing.

- [ ] **Step 4: Replace `registry.go`**

```go
package evaluator

import (
	"fmt"

	"go.opentelemetry.io/collector/component"
)

// Build constructs a Chain from a slice of PolicyCfg.
// Returns an error if an unsupported type (span_count, rate_limiting,
// bytes_limiting, composite) or unknown type is configured.
func Build(settings component.TelemetrySettings, policies []PolicyCfg) (Chain, error) {
	chain := make(Chain, 0, len(policies))
	for i := range policies {
		ev, err := buildPolicy(settings, &policies[i])
		if err != nil {
			return nil, fmt.Errorf("policy %q: %w", policies[i].Name, err)
		}
		chain = append(chain, ev)
	}
	return chain, nil
}

func buildPolicy(settings component.TelemetrySettings, cfg *PolicyCfg) (Evaluator, error) {
	switch cfg.Type {
	case And:
		return buildAndPolicy(settings, &cfg.AndCfg)
	case Not:
		return buildNotPolicy(settings, &cfg.NotCfg)
	case Drop:
		return buildDropPolicy(settings, &cfg.DropCfg)
	default:
		return buildShared(settings, &cfg.SharedPolicyCfg)
	}
}

func buildShared(settings component.TelemetrySettings, cfg *SharedPolicyCfg) (Evaluator, error) {
	logger := settings.Logger
	switch cfg.Type {
	case AlwaysSample:
		return NewAlwaysSample(logger), nil
	case Latency:
		return NewLatency(logger, cfg.LatencyCfg.ThresholdMs, cfg.LatencyCfg.UpperThresholdMs), nil
	case StatusCode:
		return NewStatusCodeFilter(logger, cfg.StatusCodeCfg.StatusCodes)
	case StringAttribute:
		c := cfg.StringAttributeCfg
		return NewStringAttributeFilter(logger, c.Key, c.Values, c.EnabledRegexMatching, c.CacheMaxSize, c.InvertMatch)
	case NumericAttribute:
		c := cfg.NumericAttributeCfg
		var minPtr, maxPtr *int64
		if c.MinValue != 0 {
			minPtr = &c.MinValue
		}
		if c.MaxValue != 0 {
			maxPtr = &c.MaxValue
		}
		return NewNumericAttributeFilter(logger, c.Key, minPtr, maxPtr, c.InvertMatch), nil
	case BooleanAttribute:
		c := cfg.BooleanAttributeCfg
		return NewBooleanAttributeFilter(logger, c.Key, c.Value, c.InvertMatch), nil
	case Probabilistic:
		c := cfg.ProbabilisticCfg
		return NewProbabilisticSampler(logger, c.HashSalt, c.SamplingPercentage), nil
	case TraceState:
		return NewTraceStateFilter(logger, cfg.TraceStateCfg.Key, cfg.TraceStateCfg.Values), nil
	case TraceFlags:
		return NewTraceFlags(logger), nil
	case OTTLCondition:
		c := cfg.OTTLConditionCfg
		return NewOTTLConditionFilter(settings, c.SpanConditions, c.SpanEventConditions, c.ErrorMode)
	case SpanCount, RateLimiting, BytesLimiting, Composite:
		return nil, fmt.Errorf("policy type %q requires complete trace data and is not supported", cfg.Type)
	default:
		return nil, fmt.Errorf("unknown policy type: %q", cfg.Type)
	}
}

func buildAndPolicy(settings component.TelemetrySettings, cfg *AndCfg) (Evaluator, error) {
	subs := make([]Evaluator, len(cfg.SubPolicyCfg))
	for i := range cfg.SubPolicyCfg {
		ev, err := buildShared(settings, &cfg.SubPolicyCfg[i].SharedPolicyCfg)
		if err != nil {
			return nil, fmt.Errorf("and sub-policy[%d]: %w", i, err)
		}
		subs[i] = ev
	}
	return NewAnd(settings.Logger, subs), nil
}

func buildNotPolicy(settings component.TelemetrySettings, cfg *NotCfg) (Evaluator, error) {
	sub, err := buildShared(settings, &cfg.SubPolicy.SharedPolicyCfg)
	if err != nil {
		return nil, fmt.Errorf("not sub-policy: %w", err)
	}
	return NewNot(settings.Logger, sub), nil
}

func buildDropPolicy(settings component.TelemetrySettings, cfg *DropCfg) (Evaluator, error) {
	subs := make([]Evaluator, len(cfg.SubPolicyCfg))
	for i := range cfg.SubPolicyCfg {
		ev, err := buildShared(settings, &cfg.SubPolicyCfg[i].SharedPolicyCfg)
		if err != nil {
			return nil, fmt.Errorf("drop sub-policy[%d]: %w", i, err)
		}
		subs[i] = ev
	}
	return NewDrop(settings.Logger, subs), nil
}
```

- [ ] **Step 5: Run registry tests**

```bash
cd processor/retroactivesampling && go test ./internal/evaluator/ -run TestBuild -v 2>&1
```

Expected: all PASS.

- [ ] **Step 6: Run all evaluator tests**

```bash
cd processor/retroactivesampling && go test ./internal/evaluator/... -v 2>&1 | tail -20
```

Expected: all PASS.

- [ ] **Step 7: Commit**

```bash
git add processor/retroactivesampling/internal/evaluator/
git commit -m "feat(evaluator): add Build registry supporting all upstream policy types"
```

---

## Task 10: Update processor plumbing

**Files:**
- Replace: `config.go`
- Modify: `processor.go`
- Modify: `processor_test.go`

- [ ] **Step 1: Replace `config.go`**

```go
package retroactivesampling

import "pitr.ca/retroactivesampling/processor/retroactivesampling/internal/evaluator"

type Config struct {
	BufferFile              string                `mapstructure:"buffer_file"`
	MaxBufferBytes          int64                 `mapstructure:"max_buffer_bytes"`
	MaxInterestCacheEntries int                   `mapstructure:"max_interest_cache_entries"`
	CoordinatorEndpoint     string                `mapstructure:"coordinator_endpoint"`
	Policies                []evaluator.PolicyCfg `mapstructure:"policies"`
}
```

- [ ] **Step 2: Update `processor.go`**

In `newProcessor`, change:

```go
chain, err := evaluator.Build(cfg.Rules)
```

to:

```go
chain, err := evaluator.Build(set, cfg.Policies)
```

Replace `processTraces` with:

```go
func (p *retroactiveProcessor) processTraces(_ context.Context, td ptrace.Traces) (ptrace.Traces, error) {
	out := ptrace.NewTraces()
	now := time.Now()
	for traceID, spans := range groupByTrace(td) {
		if p.ic.Has(traceID) {
			spans.ResourceSpans().MoveAndAppendTo(out.ResourceSpans())
			continue
		}
		d, err := p.eval.Evaluate(spans)
		if err != nil {
			p.logger.Warn("policy evaluation error", zap.String("trace_id", traceID), zap.Error(err))
		}
		switch d {
		case evaluator.Sampled:
			p.ingestInteresting(traceID, spans)
			continue
		case evaluator.SampledLocal:
			p.ingestLocal(traceID, spans)
			continue
		}
		if err := p.buf.WriteWithEviction(traceID, spans, now); err != nil {
			p.logger.Error("buffer full after eviction", zap.String("trace_id", traceID), zap.Error(err))
			spans.ResourceSpans().MoveAndAppendTo(out.ResourceSpans())
			continue
		}
	}
	if out.SpanCount() == 0 {
		return out, processorhelper.ErrSkipProcessingData
	}
	return out, nil
}
```

Add `ingestLocal` method (same as `ingestInteresting` but no coordinator notify):

```go
func (p *retroactiveProcessor) ingestLocal(traceID string, current ptrace.Traces) {
	p.ic.Add(traceID)
	buffered, ok, err := p.buf.ReadAndDelete(traceID)
	if err != nil {
		p.logger.Warn("read buffer for local trace", zap.String("trace_id", traceID), zap.Error(err))
		if err2 := p.next.ConsumeTraces(context.Background(), current); err2 != nil {
			p.logger.Error("ingest local trace", zap.String("trace_id", traceID), zap.Error(err2))
		}
		return
	}
	if ok {
		current.ResourceSpans().MoveAndAppendTo(buffered.ResourceSpans())
		current = buffered
	}
	if err := p.next.ConsumeTraces(context.Background(), current); err != nil {
		p.logger.Error("ingest local trace", zap.String("trace_id", traceID), zap.Error(err))
	}
}
```

- [ ] **Step 3: Update `processor_test.go`**

Replace `newTestProcessor` to use `Policies` instead of `Rules`:

```go
func newTestProcessor(t *testing.T, addr string, sink *consumertest.TracesSink) otelprocessor.Traces {
	t.Helper()
	p := evaluator.PolicyCfg{}
	p.Name = "errors"
	p.Type = evaluator.StatusCode
	p.StatusCodeCfg = evaluator.StatusCodeCfg{StatusCodes: []string{"ERROR"}}
	cfg := &processor.Config{
		BufferFile:              filepath.Join(t.TempDir(), "buf.ring"),
		MaxBufferBytes:          100 << 20,
		MaxInterestCacheEntries: 1000,
		CoordinatorEndpoint:     addr,
		Policies:                []evaluator.PolicyCfg{p},
	}
	factory := processor.NewFactory()
	proc, err := factory.CreateTraces(
		context.Background(),
		processortest.NewNopSettings(factory.Type()),
		cfg,
		sink,
	)
	require.NoError(t, err)
	require.NoError(t, proc.Start(context.Background(), nil))
	t.Cleanup(func() { _ = proc.Shutdown(context.Background()) })
	return proc
}
```

Add a test that verifies probabilistic does NOT notify coordinator:

```go
func TestProbabilistic_NoCoordinatorNotify(t *testing.T) {
	fc, addr := startFakeCoordinator(t)
	sink := &consumertest.TracesSink{}

	// Build processor with 100% probabilistic policy only.
	p := evaluator.PolicyCfg{}
	p.Name = "prob"
	p.Type = evaluator.Probabilistic
	p.ProbabilisticCfg = evaluator.ProbabilisticCfg{SamplingPercentage: 100}
	cfg := &processor.Config{
		BufferFile:              filepath.Join(t.TempDir(), "buf.ring"),
		MaxBufferBytes:          100 << 20,
		MaxInterestCacheEntries: 1000,
		CoordinatorEndpoint:     addr,
		Policies:                []evaluator.PolicyCfg{p},
	}
	factory := processor.NewFactory()
	proc, err := factory.CreateTraces(
		context.Background(),
		processortest.NewNopSettings(factory.Type()),
		cfg,
		sink,
	)
	require.NoError(t, err)
	require.NoError(t, proc.Start(context.Background(), nil))
	t.Cleanup(func() { _ = proc.Shutdown(context.Background()) })

	trace := makeTraceWithStatus("aabbccdd11111111aabbccdd11111111", ptrace.StatusCodeOk)
	require.NoError(t, proc.ConsumeTraces(context.Background(), trace))

	// Span ingested immediately (SampledLocal).
	assert.Equal(t, 1, sink.SpanCount())

	// Coordinator must NOT be notified.
	time.Sleep(100 * time.Millisecond)
	fc.mu.Lock()
	notified := len(fc.notified)
	fc.mu.Unlock()
	assert.Equal(t, 0, notified, "probabilistic policy must not notify coordinator")
}
```

Also remove the import of `evaluator.RuleConfig` (no longer used) from `processor_test.go`.

- [ ] **Step 4: Build to catch all errors**

```bash
cd processor/retroactivesampling && go build ./... 2>&1
```

Expected: no errors.

- [ ] **Step 5: Run all tests**

```bash
cd processor/retroactivesampling && go test ./... -timeout 60s 2>&1
```

Expected: all PASS (may take a moment for integration tests).

- [ ] **Step 6: Commit**

```bash
git add processor/retroactivesampling/config.go \
        processor/retroactivesampling/processor.go \
        processor/retroactivesampling/processor_test.go
git commit -m "feat: wire sampling policies — Policies config, ingestLocal, SampledLocal path"
```

---

## Task 11: Final check and TODO update

- [ ] **Step 1: Run full test suite**

```bash
cd processor/retroactivesampling && go test ./... -count=1 -timeout 120s -v 2>&1 | tail -30
```

Expected: all PASS.

- [ ] **Step 2: Verify YAML compatibility with an example config**

Check the existing example config and update it to use the new `policies:` format:

```bash
cat example/otelcol1.yaml
```

If it has `rules:`, update to:

```yaml
processors:
  retroactive_sampling:
    buffer_file: /data/retrosampling1.ring
    max_buffer_bytes: 1073741824
    max_interest_cache_entries: 100000
    coordinator_endpoint: coordinator:9090
    policies:
      - name: errors
        type: status_code
        status_code:
          status_codes: [ERROR]
      - name: slow
        type: latency
        latency:
          threshold_ms: 5000
```

- [ ] **Step 3: Mark TODO complete**

In `TODO.md`, mark the first large item as done:

```markdown
- [x] import sampling policies (ie. evaluation) code from ...
```

- [ ] **Step 4: Final commit**

```bash
git add example/ TODO.md
git commit -m "chore: update example configs and mark sampling policies task complete"
```
