# Validation

After changing processor code, run `scripts/validate.sh -stop 60` and review the output before claiming the change works. The script rebuilds the collector, manages both collector processes, and prints tracegen's sampling quality report.

Key metrics to read:
- `tracegen_traces_received_total{completeness="full/partial/none"}` — full = cross-collector coordination succeeded; partial = some spans missed; none = trace not sampled at all
- `tracegen_spans_received_total{reason="non-error"}` — false positives; should be zero

Coordinator must be running on `localhost:9090`. Pass `-stop N` to shorten the run (e.g. `-stop 20` for a quick check).
