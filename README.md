# AtlasCache

A high-performance, distributed in-memory key-value store written in Go.

## Features

- High-performance sharded storage engine
- Multiple eviction policies (LRU, LFU, FIFO, None)
- Hierarchical time-wheel for efficient TTL management
- Configuration hot-reload support
- Structured logging with zerolog

## Requirements

- Go 1.22 or later
- golangci-lint (for development)

## Quick Start

### Build

```bash
make build
```

### Run Tests

```bash
make test
```

### Run Benchmarks

```bash
make bench
```

### Lint

```bash
make lint
```

## Configuration

Copy the example configuration file:

```bash
cp config.example.yaml config.yaml
```

See `config.example.yaml` for all available options.

## Project Structure

```
atlascache/
├── cmd/
│   └── atlascache/          # Main server binary
├── internal/
│   ├── config/              # Configuration management
│   ├── storage/             # Storage engine
│   ├── ttl/                 # TTL time-wheel
│   ├── eviction/            # Eviction policies
│   └── logging/             # Logging setup
├── test/
│   └── integration/         # Integration tests
└── docs/                    # Documentation
```

## Development

### Prerequisites

Install development tools:

```bash
make tools
```

### Running All Checks

```bash
make check
```

This runs formatting, vet, lint, and tests.

### Coverage Report

```bash
make cover
```

## License

MIT License
