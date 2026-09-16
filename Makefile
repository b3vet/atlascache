.PHONY: build build-ctl build-e2e cross-check test tidy-check fuzz lint bench bench-network clean fmt vet cover cover-sdk check deps tools e2e e2e-smoke e2e-full e2e-soak examples phase-check dev-index dev-certs help

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

## build-ctl: build the atlasctl CLI binary
# The E2E suite finds it beside the server binary, which is why it lands in the
# same directory rather than somewhere of its own.
build-ctl:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/atlasctl ./cmd/atlasctl

## cross-check: verify both binaries build for every released platform
# FEAT-0029 requires the CLI to cross-compile; a release that discovers
# otherwise discovers it at tag time, which is the worst moment for it.
cross-check:
	@for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do \
		for pkg in ./cmd/atlascache ./cmd/atlasctl; do \
			GOOS=$${target%/*} GOARCH=$${target#*/} CGO_ENABLED=0 $(GO) build -trimpath -o /dev/null $$pkg \
				|| { echo "$$pkg does not build for $$target"; exit 1; }; \
		done; \
		echo "  $$target ok"; \
	done

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
e2e-smoke: build build-ctl build-e2e
	./$(BUILD_DIR)/atlas-e2e --tier smoke --binary ./$(BUILD_DIR)/$(BINARY_NAME) --specs $(E2E_DIR)/specs

## e2e-full: run the full tier (every PR, phase close)
e2e-full: build build-ctl build-e2e
	./$(BUILD_DIR)/atlas-e2e --tier full --binary ./$(BUILD_DIR)/$(BINARY_NAME) --specs $(E2E_DIR)/specs

## e2e-soak: run the soak tier (nightly, milestone close)
e2e-soak: build build-ctl build-e2e
	./$(BUILD_DIR)/atlas-e2e --tier soak --binary ./$(BUILD_DIR)/$(BINARY_NAME) --specs $(E2E_DIR)/specs

## examples: run every examples/ program against a live server
# The examples are documentation, and documentation nothing has executed is a
# guess (FEAT-0031). run.sh starts the servers it needs -- plaintext, with auth,
# and TLS -- on ports it picks at run time, runs every program and the atlasctl
# tour, and fails if any of them reports something other than what it documents.
examples: build
	./examples/run.sh --binary $(BUILD_DIR)/$(BINARY_NAME)

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

# ---------------------------------------------------------------------------
# Release engineering (FEAT-0032, FEAT-0033)
# ---------------------------------------------------------------------------
.PHONY: docker-build docker-build-debug docker-buildx release-check release-dry-run changelog-draft

IMAGE       ?= atlascache
IMAGE_TAG   ?= $(VERSION)
# Under 20MB is the target ADR-0024 sets; the distroless static base is ~2MB and
# the stripped static binary is most of the rest.
IMAGE_MAX_BYTES ?= 20000000
PLATFORMS   ?= linux/amd64,linux/arm64

## docker-build: build the container image and check it against the size target
docker-build:
	docker build \
		--target runtime \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg DATE=$(DATE) \
		-t $(IMAGE):$(IMAGE_TAG) .
	@size=$$(docker image inspect $(IMAGE):$(IMAGE_TAG) --format '{{.Size}}'); \
	printf '%s:%s is %d bytes (%.1fMB)\n' "$(IMAGE)" "$(IMAGE_TAG)" "$$size" "$$(echo "$$size/1000000" | bc -l)"; \
	test "$$size" -le "$(IMAGE_MAX_BYTES)" || { \
		echo "image is over the $(IMAGE_MAX_BYTES)-byte target (ADR-0024)"; exit 1; }

## docker-build-debug: build the :debug variant, same binary on a base with a shell
docker-build-debug:
	docker build \
		--target debug \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg DATE=$(DATE) \
		-t $(IMAGE):$(IMAGE_TAG)-debug .

## docker-buildx: build the multi-arch manifest without pushing it anywhere
# A multi-platform build cannot be loaded into the local image store, so this
# writes an OCI archive instead. It exists to prove both architectures build
# before a tag does it for real; the release pipeline pushes the same manifest.
#
# It creates a builder of its own and removes it again. Docker's default builder
# uses the `docker` driver, which can build neither a multi-platform image nor
# an OCI archive -- and leaving a builder behind, or switching the one somebody
# was using, is not this target's business.
BUILDX_BUILDER := atlascache-buildx
docker-buildx:
	@mkdir -p $(BUILD_DIR)
	@docker buildx inspect $(BUILDX_BUILDER) >/dev/null 2>&1 \
		|| docker buildx create --name $(BUILDX_BUILDER) --driver docker-container >/dev/null
	docker buildx build --builder $(BUILDX_BUILDER) \
		--platform $(PLATFORMS) \
		--target runtime \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg DATE=$(DATE) \
		-t $(IMAGE):$(IMAGE_TAG) \
		--output type=oci,dest=$(BUILD_DIR)/$(IMAGE)-$(IMAGE_TAG).oci.tar . \
		; status=$$?; docker buildx rm $(BUILDX_BUILDER) >/dev/null 2>&1; exit $$status
	@echo "wrote $(BUILD_DIR)/$(IMAGE)-$(IMAGE_TAG).oci.tar"

## release-check: fail unless the release tag matches VERSION (TAG=v0.1.0)
# The release pipeline runs this first, before anything is built. Tagging v0.1.0
# while VERSION says 0.1.0-dev produces binaries that report the wrong version
# forever: the artifacts are immutable and the tag cannot be moved once anyone
# has fetched it.
#
# TAG defaults to the tag on HEAD, so `make release-check` on a tagged commit
# needs no argument.
release-check:
	@tag="$(TAG)"; \
	if [ -z "$$tag" ]; then \
		tag=$$(git describe --tags --exact-match --match 'v*' 2>/dev/null); \
	fi; \
	if [ -z "$$tag" ]; then \
		echo "no release tag: pass TAG=vX.Y.Z or run this on a tagged commit"; exit 2; \
	fi; \
	if [ "$$tag" != "v$(VERSION)" ]; then \
		echo "tag $$tag does not match VERSION $(VERSION)"; \
		echo "either bump VERSION to $${tag#v} or tag v$(VERSION); the artifacts are immutable"; \
		exit 1; \
	fi; \
	echo "tag $$tag matches VERSION $(VERSION)"

## release-dry-run: build every release artifact locally, publishing nothing
# The release pipeline is the one piece of automation here with no undo, so this
# is a first-class target rather than a flag somebody has to remember. It
# produces the binaries for all six targets, the archives, checksums, SBOMs,
# signatures and the container images into dist/, and pushes nothing.
#
# The signatures are made with a throwaway key (scripts/cosign-sign.sh): keyless
# signing is CI's OIDC identity and cannot be exercised off CI. Everything else
# is exactly what a real release produces.
release-dry-run:
	@command -v goreleaser >/dev/null 2>&1 || { \
		echo "goreleaser is not installed: brew install goreleaser"; exit 1; }
	goreleaser release --snapshot --clean --skip=publish,announce
	@echo
	@echo "dist/ holds everything a release would publish. Nothing was pushed."

## changelog-draft: draft the unreleased changelog entries with git-cliff
# The draft removes the grunt work; the edit is the point (ADR-0008). This
# prints rather than writing, so the curation step stays deliberate.
changelog-draft:
	@command -v git-cliff >/dev/null 2>&1 || { \
		echo "git-cliff is not installed: brew install git-cliff"; exit 1; }
	git-cliff --tag v$(VERSION) --unreleased
