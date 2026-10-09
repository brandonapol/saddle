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

# go test's -timeout for the whole e2e suite (E2E_TIMEOUT is the per-wait
# timeout the journeys read). A race build (E2E_RACE=1) runs them
# several times slower, so the nightly raises it.
E2E_GO_TIMEOUT := $(or $(E2E_GO_TIMEOUT),15m)

# go test's -timeout for each package's test binary (Go's default is 10m).
# internal/app, the slowest, takes about 75s idle but several times that at
# load average 60+, when several agents run `make check` at once.
TEST_TIMEOUT := $(or $(TEST_TIMEOUT),20m)

# go_test runs go test with $(1), then repeats the failing packages and tests
# at the end. Gates keep only the output's last lines, where other packages'
# ok lines or a timeout's goroutine dump would otherwise hide what failed.
define go_test
	log="$$(mktemp)"
	trap 'rm -f "$$log"' EXIT
	if $(GO) test $(1) 2>&1 | tee "$$log"; then exit 0; fi
	echo
	echo "make $@ FAILED:"
	{
	    grep -E '^FAIL[[:space:]]' "$$log" || echo "(no FAIL line: see the errors above)"
	    grep -E '^panic: test timed out' "$$log" || true
	    grep -E -- '--- FAIL' "$$log" || true
	} | head -n 30
	exit 1
endef

# The version is the git description of HEAD: a tag when one points at it,
# otherwise the commit, with -dirty for uncommitted changes. A dev build never
# claims a released version. Override with VERSION=X.Y.Z.
VERSION    := $(or $(VERSION),$(shell git describe --tags --always --dirty 2>/dev/null),dev)
GO_LDFLAGS := -ldflags "-X github.com/brandonapol/saddle/internal/cli.Version=$(VERSION)"

GOLANGCI_LINT_VERSION ?= v2.14.0

# gotreesitter embeds every grammar (~30MB of blobs) unless built with
# grammar_subset plus one grammar_subset_<lang> tag per language we use. Measured
# on a binary importing internal/gitx/symbols: 21.9MB untagged, 4.8MB tagged.
# Build tags beat lazy-loading: no code, and the tag list sits next to the
# languages in symbols.Default. Adding a language there needs its tag here;
# TestSupportedLanguagesLoad fails under `make test` if one is missing.
# Used by every target that compiles, so vet/lint/test see the same build.
GO_TAGS    := grammar_subset grammar_subset_go grammar_subset_python
GO_TAGFLAG := -tags '$(GO_TAGS)'

##@ Setup

.PHONY: setup
setup: setup/golangci-lint setup/tokens ## Install dev tools and set up missing credentials
	echo "✅ Setup done. Next: make install"

.PHONY: setup/tokens
setup/tokens: ## Prompt for a Jev key and Claude login, only if missing
	# Writes `export ...` lines to your shell rc (SHELL_RC overrides which file).
	# Never prints a token; skips prompts without a terminal.
	./scripts/setup-tokens.bash

.PHONY: setup/golangci-lint
setup/golangci-lint: ## Install golangci-lint (pinned via GOLANGCI_LINT_VERSION)
	# Built with your local Go. A prebuilt binary compiled with an older Go refuses
	# to lint a module whose go.mod targets a newer one.
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

##@ Development

.PHONY: build
build: ## Build ./build/saddle
	mkdir -p $(BUILD_DIR)
	$(GO) build $(GO_TAGFLAG) $(GO_LDFLAGS) -o $(EXE) $(ENTRYPOINT)
	echo "Built $(EXE) ($(VERSION))"

.PHONY: install
install: ## Install saddle into $GOBIN (what spawned agents run)
	$(GO) install $(GO_TAGFLAG) $(GO_LDFLAGS) $(ENTRYPOINT)
	echo "Installed $(GOBIN)/$(BIN) ($(VERSION))"
	# Agents run the binary by absolute path, but you will want it on PATH too.
	case ":$$PATH:" in
	    *":$(GOBIN):"*) ;;
	    *) echo "Note: $(GOBIN) is not on your PATH. Add it to run 'saddle' directly." ;;
	esac

.PHONY: upgrade
upgrade: ## Fast-forward main from origin and reinstall (refuses if not on a clean main)
	./scripts/upgrade.sh

.PHONY: run
run: ## Run saddle from source (ARGS="status --json")
	$(GO) run $(GO_TAGFLAG) $(GO_LDFLAGS) $(ENTRYPOINT) $(ARGS)

.PHONY: clean
clean: ## Remove build artifacts (never touches .saddle/ state)
	rm -rf $(BUILD_DIR) coverage.out coverage.html

##@ Testing

.PHONY: test
test: ## Run unit and integration tests (TEST_TIMEOUT per package, default 20m)
	$(call go_test,$(GO_TAGFLAG) -timeout $(TEST_TIMEOUT) ./...)

.PHONY: test/e2e
test/e2e: ## Run the end-to-end journeys (RUN=Journey to pick some; needs tmux)
	# Builds saddle, a fake gh and a fake agent, then drives the real binary in
	# hermetic temp repos with a private tmux server. See internal/e2e.
	# E2E_RACE=1 also race-builds saddle itself (slow: minutes, not seconds).
	E2E_GO_TAGS='$(GO_TAGS)' $(GO) test -tags '$(GO_TAGS) e2e' -race -count=1 -timeout $(E2E_GO_TIMEOUT) $(if $(RUN),-run '$(RUN)') ./internal/e2e/...

.PHONY: test/scripts
test/scripts: ## Test the shell scripts (scripts/upgrade.sh)
	bash scripts/upgrade_test.sh

.PHONY: test/race
test/race: ## Run tests with the race detector
	# The hook, MCP server and CLI share one SQLite file from separate processes,
	# so races here are real bugs.
	$(call go_test,$(GO_TAGFLAG) -race -timeout $(TEST_TIMEOUT) ./...)

.PHONY: bench
bench: ## Run benchmarks (BENCH=Status to pick some)
	# Status and the TUI's task read run every second; their benchmarks seed 10
	# and 100 landed tasks so cost that grows with history shows up.
	$(GO) test $(GO_TAGFLAG) -run '^$$' -bench '$(or $(BENCH),.)' -benchmem ./...

.PHONY: test/cover
test/cover: ## Run tests and write coverage.html
	$(GO) test $(GO_TAGFLAG) -coverprofile=coverage.out -covermode=atomic ./...
	$(GO) tool cover -html=coverage.out -o coverage.html
	$(GO) tool cover -func=coverage.out | tail -1
	echo "Wrote coverage.html"

##@ Code quality

.PHONY: check
check: check/format check/tidy check/vet test test/e2e test/scripts check/lint ## Run every check CI runs

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
	$(GO) vet $(GO_TAGFLAG) ./...

.PHONY: check/lint
check/lint: ## Run golangci-lint
	if ! command -v golangci-lint >/dev/null 2>&1; then
	    echo "golangci-lint is not installed. Run 'make setup/golangci-lint' first."
	    exit 1
	fi
	golangci-lint run --build-tags "$(GO_TAGS)" ./...

.PHONY: format
format: ## Format Go code
	gofmt -s -w $$(git ls-files '*.go')

.PHONY: tidy
tidy: ## Tidy go.mod and go.sum
	$(GO) mod tidy

.PHONY: fix
fix: tidy format ## Tidy modules and format code
	golangci-lint run --build-tags "$(GO_TAGS)" --fix ./... || true

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
