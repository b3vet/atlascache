.PHONY: build build-e2e test tidy-check fuzz lint bench bench-network clean fmt vet cover cover-sdk check deps tools e2e e2e-smoke e2e-full e2e-soak phase-check dev-index dev-certs help

BINARY_NAME := atlascache
BUILD_DIR   := bin
GO          := go
E2E_DIR     := test/e2e
# The Go SDK is its own module (ADR-0015). Three modules now: every target that
# walks them has to walk all three, because a check that silently covers a
# subset is worse than no check -- and there is one more subset to miss.
SDK_DIR     := pkg/client
# Where `make dev-certs` writes. Gitignored: a committed test certificate gets
# copied into production with depressing regularity, and it expires.
CERT_DIR    := certs
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

## test: run unit tests with race detection in all three modules
test:
	$(GO) test -race -cover ./...
	cd $(SDK_DIR) && $(GO) test -race -cover ./...
	cd $(E2E_DIR) && $(GO) test -race -cover ./...

## lint: run golangci-lint in all three modules
lint:
	golangci-lint run ./...
	cd $(SDK_DIR) && golangci-lint run ./...
	cd $(E2E_DIR) && golangci-lint run ./...

## fmt: format all three modules
fmt:
	$(GO) fmt ./...
	cd $(SDK_DIR) && $(GO) fmt ./...
	cd $(E2E_DIR) && $(GO) fmt ./...

## vet: vet all three modules
vet:
	$(GO) vet ./...
	cd $(SDK_DIR) && $(GO) vet ./...
	cd $(E2E_DIR) && $(GO) vet ./...

## fuzz: fuzz the protocol decoder (FUZZTIME=30s by default)
FUZZTIME ?= 30s
fuzz:
	$(GO) test ./internal/protocol/ -run=^$$ -fuzz=FuzzDecode -fuzztime=$(FUZZTIME)

## bench: run benchmarks
bench:
	$(GO) test -bench=. -benchmem -run=^$$ ./...

## bench-network: run the P2 end-to-end network sweep against a real server (FEAT-0026)
# Minutes, not seconds, and the numbers are meaningless on a busy machine, so it
# is deliberately not part of `make bench`. Set NETBENCH_OUT to keep the raw
# table; NETBENCH_ARGS passes anything else through, e.g.
#   make bench-network NETBENCH_ARGS="-netbench.duration=10s"
NETBENCH_OUT  ?=
NETBENCH_ARGS ?=
bench-network: build
	$(GO) test ./test/bench -run TestNetworkBaseline -v -timeout 60m -netbench \
		-netbench.binary $(CURDIR)/$(BUILD_DIR)/$(BINARY_NAME) \
		$(if $(NETBENCH_OUT),-netbench.out $(NETBENCH_OUT),) $(NETBENCH_ARGS)

## cover: generate an HTML coverage report for the root module
cover:
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

## cover-sdk: generate an HTML coverage report for the SDK module
# Separate from `cover` because coverage profiles are per module: one `go tool
# cover` run cannot merge two, and a single report that quietly covered only the
# root module would read as if it covered everything.
cover-sdk:
	cd $(SDK_DIR) && $(GO) test -coverprofile=../../coverage-sdk.out ./...
	$(GO) tool cover -html=coverage-sdk.out -o coverage-sdk.html
	@echo "Coverage report: coverage-sdk.html"

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

## dev-certs: generate a self-signed localhost certificate and key for local TLS
# For development only. The E2E suite does not use these -- it generates its own
# pair per run into a temp directory, so nothing under test depends on a file
# somebody has to remember to create, or on one that quietly expired.
dev-certs:
	@command -v openssl >/dev/null 2>&1 || { echo "openssl is required for dev-certs"; exit 1; }
	@mkdir -p $(CERT_DIR)
	@openssl req -x509 -newkey rsa:2048 -sha256 -days 365 -nodes \
		-keyout $(CERT_DIR)/server.key -out $(CERT_DIR)/server.crt \
		-subj "/CN=localhost/O=AtlasCache Development" \
		-addext "subjectAltName=DNS:localhost,IP:127.0.0.1,IP:::1" >/dev/null 2>&1
	@chmod 600 $(CERT_DIR)/server.key
	@echo "wrote $(CERT_DIR)/server.crt and $(CERT_DIR)/server.key (self-signed, localhost, 365 days)"
	@echo "enable them with:"
	@echo "  tls:"
	@echo "    enabled: true"
	@echo "    cert_file: \"$(CERT_DIR)/server.crt\""
	@echo "    key_file: \"$(CERT_DIR)/server.key\""

## check: fmt, vet, lint, test
check: fmt vet lint test

## tidy-check: fail if go.mod/go.sum in any module are not tidy
# The SDK is also checked for having no dependencies at all. That is the whole
# point of its being a separate module (ADR-0015): a consumer importing it must
# not inherit the server's graph, and the guarantee is worth exactly as much as
# the check that enforces it.
tidy-check:
	@$(GO) mod tidy -diff || { echo "root module is not tidy; run 'make deps'"; exit 1; }
	@cd $(SDK_DIR) && $(GO) mod tidy -diff || { echo "pkg/client module is not tidy; run 'make deps'"; exit 1; }
	@cd $(E2E_DIR) && $(GO) mod tidy -diff || { echo "test/e2e module is not tidy; run 'make deps'"; exit 1; }
# GOWORK=off is required: inside a workspace `go list -m all` reports the
# workspace's build list, which includes the server's dependencies and would
# make this check either always fail or -- worse -- accidentally pass for the
# wrong reason. Off the workspace it reports what a consumer would actually get.
	@cd $(SDK_DIR) && test "$$(GOWORK=off $(GO) list -m all | wc -l | tr -d ' ')" = "1" || { \
		echo "pkg/client has grown a dependency:"; \
		cd $(SDK_DIR) && GOWORK=off $(GO) list -m all; \
		echo "the SDK is stdlib-only by decision (ADR-0015); adding a dependency needs an ADR"; \
		exit 1; }
	@echo "all three modules are tidy, and the SDK still depends on nothing"

## deps: download and tidy dependencies in all three modules
deps:
	$(GO) mod download && $(GO) mod tidy
	cd $(SDK_DIR) && $(GO) mod download && $(GO) mod tidy
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
	rm -f coverage.out coverage.html coverage-sdk.out coverage-sdk.html cpu.out mem.out
