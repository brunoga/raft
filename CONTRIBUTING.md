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

## Fault-injection and safety tests

Alongside the ordinary integration tests, the root package runs the cluster on
a simulated network that loses, duplicates, delays and reorders individual
messages, on a storage wrapper that can fail writes and be crashed, and under a
checker that watches the five Raft safety properties continuously. See
`internal/simnet` for the simulator and `siminvariant_test.go` for the checker.

These runs are randomized, and every one of them prints the seed it used plus
the ordered list of faults it injected. To replay a failure:

```bash
RAFT_SIM_SEED=<seed from the failure> go test -run <TestName> -count=1 .
```

Other knobs:

| Variable | Effect |
| --- | --- |
| `RAFT_SIM_SEED` | Seed for the simulated network. Defaults to a fresh random value. |
| `RAFT_SIM_ITERS` | Randomized iterations per chaos profile. Defaults to 1 so that `go test ./...` stays under a minute. |
| `RAFT_SIM_LONG=1` | Soak mode: many more iterations, each running much longer. |

Every invariant violation fails the test. If a run turns up a defect that
cannot be fixed immediately, add a test that constructs it deliberately rather
than relaxing the checker — a checker that tolerates one shape of violation
tolerates every other bug with the same shape.

## Pull Request Process

1.  Create a new branch for your changes.
2.  Write tests for any new features or bug fixes.
3.  Ensure `make all` passes locally.
4.  Open a Pull Request with a clear description of your changes using the provided template.
5.  CI will automatically run tests and linting on your PR. All checks must pass before merging.
