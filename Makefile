.PHONY: build build-e2e test tidy-check lint bench clean fmt vet cover check deps tools e2e e2e-smoke e2e-full e2e-soak phase-check dev-index help

BINARY_NAME := atlascache
BUILD_DIR   := bin
GO          := go
E2E_DIR     := test/e2e
# Keep in step with .github/workflows/ci.yml
GOLANGCI_VERSION := 2.12.2
VERSION     := $(shell cat VERSION 2>/dev/null || echo "0.0.0-unknown")
COMMIT      := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE        := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.date=$(DATE)

## help: list available targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'

## build: build the server binary
build:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/atlascache

## build-e2e: build the e2e runner binary
build-e2e:
	@mkdir -p $(BUILD_DIR)
	cd $(E2E_DIR) && $(GO) build -o ../../$(BUILD_DIR)/atlas-e2e ./cmd/atlas-e2e

## test: run unit tests with race detection in both modules
test:
	$(GO) test -race -cover ./...
	cd $(E2E_DIR) && $(GO) test -race -cover ./...

## lint: run golangci-lint in both modules
lint:
	golangci-lint run ./...
	cd $(E2E_DIR) && golangci-lint run ./...

## fmt: format both modules
fmt:
	$(GO) fmt ./...
	cd $(E2E_DIR) && $(GO) fmt ./...

## vet: vet both modules
vet:
	$(GO) vet ./...
	cd $(E2E_DIR) && $(GO) vet ./...

## bench: run benchmarks
bench:
	$(GO) test -bench=. -benchmem -run=^$$ ./...

## cover: generate an HTML coverage report
cover:
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

## e2e: run the full tier (the regression gate)
e2e: e2e-full

## e2e-smoke: run the smoke tier (fast, every commit)
e2e-smoke: build build-e2e
	./$(BUILD_DIR)/atlas-e2e --tier smoke --binary ./$(BUILD_DIR)/$(BINARY_NAME) --specs $(E2E_DIR)/specs

## e2e-full: run the full tier (every PR, phase close)
e2e-full: build build-e2e
	./$(BUILD_DIR)/atlas-e2e --tier full --binary ./$(BUILD_DIR)/$(BINARY_NAME) --specs $(E2E_DIR)/specs

## e2e-soak: run the soak tier (nightly, milestone close)
e2e-soak: build build-e2e
	./$(BUILD_DIR)/atlas-e2e --tier soak --binary ./$(BUILD_DIR)/$(BINARY_NAME) --specs $(E2E_DIR)/specs

## phase-check: verify a phase is ready to close (PHASE=P0)
phase-check:
	@test -n "$(PHASE)" || { echo "usage: make phase-check PHASE=P0"; exit 2; }
	$(GO) run ./tools/phasecheck --phase $(PHASE)

## dev-index: regenerate dev/INDEX.md from front-matter
dev-index:
	$(GO) run ./tools/devindex

## check: fmt, vet, lint, test
check: fmt vet lint test

## tidy-check: fail if go.mod/go.sum in either module are not tidy
tidy-check:
	@$(GO) mod tidy -diff || { echo "root module is not tidy; run 'make deps'"; exit 1; }
	@cd $(E2E_DIR) && $(GO) mod tidy -diff || { echo "test/e2e module is not tidy; run 'make deps'"; exit 1; }
	@echo "both modules are tidy"

## deps: download and tidy dependencies in both modules
deps:
	$(GO) mod download && $(GO) mod tidy
	cd $(E2E_DIR) && $(GO) mod download && $(GO) mod tidy

## tools: install dev tooling and git hooks
# Version is pinned and must match .github/workflows/ci.yml. The /v2/ path is
# required: the v1 module path resolves to a binary that cannot read a
# `version: "2"` config and fails with "the format is required".
tools:
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v$(GOLANGCI_VERSION)
	@git config core.hooksPath .githooks && echo "git hooks installed (core.hooksPath=.githooks)"

## clean: remove build artifacts
clean:
	rm -rf $(BUILD_DIR)
	rm -f coverage.out coverage.html cpu.out mem.out
