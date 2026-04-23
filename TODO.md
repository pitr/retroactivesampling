# TODO.md

## Small items

- [ ] with a batch-related change to coordinator.proto, does the 20 byte assumption in PERFORMANCE.md still make sense?

## Large items

- [ ] subscribe() taking an anon function in coordinator's pubsub interface is odd, as implementations need to handle multiple calls to subscribe, even though that should never be the case. re-design. locks on `p.handlers` are just silly.
- [ ] coordinator in proxy mode should support configs for auth/tls/headers/compression/etc, consider using go.opentelemetry.io/collector/config/configgrpc
- [ ] coordinator should also support HTTP, processor should be able to choose through configs.
