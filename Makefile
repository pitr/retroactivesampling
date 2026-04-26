.PHONY: build test test-integration bench lint proto collector clean generate

COORDINATOR_BIN := bin/coordinator
TRACEGEN_BIN    := bin/tracegen
COLLECTOR_BIN   := bin/otelcol-retrosampling

COORDINATOR_SRC  := $(shell find coordinator -name '*.go')
TRACEGEN_SRC     := $(shell find cmd/tracegen -name '*.go')
PROCESSOR_SRC    := $(shell find processor/retroactivesampling -name '*.go')
PROTO_GO_SRC     := $(shell find proto -name '*.go')

METADATA_YAML      := processor/retroactivesampling/metadata.yaml
GENERATED_METADATA := processor/retroactivesampling/internal/metadata/generated_status.go

$(GENERATED_METADATA): $(METADATA_YAML)
	cd processor/retroactivesampling && mdatagen metadata.yaml

generate: $(GENERATED_METADATA)

build: $(COORDINATOR_BIN) $(TRACEGEN_BIN)

$(COORDINATOR_BIN): $(COORDINATOR_SRC) $(PROTO_GO_SRC) coordinator/go.mod coordinator/go.sum
	go -C coordinator build -o ../$(COORDINATOR_BIN) .

$(TRACEGEN_BIN): $(TRACEGEN_SRC) cmd/tracegen/go.mod cmd/tracegen/go.sum
	go -C cmd/tracegen build -o ../../$(TRACEGEN_BIN) .

test:
	go -C proto test ./... -timeout 30s
	go -C coordinator test ./... -timeout 30s
	go -C processor/retroactivesampling test ./... -timeout 30s

bench:
	go -C coordinator test -run ^$$ -bench . -benchmem -v ./...
	go -C processor/retroactivesampling test -run ^$$ -bench . -benchmem -v ./...

test-integration:
	go -C proto test -tags integration ./... -timeout 120s
	go -C coordinator test -tags integration ./... -timeout 120s
	go -C processor/retroactivesampling test -tags integration ./... -timeout 120s

proto: proto/coordinator.proto
	protoc --go_out=proto --go_opt=paths=source_relative \
	       --go-grpc_out=proto --go-grpc_opt=paths=source_relative \
	       -I proto proto/coordinator.proto

collector: $(COLLECTOR_BIN)

$(COLLECTOR_BIN): example/build.yaml $(PROCESSOR_SRC) $(PROTO_GO_SRC)
	GOWORK=off builder --config example/build.yaml

lint:
	cd proto && golangci-lint run ./...
	cd coordinator && golangci-lint run ./...
	cd processor/retroactivesampling && golangci-lint run ./...
	cd cmd/tracegen && golangci-lint run ./...

clean:
	rm -rf bin/ data/

.DEFAULT_GOAL := build
