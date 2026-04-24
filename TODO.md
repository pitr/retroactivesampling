# TODO.md

## Small items

- [ ] move all span marshalling and unmarshalling out of buffer.go, that code is on a hot path. buffer should operate with byte arrays only

## Large items

- [ ] subscribe() taking an anon function in coordinator's pubsub interface is odd, as implementations need to handle multiple calls to subscribe, even though that should never be the case. re-design. locks on `p.handlers` are just silly.
- [ ] coordinator in proxy mode should support configs for auth/tls/headers/compression/etc, consider using go.opentelemetry.io/collector/config/configgrpc
- [ ] coordinator should also support HTTP, processor should be able to choose through configs.
- [ ] use of go.opentelemetry.io/collector/processor/processorhelper in our processor results in wrong ProcessorOutgoingItems metric as it undercounts (does not include async publishes). Fix it
