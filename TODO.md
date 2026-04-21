# TODO.md

## Small items

- [ ] optimize proto/coordinator.proto - use bytes instead of string, remove useless `keep`
- [ ] coordinator should try not to notify collector that gave it the message
- [ ] ensure "2 interesting spans in same trace" is handled
- [ ] track as a metric the average time span lives on disk, based on data in sweepOneLocked
- [x] replace buffer_dir in processor config with buffer_file or something, since we only ever need a single file
- [x] switch processor capability to MutatesData=false
- [ ] cmd/tracegen struggles to generate high rate traffic beyond `-rate=1000 -service=20`
- [x] migrate to range over `All()` when traversing telemetry data in processor

## Large items
