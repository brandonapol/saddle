BIN     := saddle
PKG     := ./cmd/saddle
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/brandonapol/saddle/internal/cli.Version=$(VERSION)

.PHONY: all build install test race lint fmt vet tidy check clean

all: check build

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) $(PKG)

install:
	go install -ldflags "$(LDFLAGS)" $(PKG)

test:
	go test ./...

race:
	go test -race ./...

lint:
	golangci-lint run ./...

fmt:
	gofmt -w .

vet:
	go vet ./...

tidy:
	go mod tidy

# What CI runs.
check: vet test lint

clean:
	rm -f $(BIN)
