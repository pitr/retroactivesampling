# TODO.md

## Small items

## Large items

- coordinator: subscribe() taking an anon function in pubsub interface is odd, as implementations need to handle multiple calls to subscribe, even though that should never be the case. re-design. locks on `p.handlers` are just silly.
- coordinator: in proxy mode should support configs for auth/tls/headers/compression/etc, consider using go.opentelemetry.io/collector/config/configgrpc
- coordinator: support HTTP in addition to GRPC, processor should be able to choose through configs.
- processor: the use of go.opentelemetry.io/collector/processor/processorhelper results in wrong ProcessorOutgoingItems metric as it undercounts (does not include async publishes). Fix it
