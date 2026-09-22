# Compatibility

What a version number here promises, and what it does not.

## Versioning

[Semantic versioning](https://semver.org/). Within a `v2` line, code that
compiles and works against `v2.x` continues to compile and work against every
later `v2.y`.

Breaking that requires a major version, which in Go means a new import path
(`github.com/brunoga/raft/v3`). Both can be imported at once, so a migration
does not have to happen everywhere in one commit.

`v2` is where that rule was first used. `v1` promised that a transport
listening in plaintext and an HTTP API serving without authorization would
keep working, and `v2` withdraws that promise deliberately: those listeners
now refuse to start unless the exposure is asked for by name. See the
migration notes in the [changelog](../CHANGELOG.md). `v1` is still importable
as `github.com/brunoga/raft` and still gets fixes that do not break it.

## What is covered

Everything exported from these packages:

| package | |
|---|---|
| `github.com/brunoga/raft/v2` | the engine |
| `.../easyraft` | the batteries-included layer |
| `.../storage/filestore`, `.../storage/memstore` | storage backends |
| `.../transport/grpctransport`, `.../transport/memtransport` | transports |
| `.../discovery`, `.../discovery/udpbroadcast`, `.../discovery/dnsdiscovery` | peer discovery |
| `.../metrics/prommetrics`, `.../metrics/rpctracer` | observability |

Covered means: exported identifiers are not removed or renamed, function
signatures do not change, struct fields are not removed or retyped, and
behaviour documented on them does not change incompatibly.

Also covered, because a cluster is upgraded one node at a time:

- **The on-disk format.** A `v1.y` node starts on a directory written by any
  `v1.x`, including one holding a snapshot, a partially truncated log, or a
  hard state written by an older layout.
- **The wire format.** A `v1.x` node and a `v1.y` node interoperate in the same
  cluster, in either direction. The field numbers and RPC set are pinned by a
  test; see `transport/grpctransport/proto/raft.proto`.

## What is not covered

- **Anything under `internal/`,** including the generated protobuf bindings in
  `transport/grpctransport/internal/raftpb`. The encoding is how a transport
  happens to carry an RPC, not a contract. Generate your own bindings from
  `proto/raft.proto` if you need to speak the protocol from outside.
- **The examples.** `examples/` is written to be read and changed.
- **Unexported behaviour**: log output, metric label values, the exact wording
  of an error's `Error()` string. Match errors with `errors.Is` and the
  exported sentinels, never by comparing strings.
- **Timing.** Default timeouts, batch sizes and intervals are tuning, and may
  change within a minor version. Anything your deployment depends on should be
  set explicitly in `Config`.
- **The Go version.** The minimum Go release may rise in a minor version,
  following the Go project's own support policy. It is stated in `go.mod` and
  in the README.

## How the API grows

New capability is added without breaking what exists:

- **New optional interfaces.** A `Storage` or `StateMachine` that can do better
  than the minimum says so by implementing an extra interface, which the engine
  detects with a type assertion. An implementation that does not is called
  exactly as before. `BatchWriter`, `CommitRecorder`, `DurableStateMachine`,
  `BatchApplier` and `SnapshotCapturer` were all added this way, and the next
  one will be too.
- **New `Config` fields.** Zero means "as before", so a `Config` built against
  an earlier version behaves identically.
- **New functional options.** `easyraft` takes options and nothing else; its
  configuration struct is unexported precisely so that adding a field is never
  a breaking change.

This is why the interfaces a caller implements are kept small. Adding a method
to `Storage`, `StateMachine` or `Transport` would break every implementation
that exists, so it does not happen within a major version.

## Deprecation

Anything scheduled for removal is marked `Deprecated:` in its doc comment, with
what to use instead, and keeps working for the rest of the `v1` line.

## Security

Report a suspected vulnerability privately through GitHub's security advisory
form on the repository rather than as a public issue.
