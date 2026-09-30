BINARY      ?= ocellusai-mcp
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE       ?= ocellusai-mcp:$(VERSION)
CONFIG      ?= config.example.yaml
LDFLAGS     := -s -w -X main.version=$(VERSION)

.PHONY: all build run validate test test-race vet lint fmt docker clean

all: vet test build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/ocellusai-mcp

run: build
	./bin/$(BINARY) -config $(CONFIG)

## validate: load config + tool catalog and print the tools (fails fast on bad YAML)
validate: build
	./bin/$(BINARY) -config $(CONFIG) -validate

test:
	go test -count=1 ./...

test-race:
	go test -race -count=1 ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

## lint: needs golangci-lint (https://golangci-lint.run/welcome/install/)
lint:
	golangci-lint run ./...

docker:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

clean:
	rm -rf bin
