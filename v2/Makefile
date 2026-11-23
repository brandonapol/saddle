SHELL := bash
.SHELLFLAGS = -e -o pipefail -c
.DEFAULT_GOAL := help

GO      := $(or $(GO),$(shell command -v go))
BIN     := saddle2
VERSION := $(shell git describe --always --dirty 2>/dev/null || echo dev)

.PHONY: help
help: ## List targets
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_\/-]+:.*##/ {printf "  %-14s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: build
build: ## Build ./build/saddle2
	$(GO) build -ldflags "-X main.version=$(VERSION)" -o build/$(BIN) ./cmd/saddle

.PHONY: install
install: ## Install saddle2 into GOBIN
	$(GO) build -ldflags "-X main.version=$(VERSION)" -o "$$($(GO) env GOPATH)/bin/$(BIN)" ./cmd/saddle

.PHONY: check
check: check/format check/tidy check/vet test check/lint ## Everything CI runs for v2

.PHONY: check/format
check/format: ## Fail if a Go file needs gofmt
	@unformatted="$$(gofmt -s -l .)"; \
	if [ -n "$$unformatted" ]; then echo "These files need gofmt -s:"; echo "$$unformatted"; exit 1; fi

.PHONY: check/tidy
check/tidy: ## Fail if go.mod or go.sum is not tidy
	$(GO) mod tidy -diff

.PHONY: check/vet
check/vet: ## go vet
	$(GO) vet ./...

.PHONY: test
test: ## Tests with the race detector, including the size and one-store guards
	$(GO) test -race ./...

.PHONY: check/lint
check/lint: ## golangci-lint (install with make setup/golangci-lint in the repo root)
	@command -v golangci-lint >/dev/null 2>&1 || { echo "golangci-lint is not installed. Run 'make setup/golangci-lint' in the repo root."; exit 1; }
	golangci-lint run ./...
