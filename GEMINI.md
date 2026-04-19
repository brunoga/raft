# Gemini Project Guide: Raft (Go 1.26+)

This document provides foundational mandates and expert guidance for developing within the `raft` project. Adherence ensures architectural consistency, performance, and idiomatic quality.

## Core Mandates

- **Go Version:** Target Go 1.26.2 or later. Utilize modern standard library features (e.g., `slices`, `maps`, `iter` packages) and idiomatic patterns.
- **Tooling First:** Always use the provided `Makefile` for local development.
- **Code Quality:** Zero tolerance for linting errors. All PRs must pass `make lint`.
- **Concurrency Safety:** Since this is a Raft implementation, rigorous concurrency management is required. Use the race detector (`-race`) for all tests.

## Development Workflow

### 1. Research and Navigation
- Use `smart_read` for targeted file inspection and structural outlines.
- Use `list_files` to maintain a clear map of the package hierarchy.
- Read `CONTRIBUTING.md` for the pull request process and environment setup.

### 2. Implementation Standards
- **Surgical Edits:** Use `smart_edit` for all Go file modifications. It ensures `gofmt` and `goimports` are applied automatically and validates syntax before saving.
- **Idiomatic Patterns:**
    - **Error Handling:** Use `errors.Is` and `errors.As` for error inspection. Wrap errors with `%w` for context.
    - **Context:** Pass `context.Context` as the first argument for all network and long-running operations. Respect cancellation.
    - **Generics:** Utilize generics where they provide clear type safety benefits without over-complicating the API.
    - **Performance:** For hot paths (e.g., log replication), use `sync.Pool` for allocations and avoid unnecessary copies.

### 3. Tooling & Automation
- **Linter (`golangci-lint`):** Configured via `.golangci.yml`. Enable all mandatory linters (`errcheck`, `staticcheck`, `revive`, `govet`, etc.). Run via `make lint`.
- **Formatting:** Standard `gofmt` and `goimports` are mandatory. These are handled automatically by `smart_edit`.
- **Build & Verification:** Use `smart_build` to run the full pipeline (Tidy -> Format -> Build -> Test -> Lint).

### 4. Testing Strategy
- **Unit Tests:** Every new feature or bug fix requires a corresponding test.
- **Race Detection:** Always run tests with the race detector: `go test -race ./...` (or `make test`).
- **Mutation Testing:** Use `mutation_test` to evaluate the robustness of your test suite. Aim for high mutation scores in core Raft logic.
- **Validation:** Use `test_query` for deep analysis of coverage and failure patterns across the entire codebase.

## Project Structure
- `/transport/`: Implementation-specific network layers (gRPC, Mem).
- `/storage/`: Persistence layers (File, Mem).
- `/discovery/`: Node discovery mechanisms.
- `/metrics/`: Observability and Prometheus integration.
- `node.go`, `raft_log.go`, `statemachine.go`: Core Raft logic.

## Summary of Commands
- `make all`: Run lints, tests, and build.
- `make lint`: Run `golangci-lint`.
- `make test`: Run tests with `-race`.
- `make build`: Compile the project.
- `make clean`: Remove build artifacts.
