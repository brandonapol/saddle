SHELL := bash
.SHELLFLAGS = -e -o pipefail -c
.DEFAULT_GOAL := help
.ONESHELL:
.SILENT:

# .ONESHELL needs GNU Make 3.82+. macOS ships 3.81, which silently ignores it, so
# every recipe line runs in its own shell and multi-line `if` blocks fail with
# confusing syntax errors.
MIN_MAKE := 3.82
ifneq ($(firstword $(sort $(MAKE_VERSION) $(MIN_MAKE))),$(MIN_MAKE))
$(error GNU Make $(MAKE_VERSION) is too old; this Makefile needs $(MIN_MAKE)+. \
  On macOS run `brew install make` and use `gmake`)
endif

# Saddle runs from git hooks and inside agent worktrees. Don't let git's
# per-invocation environment point the recipes' git calls at the wrong checkout.
unexport GIT_DIR
unexport GIT_INDEX_FILE
unexport GIT_WORK_TREE
unexport GIT_PREFIX

# Every $(shell ...) variable is := (simply expanded), so it runs once, not on
# every expansion. $(or ...) lets an environment or command-line override win.
GO        := $(or $(GO),$(shell command -v go))
GOBIN     := $(or $(GOBIN),$(shell $(GO) env GOBIN 2>/dev/null))
GOBIN     := $(or $(GOBIN),$(shell $(GO) env GOPATH 2>/dev/null)/bin)
GO_MOD_VERSION := $(shell awk '/^go /{print $$2; exit}' go.mod)

BIN        := saddle
BUILD_DIR  := ./build
EXE        := $(BUILD_DIR)/$(BIN)
ENTRYPOINT := ./cmd/saddle

# The version is the git description of HEAD: a tag when one points at it,
# otherwise the commit, with -dirty for uncommitted changes. A dev build never
# claims a released version. Override with VERSION=X.Y.Z.
VERSION    := $(or $(VERSION),$(shell git describe --tags --always --dirty 2>/dev/null),dev)
GO_LDFLAGS := -ldflags "-X github.com/brandonapol/saddle/internal/cli.Version=$(VERSION)"

GOLANGCI_LINT_VERSION ?= v2.14.0

##@ Setup

.PHONY: setup
setup: setup/golangci-lint ## Install development tools
	echo "✅ Dev tools installed. Next: make install"

.PHONY: setup/golangci-lint
setup/golangci-lint: ## Install golangci-lint (pinned via GOLANGCI_LINT_VERSION)
	# Built with your local Go. A prebuilt binary compiled with an older Go refuses
	# to lint a module whose go.mod targets a newer one.
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

##@ Development

.PHONY: build
build: ## Build ./build/saddle
	mkdir -p $(BUILD_DIR)
	$(GO) build $(GO_LDFLAGS) -o $(EXE) $(ENTRYPOINT)
	echo "Built $(EXE) ($(VERSION))"

.PHONY: install
install: ## Install saddle into $GOBIN (what spawned agents run)
	$(GO) install $(GO_LDFLAGS) $(ENTRYPOINT)
	echo "Installed $(GOBIN)/$(BIN) ($(VERSION))"
	# Agents run the binary by absolute path, but you will want it on PATH too.
	case ":$$PATH:" in
	    *":$(GOBIN):"*) ;;
	    *) echo "Note: $(GOBIN) is not on your PATH. Add it to run 'saddle' directly." ;;
	esac

.PHONY: run
run: ## Run saddle from source (ARGS="status --json")
	$(GO) run $(GO_LDFLAGS) $(ENTRYPOINT) $(ARGS)

.PHONY: clean
clean: ## Remove build artifacts (never touches .saddle/ state)
	rm -rf $(BUILD_DIR) coverage.out coverage.html

##@ Testing

.PHONY: test
test: ## Run unit and integration tests
	$(GO) test ./...

.PHONY: test/race
test/race: ## Run tests with the race detector
	# The hook, MCP server and CLI share one SQLite file from separate processes,
	# so races here are real bugs.
	$(GO) test -race ./...

.PHONY: test/cover
test/cover: ## Run tests and write coverage.html
	$(GO) test -coverprofile=coverage.out -covermode=atomic ./...
	$(GO) tool cover -html=coverage.out -o coverage.html
	$(GO) tool cover -func=coverage.out | tail -1
	echo "Wrote coverage.html"

##@ Code quality

.PHONY: check
check: check/format check/tidy check/vet test check/lint ## Run every check CI runs

.PHONY: check/format
check/format: ## Fail if any Go file needs gofmt
	unformatted="$$(gofmt -s -l $$(git ls-files '*.go'))"
	if [ -n "$$unformatted" ]; then
	    echo "These files need formatting (run 'make format'):"
	    echo "$$unformatted" | sed 's/^/  /'
	    exit 1
	fi

.PHONY: check/tidy
check/tidy: ## Fail if go.mod or go.sum is not tidy
	if ! $(GO) mod tidy -diff >/dev/null 2>&1; then
	    echo "go.mod/go.sum are not tidy. Run 'make tidy'. Diff:"
	    $(GO) mod tidy -diff || true
	    exit 1
	fi

.PHONY: check/vet
check/vet: ## Run go vet
	$(GO) vet ./...

.PHONY: check/lint
check/lint: ## Run golangci-lint
	if ! command -v golangci-lint >/dev/null 2>&1; then
	    echo "golangci-lint is not installed. Run 'make setup/golangci-lint' first."
	    exit 1
	fi
	golangci-lint run ./...

.PHONY: format
format: ## Format Go code
	gofmt -s -w $$(git ls-files '*.go')

.PHONY: tidy
tidy: ## Tidy go.mod and go.sum
	$(GO) mod tidy

.PHONY: fix
fix: tidy format ## Tidy modules and format code
	golangci-lint run --fix ./... || true

##@ Helpers

.PHONY: version
version: ## Print the version a build would stamp
	echo "$(VERSION) (go $(GO_MOD_VERSION) per go.mod)"

.PHONY: help
help: ## Show this help
	awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
	    /^[a-zA-Z0-9_\/-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 } \
	    /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)
	echo
