# TODO.md

## Small items

- [ ] cmd/tracegen struggles to generate high rate traffic beyond `-rate=1000 -service=20`
- [ ] update readme with development section (what processes to run, how to install necessary tools like otelcol builder, how to run tests, etc)
- [x] optimize groupByTrace in split.go
- [ ] coordinator should have graceful shut down (and not print `publish X: context canceled` forever)
- [ ] see if we can get rid of go.work in the root. Either replace it with go.mod or not have it at all, depending which works best for a go project like this.
- [ ] time.Since and time.Now are slow in processor under high load, take up 3%. Can we speed it up?

## Large items

- [ ] import sampling policies (ie. evaluation) code from https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/processor/tailsamplingprocessor so our processor can be a drop in replacement for it. Only support sampling policies that work on partial spans, so span_count, rate_limiting and bytes_limiting (maybe others) are not supported.
