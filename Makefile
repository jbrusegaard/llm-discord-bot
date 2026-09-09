GO      ?= go
BINARY  := discord-bot
BIN_DIR := bin

# Build metadata, injected via -ldflags.
VERSION   := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT    := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILD_DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS   := -s -w \
             -X main.version=$(VERSION) \
             -X main.commit=$(COMMIT) \
             -X main.buildDate=$(BUILD_DATE)

# Optional cross-compilation: make build GOOS=linux GOARCH=amd64
# (safe: the project uses pure-Go dependencies, so CGO is not required)

.PHONY: all build run test tidy clean

all: build

## build: produce a shippable static binary at bin/discord-bot
build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(BINARY) .
	@file $(BIN_DIR)/$(BINARY) 2>/dev/null || true

## run: run from source (development)
run:
	$(GO) run .

## test: unit tests
test:
	$(GO) test ./...

## tidy: sync go.mod / go.sum
tidy:
	$(GO) mod tidy

## clean: remove build artifacts
clean:
	rm -rf $(BIN_DIR)
