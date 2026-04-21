.PHONY: build test test-integration lint proto collector clean

COORDINATOR_BIN := bin/coordinator
COLLECTOR_BIN   := bin/otelcol-retrosampling
TRACEGEN_BIN    := bin/tracegen

COORDINATOR_SRC := $(shell find coordinator -name '*.go')
TRACEGEN_SRC    := $(shell find cmd/tracegen -name '*.go')

build: $(COORDINATOR_BIN) $(TRACEGEN_BIN)

$(COORDINATOR_BIN): $(COORDINATOR_SRC) coordinator/go.mod coordinator/go.sum
	go -C coordinator build -o ../$(COORDINATOR_BIN) .

$(TRACEGEN_BIN): $(TRACEGEN_SRC) cmd/tracegen/go.mod cmd/tracegen/go.sum
	go -C cmd/tracegen build -o ../../$(TRACEGEN_BIN) .

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
	GOWORK=off builder --config example/build.yaml

lint:
	cd proto && golangci-lint run ./...
	cd coordinator && golangci-lint run ./...
	cd processor/retroactivesampling && golangci-lint run ./...
	cd cmd/tracegen && golangci-lint run ./...

clean:
	rm -rf bin/

.DEFAULT_GOAL := build
