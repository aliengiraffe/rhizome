# Working on Rhizome

## Orientation

Rhizome is a Go MCP bridge for local Claude Code and Codex sessions. It binds
verified session identities, collects selected activity, discovers peers through
memberlist, and delivers events over authenticated loopback gRPC. Claude uses
channel notifications; Codex uses per-thread nodes and local queue delivery.
There is no broker or background device-wide transcript scanner in the current
implementation.
The README leads with the planned network server/client architecture; do not
treat that plan as implemented behavior or invent deployment commands.

Start with [README.md](README.md) for product direction and current setup,
[docs/architecture.md](docs/architecture.md) for current implementation details,
[docs/codex.md](docs/codex.md) for Codex contracts, and
[docs/verification.md](docs/verification.md) for tested scope and manual checks.

| Path | Responsibility |
| --- | --- |
| `main.go` | CLI dispatch, including MCP and the separate legacy demo. |
| `server.go`, `client.go`, `cluster.go` | Legacy demo networking; not the production MCP channel. |
| `internal/channel/` | MCP, binding/lifecycle, discovery, routing, collection, and delivery. |
| `proto/` | Protocol definitions and generated Go bindings. |
| `scripts/` | Process smoke tests. |
| `Makefile`, `go.mod` | Authoritative build targets and Go module/toolchain requirements. |

## Commands and completion

Run from the repository root with Go 1.25+ and Python 3. Prefer Makefile targets
over their underlying commands.

```sh
make build                         # Build the rhizome executable.
make check                         # go vet ./... and go test -race ./...
python3 scripts/smoke_stdio.py      # Isolated cluster and synthetic transcripts.
git diff --check                   # Check whitespace errors.
make proto                         # Requires protoc; installs pinned generators in .bin.
python3 scripts/smoke_codex.py --help # Inspect live smoke prerequisites/options.
```

Before marking a change done, build, check (including race tests), the stdio
smoke, and the whitespace check must pass. Format touched Go files with `gofmt`;
run targeted package tests first. Regenerate protobufs only for protocol changes
and review the generated diff. Check all documentation links and paths.

For Codex integration changes, follow the opt-in live smoke instructions in
[docs/codex.md](docs/codex.md): they require an existing authenticated daemon,
create disposable threads, and run model turns. The script refuses to run without
live opt-in; help output does not validate integration. Use the manual checklist
for client/hook upgrades.

**LAN coverage caveat:** this checkout has no LAN mode. The requested invocation
`python3 scripts/smoke_stdio.py --lan` currently runs the same loopback test because
the script ignores arguments; it does not validate LAN behavior. When LAN support
lands, implement and verify that option and update this guidance in the same change.

## Go engineering principles

- Follow idiomatic Go: MixedCaps names, focused packages, small APIs, and exported
  doc comments beginning with the identifier. Use `gofmt` and `go vet` via the
  workflow above; do not introduce a new linter without a repository need.
- Return errors with useful context; wrap underlying errors with `%w` when callers
  need to inspect them. Never use panics for ordinary control flow.
- Put `context.Context` first in functions that accept it and propagate cancellation
  and deadlines through calls. Every goroutine needs an owner and a stop path;
  clean up timers, listeners, streams, and subprocesses on shutdown or rebinding.
- Bound channels, queues, buffers, replay, and input reads. Keep lock scopes small
  and never hold locks across network I/O; preserve atomic replay/live registration.
- Prefer table-driven tests. Keep tests race-clean and synchronize with explicit
  signals or conditions with deadlines, not sleeps. Cover cancellation, overflow,
  stale identities, and shutdown when changing concurrent paths.
- Prefer the standard library and justify new modules; keep module files consistent
  after dependency changes. Never hand-edit generated protobuf Go files in `proto/`.
- Keep secrets out of source. Use `crypto/rand` for security-sensitive randomness
  and standard-library cryptography; use constant-time comparisons for secrets.

## Rhizome invariants

- **Stdout is MCP only; diagnostics go to stderr.** Any stray output breaks framing
  (`internal/channel/mcp.go`, stdio smoke).
- **Route only by exact `session_id`, never display name, cwd, or PID.** Multiple
  sessions can share those labels; ambiguous/stale identities must fail closed
  (`internal/channel/network.go`, `internal/channel/discovery.go`).
- **Never manually invoke `observe_session` or invent an ID.** Trusted hooks and
  verified transport identity own binding; synthetic test harnesses are not a user
  workflow (`internal/channel/node.go`, `internal/channel/codex.go`).
- **Never log or advertise the cluster token.** It authenticates trusted peers;
  keep runtime files private (`internal/channel/discovery.go`, README).
- **Preserve loopback-only defaults and behavior without `--lan`.** Current MCP
  endpoints must be `127.0.0.1`; widening discovery changes the trust boundary
  (`internal/channel/discovery.go`). Future LAN support must allow private ranges
  only, reject public addresses, and pin TLS certificate fingerprints to authenticate
  remote endpoints. These are requirements for LAN work, not implemented guarantees.
- **Never silently loosen retention, queue, or deduplication limits.** They bound
  memory and slow-peer impact: publisher retention is five minutes/1,000 events/
  8 MiB, stream queues are 64 events, receiver queues 256, and dedup windows 4,096
  (`internal/channel/node.go`, `internal/channel/network.go`,
  `internal/channel/delivery.go`, `internal/channel/transcript.go`).
- **No transcript content in logs.** Diagnose with counters and metadata to avoid
  leaking session content (`internal/channel/transcript.go`, `internal/channel/codex_collect.go`).
- **Peer-only turns must not trigger feedback loops.** Preserve filtering,
  suppression, rate limits, and circuit breakers; never republish received envelopes
  (`internal/channel/transcript.go`, `internal/channel/codex_collect.go`,
  `internal/channel/delivery.go`).
- **Codex collection stays read-only and local.** Do not start/restart its daemon
  or mutate threads through collection; preserve per-thread isolation and bounded
  polling (`internal/channel/codex_rpc.go`, [docs/codex.md](docs/codex.md)).

## Git and PR conventions

Never commit to `main`; use a feature branch. Keep changes scoped and describe
validation and limitations in the PR. Do not add `Co-Authored-By` or other agent
attribution lines to commits or PR descriptions. Preserve the user's author,
committer, and signing configuration.

## Maintaining this file

**The same change must update AGENTS.md** whenever it introduces a new feature,
functionality, flag/environment variable, tool, protocol/wire change, invariant,
or engineering principle, or invalidates an existing entry. Update README/docs
where relevant as well. Keep knowledge useful to nearly every agent session;
prefer concise pointers to authoritative files over duplicating code or detailed
user documentation. Rewrite or prune existing entries rather than endlessly
appending. Remove stale claims and distinguish planned behavior from implemented
and verified behavior.
