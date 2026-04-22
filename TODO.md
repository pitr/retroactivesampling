# TODO.md

## Small items

- [x] cmd/tracegen struggles to generate high rate traffic beyond `-rate=10000 -service=20`
- [ ] Rework README files. Fix mistakes and out of date info. Coordinator should have its own README on how to run it. Root README should have user facing docs (they need both need coordinator and processor) with links to the corresponding READMEs. Processor doc should show how to include it in own otelcol builder and how to include it. Add DEVELOPMENT.md with development instructions (what processes to run, how to install necessary tools like otelcol builder, how to run tests, etc) and instructions about example folder.

## Large items

- [ ] import sampling policies (ie. evaluation) code from https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/processor/tailsamplingprocessor so our processor can be a drop in replacement for it. Only support sampling policies that work on partial spans, so span_count, rate_limiting and bytes_limiting (maybe others) are not supported.
- [ ] evaluate coordinator design against high load (10k collectors, 10k interesting traces per second)
- [ ] re-analyze architecture of processor and coordinator. What are downsides of this approach vs tail sampling processor? How can they be fixed or mitigated?
