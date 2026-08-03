.PHONY: build test lint bench clean fmt vet cover

# Build settings
BINARY_NAME=atlascache
BUILD_DIR=bin
GO=go

# Build the main binary
build:
	@mkdir -p $(BUILD_DIR)
	$(GO) build -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/atlascache

# Run all tests with race detection
test:
	$(GO) test -v -race -cover ./...

# Run linter
lint:
	golangci-lint run ./...

# Run benchmarks
bench:
	$(GO) test -bench=. -benchmem ./...

# Run benchmarks for storage package only
bench-storage:
	$(GO) test -bench=. -benchmem ./internal/storage/...

# Format code
fmt:
	$(GO) fmt ./...

# Run go vet
vet:
	$(GO) vet ./...

# Generate coverage report
cover:
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report generated: coverage.html"

# Run all checks (format, vet, lint, test)
check: fmt vet lint test

# Clean build artifacts
clean:
	rm -rf $(BUILD_DIR)
	rm -f coverage.out coverage.html

# Download dependencies
deps:
	$(GO) mod download
	$(GO) mod tidy

# Install development tools
tools:
	go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest

# Run integration tests
integration:
	$(GO) test -v -tags=integration ./test/integration/...

# Profile CPU usage
profile-cpu:
	$(GO) test -bench=BenchmarkGet -cpuprofile=cpu.out ./internal/storage/
	$(GO) tool pprof cpu.out

# Profile memory usage
profile-mem:
	$(GO) test -bench=BenchmarkGet -memprofile=mem.out ./internal/storage/
	$(GO) tool pprof mem.out
