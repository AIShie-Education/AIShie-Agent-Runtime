# The Makefile is the single source of truth for how the runtime is built and
# checked. CI calls these targets and nothing else, so `make ci` on a laptop
# and a green pipeline mean the same thing.

SHELL       := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

BIN     := bin/aishie-runtime
PKG     := github.com/AIShiteru-LMS/AIShie-Agent-Runtime
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X $(PKG)/internal/version.Version=$(VERSION) \
	-X $(PKG)/internal/version.Commit=$(COMMIT) \
	-X $(PKG)/internal/version.Date=$(DATE)

# Maintenance database for the store's Postgres tests; the role must be able
# to CREATE DATABASE. The default reaches a local server over its unix socket.
TEST_DATABASE_URL ?= postgres:///postgres
export TEST_DATABASE_URL

.PHONY: help
help:
	@grep -E '^[a-z][a-z0-9-]*:.*##' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-16s %s\n", $$1, $$2}'

.PHONY: build
build: ## compile the runtime into bin/
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/aishie-runtime

.PHONY: test
test: ## unit and integration tests: the fake Core, the scripted models, the store on TEST_DATABASE_URL
	go test -race -shuffle=on -coverprofile=cover.out ./...

.PHONY: e2e
e2e: build ## the runtime against the pinned Core (scripts/ci-core.sh), with a scripted model
	scripts/e2e.sh

.PHONY: live
live: ## the adapters against the real providers whose keys are set (OPENAI_API_KEY, ANTHROPIC_API_KEY, GEMINI_API_KEY, ...)
	LIVE=1 go test -count=1 -run Live ./internal/llm/...

.PHONY: record-fixtures
# Record against a Core started with CORE_RATE_LIMIT_PER_MINUTE=600 and
# CORE_PROPOSAL_TTL=20s (scripts/ci-core.sh), and RECORD_PROPOSAL_TTL=20s
# here: without them the 429 and the expired proposal are not recorded again,
# and keep their old fixtures.
record-fixtures: ## record the fake Core's fixtures from a live Core (E2E_CORE_URL, E2E_ROOT_TOKEN)
	RECORD_FIXTURES=1 go test -count=1 -run TestRecordFixtures ./internal/fakecore/

.PHONY: fmt-check
fmt-check: ## fail if any file needs gofmt
	@out=$$(gofmt -l .); [ -z "$$out" ] || { echo "gofmt needed:"; echo "$$out"; exit 1; }

.PHONY: tidy-check
tidy-check: ## fail if go.mod or go.sum is not tidy
	go mod tidy -diff

# actionlint also runs shellcheck over every `run:` block when shellcheck is
# installed, as it is on GitHub's runners.
ACTIONLINT_VERSION ?= v1.7.12

.PHONY: actionlint
actionlint: ## the GitHub Actions workflows
	go run github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION)

.PHONY: lint
lint: fmt-check tidy-check actionlint ## gofmt, go mod tidy, the workflows, go vet, golangci-lint
	go vet ./...
	golangci-lint run
	@if command -v shellcheck >/dev/null; then shellcheck scripts/*.sh; else echo "shellcheck is not installed: scripts/ not checked"; fi

.PHONY: script-test
script-test: ## the tests of scripts/ and deploy/, and shellcheck over deploy/ where it is installed
	scripts/release-notes_test.sh
	deploy/aishie-runtime-deploy_test.sh
	@if command -v shellcheck >/dev/null; then shellcheck -s sh deploy/aishie-runtime-deploy deploy/aishie-runtime deploy/setup-server.sh && shellcheck deploy/aishie-runtime-deploy_test.sh; else echo "shellcheck is not installed: deploy/ not checked"; fi

.PHONY: vuln
vuln: ## known vulnerabilities in dependencies
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

.PHONY: docker
docker: ## build the image locally; never pushes
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) -t aishie-runtime:dev .

.PHONY: ci
ci: lint script-test test e2e ## everything CI runs, except docker and vuln

.PHONY: clean
clean: ## remove build output
	rm -rf bin dist cover.out

.PHONY: clean-testdb
clean-testdb: ## drop stray test databases
	@psql -X -At -d postgres -c "SELECT datname FROM pg_database WHERE datname ~ '^aishie_.*_t_'" \
		| while read -r d; do echo "drop $$d"; dropdb --if-exists --force "$$d"; done
