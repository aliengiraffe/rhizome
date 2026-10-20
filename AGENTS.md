# Working on Rhizome

## Orientation

Rhizome is a Go MCP bridge for local coding agent sessions. It binds
verified session identities, collects selected activity, discovers peers through
memberlist, and delivers events over authenticated loopback gRPC. Claude uses
channel notifications; Codex uses per-thread nodes and local queue delivery.
An optional per-user service observes coding agent logs independently of MCP
and maintains the local session-history catalog behind `search_sessions` and
`read_session_history`; see [docs/service.md](docs/service.md) for lifecycle,
boundaries, and validation and [docs/history.md](docs/history.md) for the
history contract.

Start with [README.md](README.md) for behavior and setup,
[docs/codex.md](docs/codex.md) for Codex contracts, and
[docs/verification.md](docs/verification.md) for tested scope and manual checks.

| Path | Responsibility |
| --- | --- |
| `main.go`, `service.go`, `gateway.go`, `files.go` | CLI dispatch, MCP, setup, service lifecycle and legacy demo. |
| `internal/config/` | Strict TOML schema, file/env/flag resolution and source tracking. |
| `internal/service/`, `internal/localstate/` | Native user-service definitions, private runtime state and OS locks. |
| `server.go`, `client.go`, `cluster.go` | Legacy demo networking; not the production MCP channel. |
| `internal/channel/` | MCP, binding/lifecycle, discovery, routing, collection, delivery, session naming (`name.go`), and session history (`history*.go`: catalog types, observer-side indexer, snapshot reader, MCP tools). |
| `proto/` | Protocol definitions and generated Go bindings. |
| `scripts/` | Process smoke tests. |
| `Taskfile.yml`, `go.mod` | Authoritative build targets and Go module/toolchain requirements. |

## Commands and completion

Run from the repository root with Go 1.25+, Python 3, and [Task v3](https://taskfile.dev/docs/installation).
Install Task with `brew install go-task` or
`go install github.com/go-task/task/v3/cmd/task@latest`, with the Go bin directory
on `PATH`. Prefer Taskfile tasks over their underlying commands; `task --list`
lists them. Use `PROTOC=/path/to/protoc task proto` to override the compiler.

```sh
task build                         # Build the rhizome executable.
./rhizome config                   # Inspect effective configuration and sources.
task check                         # go vet ./... and go test -race ./...
python3 scripts/smoke_stdio.py      # Isolated cluster and synthetic transcripts.
python3 scripts/smoke_service.py    # Separate observer/MCP processes, handoffs, history catalog.
python3 scripts/smoke_stdio.py --lan # Same scenario in LAN mode: pinned TLS, separate registries.
git diff --check                   # Check whitespace errors.
task proto                         # Requires protoc; installs pinned generators in .bin.
python3 scripts/smoke_codex.py --help # Inspect live smoke prerequisites/options.
```

Before marking a change done, build, check (including race tests), both stdio
smokes (loopback and `--lan`), the service smoke, and the whitespace check must pass.
Format touched Go files with `gofmt`; run targeted package tests first. Regenerate
protobufs only for protocol changes
and review the generated diff. Check all documentation links and paths.

For Codex integration changes, follow the opt-in live smoke instructions in
[docs/codex.md](docs/codex.md): they require an existing authenticated daemon,
create disposable threads, and run model turns. The script refuses to run without
live opt-in; help output does not validate integration. Use the manual checklist
for client/hook upgrades.

**LAN coverage:** `python3 scripts/smoke_stdio.py --lan` runs the stdio scenario
with `--lan` on an isolated runtime directory per process and verifies pinned TLS.
It exercises one machine only; cross-device behavior is covered by the manual
two-machine check in [docs/verification.md](docs/verification.md).

**Tailscale coverage:** `internal/channel/tailscale_test.go` drives Tailscale mode
on loopback through an injected daemon view (beacon port, peer enumeration,
same-device mode isolation, wrong-token peers). Nothing in `make check` talks to
a real tailnet; the two-device Tailscale checklist in
[docs/verification.md](docs/verification.md) is manual.

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

- **One configuration resolver.** `internal/config` owns settings with explicit
  flag > env > TOML > default precedence and strict unknown-key rejection.
  MCP and service share `runtime_dir` through `~/.rhizome/config.toml`; tokens
  never enter configuration. Keys are grouped per agent (`[claude]`, `[codex]`);
  which agent a process serves is the per-launch `rhizome mcp --agent` flag and
  must never become a configuration key. `RHIZOME_CONFIG`/`--config` select an
  existing file.
  Services pin that absolute path, never snapshot settings; installation requires
  the file and rejects overrides that would be lost when the service starts.
  CLI usage: `rhizome mcp | setup | service | config | token | invite | join | peers | files | demo [flags]`.
  The `[network]` table (`mode` = local/lan/tailscale, plus bind/advertise/join/
  mdns/gossip_port/discovery_interval) is validated at read time and applies to
  `mcp` only, with flag > env > file > default. `rhizome setup` is the only
  writer of the file: it asks or takes `--network`, never overwrites without a
  backup, writes only answered keys at 0600, and never writes a token
  (`setup.go`, `setup_config.go`).

- **Host pairing is service-owned and non-transitive.** `invite` / `join` pair
  persistent host keys over TLS without exporting or replacing local tokens.
  `[gateway]` (`mode`, `listen`, `advertise`) and matching `RHIZOME_GATEWAY_*` /
  service flags are opt-in and separate from MCP networking, which stays local
  when pairing is enabled. The service bridges remote sessions through protocol-2
  proxies (`gateway_host`); only local identities with `gateway_version = 1`
  are exported. Never forward proxy advertisements, trust peer-supplied endpoints,
  or expose local history/admin remotely. Cross-host ID collisions fail closed.
  Revocation closes connections and invalidates queued events via local trust
  grants; old invitations cannot restore revoked trust. State is private under
  `<runtime>/gateway/`. Session privacy (#37) is not yet implemented; the shared
  export/access predicate must enforce it when added. See [docs/gateway.md](docs/gateway.md)
  for enrollment recovery, bounds, configuration, compatibility, and manual checks.
  Run `python3 scripts/smoke_gateway.py` for gateway changes in addition to the
  required existing checks. Gateway protocol code is in `internal/channel/gateway*.go`.

- **File transfers require explicit selection and acceptance.** The service stages
  regular files and serves bounded chunks over pinned host TLS (`/v1/files`),
  never gossip or activity text. MCP binds the source/recipient to its verified
  local session; CLI `files` requires `--session`. Every remote operation checks
  the exact sessions through the gateway's shared access policy. No peer-selected
  paths, symlinks, execution, overwrite, or automatic acceptance. Limits: 16 MiB
  per file, 32 records (512 MiB maximum), two local transfers, 128 KiB chunks,
  59-minute expiry. Transfers are connection-scoped; revocation/disconnection
  fences them. The private inbox is ephemeral: copy verified files out to keep
  them before expiry, disconnection, or service restart. See [docs/files.md](docs/files.md).
  Run the gateway smoke for file changes; tests cover byte integrity and boundaries.

- **Hostnames are display labels.** Shared top-level `hostname` is written by
  the setup prompt or `setup --network ... --hostname ...`. Blank clears an
  override and uses the OS short hostname in every mode, including Tailscale.
  Explicit labels are trimmed, valid UTF-8, control-free, and at most 64 bytes;
  gossip trims whole runes. MCP and observer nodes read it on restart. Never use
  this label for routing, DNS, or identity (`internal/config`, `discovery.go`).
- **Stdout is MCP only; diagnostics go to stderr.** Any stray output breaks framing
  (`internal/channel/mcp.go`, stdio smoke).
- **Route only by exact `session_id`, never display name, session name, cwd, or
  PID.** Multiple sessions can share those labels; ambiguous/stale identities must
  fail closed (`internal/channel/network.go`, `internal/channel/discovery.go`).
- **Session names are untrusted labels with fixed precedence.** Explicit
  `set_session_name` override, then the agent's native title (Claude
  `custom-title`/`ai-title` records, Codex `thread.name`), then the stable fallback
  `<display_name> · <agent_type> · <8-char id>`. Keep `display_name` as the
  directory label, the 96-byte UTF-8 bound, rune-safe trimming in gossip, caller-only
  renaming, and overrides persisted under `<runtime>/names/`. Rhizome must never
  derive a name from prompts or transcript content, and a name-only change must
  not touch generation, streams, subscriptions, cursors, or queues
  (`internal/channel/name.go`, README "Session names").
- **Never manually invoke `observe_session` or invent an ID.** Trusted hooks,
  verified transport identity, and authoritative log metadata own binding;
  synthetic test harnesses are not a user workflow (`internal/channel/node.go`, `internal/channel/codex.go`).
- **Never log or advertise the cluster token.** It authenticates trusted peers;
  keep runtime files private (`internal/channel/discovery.go`, README).
- **Preserve loopback-only defaults and behavior without `--lan`.** Without
  `--lan`/`RHIZOME_LAN=1` every endpoint must be `127.0.0.1`. LAN mode allows
  loopback, private and link-local ranges only, rejects public addresses in both
  modes, and pins TLS certificate fingerprints carried in gossip metadata before
  sending the token (`internal/channel/lan.go`, `internal/channel/discovery.go`).
  Widening the address policy or skipping pinning changes the trust boundary;
  processes in different modes on one device must keep ignoring each other.
- **Tailscale mode is a third mode, not a wider LAN mode.** `--tailscale`/
  `RHIZOME_TAILSCALE=1` implies the LAN transport but its address policy accepts
  only loopback, `100.64.0.0/10` and `fd7a:115c:a1e0::/48`; local and LAN
  policies stay byte-for-byte unchanged and the gossip key is derived per mode.
  Bind/advertise default to the Tailscale IPv4 read from `tailscale status --json`;
  a missing CLI, stopped daemon or missing IP must fail startup, never fall back
  to loopback or `0.0.0.0`. The well-known gossip port (`--gossip-port`, default
  7946, `RHIZOME_GOSSIP_PORT`) is a per-device beacon that forwards to the
  holder's ephemeral listener; peers are enumerated every `--discovery-interval`
  (`RHIZOME_DISCOVERY_INTERVAL`) and joined through the same token-keyed
  `join()` path as mDNS and `--join`. mDNS is off by default in this mode
  (`internal/channel/tailscale.go`, `internal/channel/lan.go`, README).
- **Never silently loosen retention, queue, or deduplication limits.** They bound
  memory and slow-peer impact: publisher retention is five minutes/1,000 events/
  8 MiB, stream queues are 64 events, receiver queues 256, and dedup windows 4,096
  (`internal/channel/node.go`, `internal/channel/network.go`,
  `internal/channel/delivery.go`, `internal/channel/transcript.go`).
- **No transcript content or session names in logs.** Diagnose with counters and
  metadata to avoid leaking session content (`internal/channel/transcript.go`,
  `internal/channel/codex_collect.go`, `internal/channel/name.go`).
- **Peer-only turns must not trigger feedback loops.** Preserve filtering,
  suppression, rate limits, and circuit breakers; never republish received envelopes
  (`internal/channel/transcript.go`, `internal/channel/codex_collect.go`,
  `internal/channel/delivery.go`).
- **Codex collection stays read-only and local.** Do not start/restart its daemon
  or mutate threads through collection; preserve per-thread isolation and bounded
  polling (`internal/channel/codex_rpc.go`, [docs/codex.md](docs/codex.md)).
- **Coordinate collection across processes.** Direct integrations acquire the
  per-session OS lease before baselining. Passive scans yield and expose handoff
  gaps. Never route delivery to `observation_only` identities or replay another
  collector's history on handoff. All local processes must share runtime scope.
- **Native watches drive observer collection.** Do not add normal-path tree
  polling. Keep file/directory/session limits and visible degraded coverage.
  See `internal/channel/observer.go`, `ownership.go`, and `docs/service.md`.
- **History is a read-only, local, metadata-first cache.** The observer is the
  only catalog writer (`<runtime>/history/catalog.json`, atomic rename, version
  1, scope-bound to runtime and roots); MCP processes only read it and serve a
  visibly `stale` catalog when the service is stopped. Indexing and history
  reads must never publish, create nodes, take collection leases, touch live
  queues/cursors, contact an agent daemon, or open files outside the configured
  roots (re-confined on every page, symlinks refused). Reads reuse the live
  normalizers through `activitySink` from byte zero so the content policy stays
  identical; never add a raw-record fallback. Keep the documented bounds
  (4,096 files, 8,192 queued, 4 MiB per tick, 512 tombstones; 8 MiB scan,
  1 MiB response, 2 MiB record, 10 s deadline, 4 concurrent queries, 32
  snapshots for 15 minutes), signed single-use forward-only cursors, and the
  stable error codes in `docs/history.md`. Live status comes only from current
  discovery; never derive `delivery_capable` from a catalog row.

## Git and PR conventions

Never commit to `main`; use a feature branch. Keep changes scoped and describe
validation and limitations in the PR. Do not add `Co-Authored-By` or other agent
attribution lines to commits or PR descriptions. Preserve the user's author,
committer, and signing configuration.

## Documentation language

Describe shared capabilities in terms of agents or coding agents. Use product
names where they identify supported integrations, configuration keys, native
formats, version-specific behavior, or tested scope. Generic language must not
imply support for unimplemented integrations.

## Maintaining this file

**The same change must update AGENTS.md** whenever it introduces a new feature,
functionality, flag/environment variable, tool, protocol/wire change, invariant,
or engineering principle, or invalidates an existing entry. Update README/docs
where relevant as well. Keep knowledge useful to nearly every agent session;
prefer concise pointers to authoritative files over duplicating code or detailed
user documentation. Rewrite or prune existing entries rather than endlessly
appending. Remove stale claims and distinguish planned behavior from implemented
and verified behavior.
