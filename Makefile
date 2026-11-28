# The Makefile is the single source of truth for local and CI builds of the
# Go control plane and the Node 22 TypeScript runner.

SHELL       := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

BIN     := bin/aishie-runtime
PKG     := github.com/AIShie-Education/AIShie-Agent-Runtime
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X $(PKG)/internal/version.Version=$(VERSION) \
	-X $(PKG)/internal/version.Commit=$(COMMIT) \
	-X $(PKG)/internal/version.Date=$(DATE)
NPM     ?= npm

TEST_DATABASE_URL ?= postgres:///postgres
export TEST_DATABASE_URL

.PHONY: help
help:
	@grep -E '^[a-z][a-z0-9-]*:.*##' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-16s %s\n", $$1, $$2}'

.PHONY: build
build: ## compile the Go control plane into bin/
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/aishie-runtime

.PHONY: runner-build
runner-build: ## install the runner and compile TypeScript to runner/dist
	$(NPM) --prefix runner ci --prefer-offline --no-audit --no-fund
	$(NPM) --prefix runner run build

.PHONY: runner-ci
runner-ci: ## runner install, typecheck, unit tests and build on Node 22
	$(NPM) --prefix runner ci --prefer-offline --no-audit --no-fund
	$(NPM) --prefix runner run typecheck
	$(NPM) --prefix runner test
	$(NPM) --prefix runner run build

.PHONY: test
test: ## all Go unit and store integration tests on TEST_DATABASE_URL
	go test -race -shuffle=on -coverprofile=cover.out ./...

.PHONY: live
# Start the built runner first, or set RUNNER_URL to one already running.
# Each provider without its existing key environment variable is skipped.
live: ## smoke every configured provider through the runner /probe
	LIVE=1 go test -count=1 -v -run Live ./internal/runner

.PHONY: e2e
e2e: ## real Core + AR PostgreSQL + Node runner against a local mock AI
	scripts/e2e.sh

.PHONY: fmt-check
fmt-check: ## fail if any Go file needs gofmt
	@out=$$(gofmt -l .); [ -z "$$out" ] || { echo "gofmt needed:"; echo "$$out"; exit 1; }

.PHONY: tidy-check
tidy-check: ## fail if go.mod or go.sum is not tidy
	go mod tidy -diff

ACTIONLINT_VERSION ?= v1.7.12

.PHONY: actionlint
actionlint: ## the GitHub Actions workflows
	go run github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION)

.PHONY: lint
lint: fmt-check tidy-check actionlint ## gofmt, modules, workflows, go vet, golangci-lint
	go vet ./...
	golangci-lint run
	@if command -v shellcheck >/dev/null; then shellcheck scripts/*.sh; else echo "shellcheck is not installed: scripts/ not checked"; fi

.PHONY: script-test
script-test: ## tests of scripts/ and deploy/, plus available shell checks
	scripts/release-notes_test.sh
	deploy/aishie-runtime-deploy_test.sh
	@if command -v shellcheck >/dev/null; then shellcheck -s sh deploy/aishie-runtime-deploy deploy/aishie-runtime deploy/setup-server.sh && shellcheck deploy/aishie-runtime-deploy_test.sh; else echo "shellcheck is not installed: deploy/ not checked"; fi

.PHONY: vuln
vuln: ## known vulnerabilities in Go dependencies
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

.PHONY: docker
docker: ## build the single Go + Node 22 image locally; never pushes
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) -t aishie-runtime:dev .

.PHONY: docker-test
docker-test: docker ## build the image and test run, runner health and shutdown
	scripts/image_test.sh aishie-runtime:dev

.PHONY: ci
ci: lint script-test runner-ci test ## everything CI checks, except docker and vuln

.PHONY: clean
clean: ## remove Go build output and runner dist (dependencies are kept)
	rm -rf bin dist cover.out runner/dist

.PHONY: clean-testdb
clean-testdb: ## drop stray test databases
	@psql -X -At -d postgres -c "SELECT datname FROM pg_database WHERE datname ~ '^aishie_.*_t_'" \
		| while read -r d; do echo "drop $$d"; dropdb --if-exists --force "$$d"; done
