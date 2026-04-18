.PHONY: build test test-integration proto collector clean

COORDINATOR_BIN := bin/coordinator
COLLECTOR_BIN   := bin/otelcol-retrosampling

build: $(COORDINATOR_BIN)

$(COORDINATOR_BIN):
	go -C coordinator build -o ../$(COORDINATOR_BIN) .

test:
	go -C proto test ./... -timeout 30s
	go -C coordinator test ./... -timeout 30s
	go -C processor/retroactivesampling test ./... -timeout 30s

test-integration:
	go -C proto test -tags integration ./... -timeout 120s
	go -C coordinator test -tags integration ./... -timeout 120s
	go -C processor/retroactivesampling test -tags integration ./... -timeout 120s

proto:
	protoc --go_out=proto --go_opt=paths=source_relative \
	       --go-grpc_out=proto --go-grpc_opt=paths=source_relative \
	       -I proto proto/coordinator.proto

collector: example/build.yaml
	ocb --config example/build.yaml

clean:
	rm -rf bin/

.DEFAULT_GOAL := build
