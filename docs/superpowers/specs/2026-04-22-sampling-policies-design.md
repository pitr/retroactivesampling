# Sampling Policies Import — Design Spec

**Date:** 2026-04-22
**Status:** Approved

---

## Goal

Import sampling policy implementations from `opentelemetry-collector-contrib/processor/tailsamplingprocessor` so our processor can be used as a drop-in replacement. Only policies that work on partial spans are supported. Config YAML must be compatible with the upstream processor's `policies:` format.

---

## Config Changes

Replace `Rules []evaluator.RuleConfig` with `Policies []PolicyCfg` in the processor `Config` struct. This is a breaking change: `rules:` → `policies:` in YAML.

`PolicyCfg` and all sub-config structs (`LatencyCfg`, `StatusCodeCfg`, `StringAttributeCfg`, etc.) are copied from the upstream `config.go` verbatim, preserving all `mapstructure` tags for YAML compatibility. They live in our `internal/evaluator` package — we do not import the upstream module.

```yaml
# upstream-compatible config
policies:
  - name: errors
    type: status_code
    status_code:
      status_codes: [ERROR]
  - name: slow
    type: latency
    latency:
      threshold_ms: 5000
  - name: probabilistic-10pct
    type: probabilistic
    probabilistic:
      sampling_percentage: 10
```

---

## Evaluator Interface

```go
type Decision uint8

const (
    NotSampled  Decision = iota
    Sampled               // interesting — notify coordinator
    SampledLocal          // interesting — skip coordinator (deterministic by trace ID)
    Dropped               // explicitly halt chain, do not sample
)

type Evaluator interface {
    Evaluate(ptrace.Traces) (Decision, error)
}
```

`Chain` evaluates policies in order:
- `NotSampled` → continue to next policy
- `Sampled` or `SampledLocal` → halt, return immediately (trace is interesting)
- `Dropped` → halt, return immediately (trace is explicitly dropped; no subsequent policy can override)
- non-nil error → halt, return `(NotSampled, err)`

The processor call site:

```go
d, err := p.eval.Evaluate(spans)
if err != nil {
    p.logger.Warn("policy evaluation error", zap.String("trace_id", traceID), zap.Error(err))
}
switch d {
case evaluator.Sampled:
    p.ingestInteresting(traceID, spans)  // flushes buffer + notifies coordinator
case evaluator.SampledLocal:
    p.ingestLocal(traceID, spans)        // flushes buffer, no coordinator notify
}
```

---

## Probabilistic Policy — Special Handling

The `probabilistic` policy is deterministic by trace ID: all collectors with the same config make the same keep/drop decision for a given trace. Therefore it returns `SampledLocal` — the processor ingests buffered spans directly without broadcasting to the coordinator.

Trace ID is extracted from the first span in the batch (guaranteed to be present and correct since `groupByTrace` groups by trace ID):

```go
tid := t.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0).TraceID()
```

Compound policies (`and`, `not`) that contain `probabilistic` as a sub-policy return `Sampled` (not `SampledLocal`), because their result may depend on which spans were seen by a given collector.

---

## Policy Implementations

Clone `opentelemetry-collector-contrib` locally. Copy `processor/tailsamplingprocessor/internal/sampling/*.go` into `internal/evaluator/`. Adapt each file:

- Remove `ctx context.Context`, `traceID pcommon.TraceID`, `*TraceData` params
- Replace `trace.ReceivedBatches` with `t ptrace.Traces` (direct arg)
- Change return `(samplingpolicy.Decision, error)` → `(Decision, error)` (our type)
- Drop `IsStateful()` method
- Replace `component.TelemetrySettings` with `*zap.Logger`

### Supported policies

| Policy type | Notes |
|---|---|
| `always_sample` | Returns `Sampled` |
| `latency` | Any span exceeds `threshold_ms` (or within range) |
| `status_code` | Any span matches one of the configured codes |
| `string_attribute` | Any span attribute matches (exact or regex) |
| `numeric_attribute` | Any span attribute in numeric range |
| `boolean_attribute` | Any span attribute matches bool value |
| `probabilistic` | Hash of trace ID; returns `SampledLocal` |
| `trace_state` | Any span trace state key matches |
| `trace_flags` | Any span has sampled flag set |
| `ottl_condition` | OTTL expressions on spans/span events |
| `and` | All sub-policies must match; returns `Sampled` |
| `not` | Inverts sub-policy; returns `Sampled` or `NotSampled` |
| `drop` | Returns `Dropped` — halts chain, trace is not sampled |

### Not supported (require complete trace)

`span_count`, `rate_limiting`, `bytes_limiting`, `composite` — return an error from `Build` if configured.

### Files to copy and adapt

```
internal/sampling/always_sample.go
internal/sampling/latency.go
internal/sampling/status_code.go
internal/sampling/string_attribute.go
internal/sampling/numeric_attribute.go
internal/sampling/boolean_attribute.go
internal/sampling/probabilistic.go
internal/sampling/trace_state.go
internal/sampling/trace_flags.go
internal/sampling/ottl_condition.go
internal/sampling/and.go
internal/sampling/not.go
internal/sampling/drop.go
internal/sampling/span_and_scope_samplers.go  (hasSpanWithCondition helper)
```

Existing `error.go` and `latency.go` in our evaluator are deleted — `status_code` with `status_codes: [ERROR]` replaces `error_status`; upstream `latency` replaces our `high_latency`.

---

## Registry

```go
// Build constructs a Chain from a slice of PolicyCfg.
// Returns error if an unsupported policy type is configured.
func Build(logger *zap.Logger, policies []PolicyCfg) (Chain, error)
```

Handles `and`/`not` recursively. `not` sub-policy reuses `sharedPolicyCfg`. `and` sub-policies are `[]AndSubPolicyCfg` which embeds `sharedPolicyCfg`.

---

## OTTL Dependencies

Add to `processor/retroactivesampling/go.mod`:

```
github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl latest
github.com/open-telemetry/opentelemetry-collector-contrib/internal/filter latest
```

---

## Error Handling

OTTL's `error_mode` config (`propagate` / `ignore` / `silent`) is preserved from upstream:
- `propagate`: `ottl_condition.Evaluate` returns the error; `Chain` stops and returns `(NotSampled, err)`
- `ignore` / `silent`: error handled internally, returns `(NotSampled, nil)`

The processor logs evaluation errors as warnings and treats the trace as not interesting (remains buffered).

---

## Testing

- Unit test each new policy in isolation using `ptrace.Traces` fixtures
- Update `registry_test.go` to cover all supported policy types and the unsupported-type error
- Integration test: verify `status_code: [ERROR]` behaves identically to old `error_status`; verify `probabilistic` does not trigger coordinator notification
