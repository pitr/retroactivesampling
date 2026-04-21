# TODO.md

## Small items

- [ ] cmd/tracegen struggles to generate high rate traffic beyond `-rate=10000 -service=20`
- [ ] update readme with development section (what processes to run, how to install necessary tools like otelcol builder, how to run tests, etc)
- [x] coordinator should have graceful shut down (and not print `publish X: context canceled` forever)
- [ ] see if we can get rid of go.work in the root. Either replace it with go.mod or not have it at all, depending which works best for a go project like this.

## Large items

- [ ] import sampling policies (ie. evaluation) code from https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/processor/tailsamplingprocessor so our processor can be a drop in replacement for it. Only support sampling policies that work on partial spans, so span_count, rate_limiting and bytes_limiting (maybe others) are not supported.
- [ ] evaluate coordinator design against high load (10k collectors, 10k interesting traces per second)
