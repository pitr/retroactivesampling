# TODO.md

## Small items

- [ ] tail sampling processor keeps component.Host for some reason, see if our processor needs it also.
- [ ] update coordinator readme with performance info. Say that the traffic it receives is on multiple orders lower than span traffic.
- [ ] measure interesting traces per second in coordinator (since it knows when a new unique interesting trace is found)
- [ ] coordinator Broadcast holds the mutex for the full duration of all stream.Send() calls; a single slow (backpressured) stream stalls the entire coordinator. Replace with per-stream goroutine + buffered channel pattern so Broadcast only does non-blocking channel pushes under the lock, matching the pattern already used in the processor client's sendCh. Update risks in PERFORMANCE.md when done.
- [ ] coordinator Broadcast silently drops send errors (`_ = stream.Send(msg)`); add a Prometheus counter for failed/dropped sends per stream for observability.
- [ ] coordinator redis_addr config only supports a single address; support read replicas so coordinators can subscribe to replicas instead of the primary, distributing Redis outbound fan-out (I × M × C total) across replica nodes.

## Large items

- [ ] evaluate coordinator design against high load (10k collectors, 10k interesting traces per second)
- [ ] re-analyze architecture of processor and coordinator. What are downsides of this approach vs tail sampling processor? How can they be fixed or mitigated?
- [ ] coordinator_endpoint string config in processor should instead be go.opentelemetry.io/collector/config/configgrpc
- [ ] include tail sampling processor in examples/ so that they can be compared by a manual test. Crete a separate otelcol config that does single collector tail sampling, use the same policies as the other configs. All ports should be different.
