# Working on Rhizome

## Orientation

Rhizome is a Go MCP bridge for local coding agent sessions (Claude Code, Codex,
OpenCode and Cursor). It binds verified session identities, collects selected activity,
discovers peers through memberlist, and delivers events over authenticated
loopback gRPC. Claude uses channel notifications; Codex uses per-thread nodes and
local queue delivery; OpenCode uses an embedded plugin over a private Unix socket
bridge; Cursor uses per-conversation nodes bound by `rhizome hook --agent cursor`
and hook-pull delivery (see [docs/cursor.md](docs/cursor.md)). An optional per-user service observes coding agent logs independently of MCP
and maintains the local session-history catalog behind `search_sessions` and
`read_session_history`; see [docs/service.md](docs/service.md) for lifecycle,
boundaries, and validation and [docs/history.md](docs/history.md) for the
history contract.

Start with [README.md](README.md) for behavior and setup,
[docs/codex.md](docs/codex.md) for Codex contracts,
[docs/opencode.md](docs/opencode.md) for OpenCode, and
[docs/verification.md](docs/verification.md) for tested scope and manual checks.

| Path | Responsibility |
| --- | --- |
| `main.go`, `service.go`, `gateway.go`, `files.go` | CLI dispatch, MCP, service lifecycle and legacy demo. |
| `setup.go`, `setup_config.go`, `setup_agents.go` | `rhizome setup`: agent detection and picker, the configuration wizard, and user-scope skill/MCP/hook installation. |
| `internal/config/` | Strict TOML schema, file/env/flag resolution and source tracking. |
| `internal/service/`, `internal/localstate/` | Native user-service definitions, private runtime state and OS locks. |
| `server.go`, `client.go`, `cluster.go` | Legacy demo networking; not the production MCP channel. |
| `internal/channel/` | MCP, binding/lifecycle, discovery, routing, collection, delivery, Cursor hook binding and hook-pull delivery (`cursor.go`, `cursor_hook.go`), session naming (`name.go`), session history (`history*.go`: catalog types, observer-side indexer, snapshot reader, MCP tools, paired-host policy in `history_peer.go`), and the OpenCode bridge (`opencode*.go`). |
| `plugins/` | Embedded OpenCode relay plugin (relay only, no decisions) and display-only TUI toast plugin. |
| `proto/` | Protocol definitions and generated Go bindings. |
| `scripts/` | Process smoke tests; `scripts/release/` holds the signing, notarization and `.pkg` scripts. |
| `version.go`, `internal/release/`, `.goreleaser.yaml`, `.github/workflows/release.yml` | `--version`, SemVer tag validation, asset matrix/checksums/smoke tool, and the tag-driven release pipeline ([docs/releasing.md](docs/releasing.md)). |
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
task test-plugin                   # OpenCode plugin tests (Node 20+; skips without node).
python3 scripts/smoke_stdio.py      # Isolated cluster and synthetic transcripts.
python3 scripts/smoke_service.py    # Separate observer/MCP processes, handoffs, history catalog.
python3 scripts/smoke_tui.py        # Unix PTYs; isolated setup/service/peers UI and restoration.
python3 scripts/smoke_stdio.py --lan # Same scenario in LAN mode: pinned TLS, separate registries.
python3 scripts/smoke_cursor.py     # Cursor mode: synthetic hook payloads, tickets, hook-pull delivery.
git diff --check                   # Check whitespace errors.
task proto                         # Requires protoc; installs pinned generators in .bin.
python3 scripts/smoke_codex.py --help # Inspect live smoke prerequisites/options.
python3 scripts/smoke_opencode.py --help # Inspect OpenCode live smoke options.
```

Before marking a change done, build, check (including race tests), both stdio
smokes (loopback and `--lan`), the service smoke, the Cursor smoke, and the
whitespace check must pass.
Format touched Go files with `gofmt`; run targeted package tests first. Regenerate
protobufs only for protocol changes
and review the generated diff. Check all documentation links and paths.

For Codex integration changes, follow the opt-in live smoke instructions in
[docs/codex.md](docs/codex.md): they require an existing authenticated daemon,
create disposable threads, and run model turns. The script refuses to run without
live opt-in; help output does not validate integration. Use the manual checklist
for client/hook upgrades. OpenCode integration changes likewise require the live
smoke in [docs/opencode.md](docs/opencode.md) (real model turns; deletes only its
own sessions).

**LAN coverage:** `python3 scripts/smoke_stdio.py --lan` runs the stdio scenario
with `--lan` on an isolated runtime directory per process and verifies pinned TLS.
It exercises one machine only; cross-device behavior is covered by the manual
two-machine check in [docs/verification.md](docs/verification.md).

**Tailscale coverage:** `internal/channel/tailscale_test.go` drives Tailscale mode
on loopback through an injected daemon view (beacon port, peer enumeration,
same-device mode isolation, wrong-token peers). Nothing in `make check` talks to
a real tailnet; the two-device Tailscale checklist in
[docs/verification.md](docs/verification.md) is manual.

## Terminal interfaces

`setup` uses Bubble Tea on usable stdin/stdout terminals when input is needed;
`--plain` retains legacy prompts. Stage all answers until review/apply; cancellation
before apply must not write skills, config, or service state. The opt-in
"install and start" service step (`setup_service.go`) follows the configuration
whether it was created, replaced, or kept; it runs the normal install lifecycle
through an injectable `serviceInstaller` (which replaces an outdated definition
by stopping, rewriting and starting the service, and refuses only a runtime
directory change while the observer runs), reports running only after
`service.Inspect` confirms the observer lock, distinguishes an installed
definition from a running observer on failure, and is never run by `--list` or
by `--network` without the interactive questions. Use `config.Preview`
for resolver-backed validation without disk writes. Filter network fields by the
selected mode. Bare terminal `service` manages lifecycle (confirm stop/uninstall),
`service status --tui` is read-only, and `peers list --tui` browses paired hosts
with exact host IDs. Plain peers stays JSON; explicit service commands, help/list,
redirected streams, `TERM=dumb`, and `service run` never auto-start a UI.
Keep models (`tui*.go`) separate from operations; bound/cancel requests, join
workers on exit, suppress native subprocess output inside screens, sanitize
untrusted display text, and report partial apply without implying rollback.
See README, [service](docs/service.md#terminal-interface), and
[gateway](docs/gateway.md#paired-host-terminal-view) for keys and terminal checks.

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
  CLI usage: `rhizome --version | mcp | hook | setup | service | config | token | invite | join | peers | files | demo [flags]`.
  The `[network]` table (`mode` = local/lan/tailscale, plus bind/advertise/join/
  mdns/gossip_port/discovery_interval) is validated at read time and applies to
  `mcp` only, with flag > env > file > default. `rhizome setup` is the only
  writer of the file: it asks or takes `--network` / `--gateway-mode` (plus
  `--gateway-listen`, `--gateway-advertise`, `--runtime-dir`), merges only explicit
  edits into validated stored TOML, and preserves all other supported keys without
  snapshotting environment overrides. Prompts default to stored choices; keeping
  a file performs no rewrite. Updates require a private backup and atomic 0600
  replacement, and never write a token or alter pairing state; the Bubble Tea
  screen merges into the same stored document. Gateway pairing
  requires local MCP networking; switching transports is explicit
  (`setup.go`, `setup_config.go`).

- **Host pairing is service-owned and non-transitive.** `invite` / `join` pair
  persistent host keys over TLS without exporting or replacing local tokens.
  `[gateway]` (`mode`, `listen`, `advertise`) and matching `RHIZOME_GATEWAY_*` /
  service flags are opt-in and separate from MCP networking, which stays local
  when pairing is enabled. The service bridges remote sessions through protocol-2
  proxies (`gateway_host`); only local identities with `gateway_version = 1`
  are exported, and `opencode` sessions only over links that negotiated the
  `rhizome.gateway.opencode.v1` subprotocol (older peers drop links over unknown agent
  types). Never forward proxy advertisements, trust peer-supplied endpoints,
  or expose the admin API or history except through the opt-in peer-history policy below. Cross-host ID collisions fail closed.
  Revocation closes connections and invalidates queued events via local trust
  grants; old invitations cannot restore revoked trust. Every link end logs a
  stable reason and host ID prefix (never session data); a link that ends within
  5 s counts as a dial failure for the per-host backoff, whichever side dialed,
  and a longer one resets it. State is private under
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

- **Setup installs agents at user scope through Rhizome-owned merges.**
  `rhizome setup --agents` (or the picker) installs, per supported agent, the
  skill, the MCP registration and the binding hooks: Claude in
  `~/.claude.json` (`<CLAUDE_CONFIG_DIR>/.claude.json` for a non-default
  directory) plus `<config dir>/settings.json`, Codex in `<CODEX_HOME>/config.toml`
  plus `hooks.json`, Cursor in `<cursor dir>/mcp.json` plus `hooks.json`
  (`version: 1`; command entries `<exe> hook --agent cursor`, shell-quoted,
  identified on merge by that command). The entry is the absolute executable with `mcp --agent
  <name>`, `--config` only for an explicit existing file, `--codex-bin` when
  resolvable, and the agent home in `env` only when non-default; never a token.
  Hook templates in `setup_agents.go` must equal `.claude/settings.json`,
  `examples/codex/hooks.json` and `examples/cursor/hooks.json` (tested). Merges touch only Rhizome entries,
  keep everything else, back up changed files as `<file>.bak-<timestamp>`,
  follow symlinks, reject malformed files without replacing them and skip
  unchanged files. OpenCode gets the skill and its two plugins only (no hooks;
  its MCP entry stays manual, see `docs/opencode.md`). Configuration is written before installs so wizard
  directories apply. Project files, `--list` and `--network`-only runs never
  install anything; remaining restart/trust/channel steps are printed.
- **Releases are tag-driven, signed and fail closed.** Only SemVer 2.0 tags
  (optional `v`), validated by `internal/release` before any job reaches
  secrets, publish. A prerelease component marks a prerelease; build metadata
  alone does not. `rhizome --version` must stay side-effect free (no config,
  writes, network or daemons) and report the exact version and commit
  injected via ldflags. Production (`RHIZOME_SIGNING=required`) must never
  degrade to ad-hoc or unsigned macOS artifacts. It requires a Developer ID
  signature, execution of the signed binary, an explicit notarization
  `Accepted`, and a stapled Developer ID Installer package whose only payload
  is the same binary at `/usr/local/bin/rhizome` (receipt
  `com.aliengiraffe.rhizome`). The package has no scripts and never touches
  user config, state or the observer. Secrets stay in the `release`
  environment; only the publish job writes releases. It stages a draft,
  verifies the checksum manifest of every asset, and never replaces a
  published release. Linux and Windows assets are unsigned. PRs and manual
  runs build ad-hoc snapshots without secrets. Run `goreleaser check`,
  `actionlint` and `shellcheck -x -P SCRIPTDIR scripts/release/*.sh` for
  pipeline changes. Real signing is validated only by a tagged prerelease
  (docs/releasing.md).
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
- **Cursor binds through hooks, never through `tools/call`.** Cursor sends no
  conversation identity in MCP requests and cannot be woken when idle. `rhizome
  mcp --agent cursor` hosts one node per `conversation_id`, registers a private
  0600 socket under `<runtime>/cursor/` with its PID, working directory and
  `roots/list`, and
  `rhizome hook --agent cursor` (run by `~/.cursor/hooks.json`) forwards a
  minimized payload: never the prompt, tool bodies, transcripts or `user_email`.
  The hook ranks processes against `workspace_roots` (exact, then most
  specific parent, then child; home and `/` never claim nested workspaces),
  contacts exactly one best candidate, and otherwise answers Cursor's default (`{}`, `continue`, `ask`)
  with exit 0; it must never block Cursor. Tool calls resolve by a single-use
  `beforeMCPExecution` ticket (tool name + canonical input digest, 5 s) or the
  single bound conversation; anything else is `identity_ambiguous`. Collection
  is hook push only (`afterAgentResponse`, `postToolUse*`, `stop`), with the
  shared `isPeerInput` suppression; no transcript tailing and no history
  indexing of Cursor logs. Delivery is hook pull through a bounded per-node
  outbox (32 entries, 64 KiB, 8 KiB per injection): `postToolUse*`
  `additional_context` always, `stop` `followup_message` unless
  `[cursor] followup_delivery = false` (file-only, default true; one per
  completed turn with `loop_count` 0, marked peer-only). `rhizome setup --agents cursor` installs the skill, the
  MCP entry and the hooks (see the setup invariant); `examples/cursor/` holds
  the same definitions for manual or project-scope merges. Live Cursor behavior is unverified:
  keep `docs/cursor.md` and the checklist in `docs/verification.md` honest.
- **Never manually invoke `observe_session` or invent an ID.** Trusted hooks,
  verified transport identity, and authoritative log metadata own binding;
  synthetic test harnesses are not a user workflow (`internal/channel/node.go`, `internal/channel/codex.go`, `internal/channel/cursor.go`).
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
  `internal/channel/codex_collect.go`, `internal/channel/opencode_collect.go`,
  `internal/channel/opencode.go`, `internal/channel/name.go`,
  `plugins/opencode/rhizome.js`, `plugins/opencode/rhizome-tui.js`).
- **Peer-only turns must not trigger feedback loops.** Preserve filtering,
  suppression, rate limits, and circuit breakers; never republish received
  envelopes (`internal/channel/transcript.go`, `internal/channel/codex_collect.go`,
  `internal/channel/opencode_collect.go`, `internal/channel/delivery.go`,
  `plugins/opencode/rhizome.js`). OpenCode inputs count when delivered into the
  agent loop (`session.inbox.delivered`), not when queued.
- **Codex and OpenCode collection stay read-only and local.** Do not start/restart
  the Codex daemon or mutate threads/sessions through collection; preserve
  per-thread isolation and bounded polling (`internal/channel/codex_rpc.go`,
  [docs/codex.md](docs/codex.md)). OpenCode's only write is `synthetic` delivery.
- **OpenCode identity comes only from `_meta["ai.opencode/sessionID"]`, and binding
  only from the plugin bridge.** Never add `observe_session` to OpenCode mode. The
  plugin relays allowlisted events of its own directory's sessions and executes
  deliveries; all filtering stays in Go. OpenCode mode shares configuration,
  `[network]` and the other 12 tools. Each bound session holds the collection
  lease; on an instance reload the predecessor releases leases once a successor
  owns the socket (never unlinking it), and the successor retries busy binds for
  at most 60 s; the plugin re-binds its sessions after every reconnect. Bindings
  persist in `<runtime>/opencode-bindings/` (0600, 64 entries, 24 h idle, only
  human-prompt binds, re-verified by the plugin). The plugin finds the socket via
  `RHIZOME_RUNTIME_DIR` or the default only. Bridge limits: 64 KiB frames,
  256-frame plugin buffer, 256 pending inputs, 512-frame manager queue, 16 KiB
  text, 5 s acknowledgement, 64 sessions, 24 h idle
  (`internal/channel/opencode*.go`, `plugins/opencode/rhizome.js`,
  [docs/opencode.md](docs/opencode.md)). Peer deliveries stay synthetic, never
  prompts. The TUI plugin (`plugins/opencode/rhizome-tui.js`, installed by setup
  as `plugins/rhizome-tui/tui.js`) is display-only: it toasts a sanitized,
  200-character excerpt of enqueued Rhizome-envelope synthetic items for its own
  directory's sessions and never decides, sends, opens sockets or files, or logs.
- **Coordinate collection across processes.** Direct integrations acquire the
  per-session OS lease before baselining. Passive scans yield and expose handoff
  gaps. Never route delivery to `observation_only` identities or replay another
  collector's history on handoff. All local processes must share runtime scope.
  Each node holds `<runtime>/<node_id>.lock` from before writing its (atomic,
  routing-only) `<node_id>.json` until close; bootstrap prunes a registration
  only when that lock is free, never because a probe failed or timed out, and
  live nodes restore a missing registration every 30 s. `registry.lock` covers
  only file-system decisions; probes run after it is released, at most 8 at
  once, and a failed live peer is not re-probed by the process for 30 s
  (`internal/channel/registry.go`).
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
- **Peer history is opt-in, serving-host enforced and direct.** File-only keys
  `[gateway.history]` (`enabled` default false, `lookback` default `30d`) and
  `[claude|codex.peer_history]` overrides resolve to `config.HistoryPolicy`.
  The global switch is a master gate, and agent keys refer to the source
  history's agent; OpenCode history is not cataloged, so `[opencode]` tables
  are rejected (OpenCode callers may still pass `host_id`). The service reads the policy at start; a restart applies a
  change and expires remote cursors. Tools take an optional exact `host_id`
  (lowercase fingerprint). Omitting it stays local, with no fan-out or second
  hop. The gateway's `/v1/history` serves only trusted, connected peers and checks
  policy before enumeration and on every page: inclusive UTC cutoffs on
  `last_activity_at` and on each event timestamp, with unknown timestamps failing
  closed unless `all`. Hidden sessions look absent. Cursors bind host, grant and
  policy fingerprint. Rows are stamped with the authenticated `host_id` and
  `live_status: not_reported`. Bounds: 4 concurrent, 2 per peer, 8 snapshots
  per peer (`internal/channel/history_peer.go`, `internal/config/history.go`,
  `docs/history.md`).
- **Device origin is evidence-based; labels never decide it.** The bundled
  `skills/rhizome-collab/SKILL.md` (embedded and installed by `rhizome setup`)
  classifies sessions as this session, this device (own identity has no
  `cert_fingerprint`, i.e. local mode, and the peer has no `gateway_host`),
  paired remote host (`gateway_host`, equal to the `rhizome peers` `host_id`),
  or unknown origin (direct LAN/Tailscale peers, missing metadata). `host`,
  project, endpoints, node IDs and per-process `cert_fingerprint` are labels;
  remote presence never implies a different person. History without `host_id`
  is this device only; remote history keeps one exact `host_id` across search,
  read and pages and reports policy errors instead of substituting local data.
  Status review is read-only. Keep the skill, README and
  [docs/collab-skill-evals.md](docs/collab-skill-evals.md) consistent.

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
