# TODO.md

## Small items


## Large items

- [ ] otelcol_retroactive_sampling_buffer_span_age_on_eviction_milliseconds_bucket in one collector starts reporting really low eviction durations after a few hours, and traces ingested are incomplete (spans from that collector)
- [ ] subscribe() taking an anon function in coordinator's pubsub interface is odd, as implementations need to handle multiple calls to subscribe, even though that should never be the case. re-design. locks on `p.handlers` are just silly.
- [ ] coordinator in proxy mode should support configs for auth/tls/headers/compression/etc, consider using go.opentelemetry.io/collector/config/configgrpc
- [ ] coordinator should also support HTTP, processor should be able to choose through configs.
- [ ] use of go.opentelemetry.io/collector/processor/processorhelper in our processor results in wrong ProcessorOutgoingItems metric as it undercounts (does not include async publishes). Fix it
