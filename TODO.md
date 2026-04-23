# TODO.md

## Small items

- [ ] coordinator should use a proper logging solution, not log.Printf
- [ ] redis pubsub in coordinator has reduntant New/NewWithReplica, should just be 1 method

## Large items

- [ ] subscribe() taking an anon function in coordinator's pubsub interface is odd, as implementations need to handle multiple calls to subscribe, even though that should never be the case. re-design
- [ ] coordinator in proxy mode should support configs for auth/tls/headers/compression/etc, consider using go.opentelemetry.io/collector/config/configgrpc
- [ ] coordinator should also support HTTP, processor should be able to choose through configs.
