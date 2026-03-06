# Contributing to Hive

Thank you for your interest in contributing to Hive! This document covers
everything you need to get started.

## Getting Started

### Prerequisites

- Go 1.23+
- Linux with KVM for VM features (macOS works for building and testing with mocks)
- Docker (optional, for building rootfs images)

### Development Setup

```bash
# Clone the repo
git clone https://github.com/brmurrell3/hive.git
cd hive

# Build all binaries
make build

# Run the test suite
make test
```

### Running Tests

```bash
# Unit tests only (fast, no external dependencies)
make test-unit

# Integration tests (spins up embedded NATS)
make test-integration

# VM tests (requires Linux with /dev/kvm)
make test-vm

# Use mock mode on macOS or without KVM
HIVE_TEST_FIRECRACKER=mock make test
```

## How to Contribute

### Reporting Bugs

Open an issue on GitHub with:
- A clear description of the bug
- Steps to reproduce
- Expected vs. actual behavior
- Your environment (OS, Go version, hardware)

### Suggesting Features

Open an issue describing:
- The problem you're trying to solve
- Your proposed solution
- Any alternatives you've considered

### Pull Requests

1. Fork the repository
2. Create a feature branch from `main`
3. Make your changes
4. Write tests for new functionality
5. Run the full test suite: `make test`
6. Submit a pull request

#### PR Guidelines

- Keep PRs focused. One feature or fix per PR.
- Write a clear description of what changed and why.
- Include tests. PRs without test coverage for new functionality will be asked to add them.
- Follow the existing code style (see below).

## Code Style

### Go Conventions

- **Error handling:** Return errors, don't panic. Wrap with context: `fmt.Errorf("doing X: %w", err)`.
- **Logging:** Use `log/slog` with structured fields. No `log.Fatal` except in `main()`.
- **Testing:** Table-driven tests. Use `t.Helper()` in test helpers. Use build tags (`unit`, `integration`, `vm`).
- **Naming:** Packages are lowercase single words. IDs are lowercase alphanumeric with hyphens.

### Commit Messages

- Use present tense ("Add feature" not "Added feature")
- Keep the subject line under 72 characters
- Reference issue numbers where applicable

## Project Structure

```
cmd/           Binary entrypoints (hived, hivectl, hive-agent)
internal/      Library packages (not importable externally)
docs/          Specification documents
rootfs/        Firecracker VM rootfs build scripts
sdk/           Firmware SDKs (C, MicroPython)
testdata/      Test fixtures
```

## License

By contributing, you agree that your contributions will be licensed under the Apache License 2.0.
