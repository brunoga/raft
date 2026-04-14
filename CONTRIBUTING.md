# Contributing to Raft

Thank you for your interest in contributing to this project!

## Development Setup

1.  Ensure you have Go 1.26+ installed.
2.  Install `golangci-lint`:
    ```bash
    go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
    ```

## Running Tests and Linting

We use a `Makefile` to simplify local development.

-   **Run tests**: `make test`
-   **Run linter**: `make lint`
-   **Run everything**: `make all`

## Pull Request Process

1.  Create a new branch for your changes.
2.  Write tests for any new features or bug fixes.
3.  Ensure `make all` passes locally.
4.  Open a Pull Request with a clear description of your changes using the provided template.
5.  CI will automatically run tests and linting on your PR. All checks must pass before merging.
