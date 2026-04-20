# Eager Span Evaluation — Design Spec

**Date:** 2026-04-20  
**Status:** Approved

---

## Problem

The current processor waits for `buffer_ttl` quiescence (no new spans for N seconds) before evaluating a trace. This is unnecessary: sampling rules (`error_status`, `high_latency`) are span-level — a single matching span is sufficient to decide. Waiting adds latency and complexity.

---

## Design

### Core change

Evaluation moves from quiescence-triggered (on `buffer_ttl` expiry) to per-batch (on each `ConsumeTraces` call), before any buffer write.

### `processTraces` loop (per traceID group)

```
1. InterestCache hit → pass through immediately (unchanged)
2. Evaluate current batch
3. Interesting:
   a. Cancel drop timer (if running) — prevents onDecision double-ingest
   b. Add to InterestCache (TTL = drop_ttl)
   c. Notify coordinator — safe: no drop timer, cache set, onDecision is no-op
   d. Read accumulated buffer (may be absent), merge with current batch, ingest once
   e. Delete buffer
4. Not interesting:
   a. WriteWithEviction to buffer
   b. Start drop_ttl timer — only if not already running (no reset on subsequent batches)
```

Drop timer is one-shot per trace. New non-interesting batches for a trace that already has a running timer just append to the buffer; the timer keeps counting.

### Coordinator path

Unchanged. `onDecision` still checks `hadDropTimer`; no drop timer means no buffered spans to ingest, so it's a no-op. This is correct: if we found the trace interesting locally, we already ingested and notified.

### Error handling

| Failure | Behavior |
|---|---|
| Buffer read fails (step 3d) | Ingest current batch only, log warning, delete buffer |
| Ingest fails | Log error; buffer already deleted (accepted data loss, same as current) |
| Coordinator notify fails | Log error; ingest already done (fire-and-forget, same as current) |
| WriteWithEviction fails | Log error, pass through (same as current) |

---

## Removed

| Removed | Replaced by |
|---|---|
| `buffer_ttl` config field | — (evaluation is now eager) |
| `interest_cache_ttl` config field | Derived internally from `drop_ttl` |
| `timers` map | — |
| `resetBufferTimer` function | — |
| `onBufferTimeout` function | — |

`InterestCache` itself is kept; its TTL is set to `drop_ttl`.

---

## Config after

```yaml
processors:
  retroactive_sampling:
    buffer_dir: /var/otelcol/retrosampling/
    max_buffer_bytes: 1073741824
    drop_ttl: 30s
    coordinator_endpoint: coordinator:9090
    rules:
      - type: error_status
      - type: high_latency
        threshold: 5s
```

**Breaking change:** `buffer_ttl` and `interest_cache_ttl` fields are removed. Existing configs must drop them.

---

## Testing

- Remove all tests that set/exercise `buffer_ttl` or `interest_cache_ttl`
- Add: interesting first batch (no prior buffer) — verify ingest + notify, no buffer write
- Add: interesting second batch (prior non-interesting buffer) — verify buffer + current batch ingested once, drop timer cancelled
- Add: two non-interesting batches — verify single drop timer, not reset on second batch
- Update integration tests: remove `buffer_ttl` waits; replace with immediate assertion after span ingestion
