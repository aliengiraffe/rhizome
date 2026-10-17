# Rhizome

One Go MCP process per Claude Code session. Rhizome binds through that session's
hooks, watches its transcript, discovers local peers with memberlist, and streams
selected peer activity over authenticated loopback gRPC into its own MCP channel.
Two sessions in the same directory remain separate. An optional background
observer discovers local headless sessions from their logs. Install it with
`rhizome service install`; see [service setup and platform behavior](docs/service.md).
An opt-in [LAN mode](#lan-mode) peers sessions across devices on the same network.

## Configuration

MCP, setup, and every service action read `~/.rhizome/config.toml` once at
startup. A missing default file keeps the built-in defaults. Select another file
with `RHIZOME_CONFIG` or the subcommand's `--config /absolute/path.toml` flag
(`service run --config ...` for service definitions). An explicitly selected
file must exist. Reading configuration never creates directories or files.

Precedence is **explicit flag > nonempty environment variable > file > default**.
Run `rhizome config` to print every effective key, its source, and the selected
file path (including “not found”). It also accepts the override flags in the table
for diagnostics. All startup diagnostics and errors go to stderr.

| TOML key | Environment | Flag | Default |
| --- | --- | --- | --- |
| `runtime_dir` | `RHIZOME_RUNTIME_DIR` | service `--runtime-dir` | `~/.rhizome/run` |
| `claude.config_dir` | `CLAUDE_CONFIG_DIR` | — | `~/.claude` |
| `claude.projects_dir` | — | service `--claude-root` | effective `claude.config_dir` + `/projects` |
| `codex.bin` | — | mcp `--codex-bin` | `codex` |
| `codex.home` | `CODEX_HOME` | — | `~/.codex` |
| `codex.sessions_dir` | — | service `--codex-root` | effective `codex.home` + `/sessions` |

```toml
runtime_dir = "~/.rhizome/run"

[claude]
config_dir = "~/.claude"
# Omit, or leave empty, to derive from config_dir.
projects_dir = ""

[codex]
bin = "codex"
home = "~/.codex"
# Omit, or leave empty, to derive from home.
sessions_dir = ""
```

Settings are grouped by the agent they describe. Which agent a process serves is
not configuration: each MCP client launches `rhizome mcp --agent claude` or
`--agent codex` (Claude is the default), so one file serves both agents on a
device. Path settings expand `~`, `$HOME`, and `${HOME}` and must resolve to
absolute paths. `codex.bin` may be an executable name on PATH. Empty agent
directories also use their defaults. Unknown keys/tables, wrong types, invalid
TOML, relative paths, and identical log roots fail before startup.
The file is limited to 64 KiB and must be a regular, user-owned file, not a
symlink or group/world-writable file; 0644 is accepted on Unix. Windows checks
ownership and rejects write grants to other accounts except SYSTEM and
Administrators. Create `~/.rhizome` with 0700 when creating the file manually.

The cluster token is **never configuration**: it remains at `<runtime_dir>/token`
with 0600 permissions. MCP and the observer should share `runtime_dir` through
this file; per-process overrides can intentionally isolate them. Installed
services pin the absolute TOML path and reread it on restart; see
[service configuration](docs/service.md) for installation requirements.

## Codex

Local Codex sessions use `rhizome mcp --agent codex`, with the same seven tools and
cross-agent subscriptions. Incoming events use `codex queue`; outgoing activity
comes from read-only App Server polling. Each caller thread has an independent
node even when Codex shares an MCP connection. See [Codex setup and tested
limitations](docs/codex.md) and the samples in `examples/codex/`.

## Claude setup

Requires Go 1.25+, [Task v3](https://taskfile.dev/docs/installation), and macOS or Linux.
Claude Code **2.1.302** is the tested client; older versions are not supported by this verification.

Install Task with `brew install go-task` or
`go install github.com/go-task/task/v3/cmd/task@latest` (ensure your Go bin directory
is on `PATH`). List available tasks with `task --list`.

```sh
task build
```

This repository ships both `.mcp.json` and `.claude/settings.json`. From this
directory, start Claude in two terminals:

```sh
claude --dangerously-load-development-channels server:rhizome
```

Accept the local development channel confirmation and project MCP consent if
shown. This is the current research-preview channel enablement path; organization
channel policy still applies. The development flag requires an **interactive**
terminal; `claude -p` does not enable it. See the
[Claude channel reference](https://code.claude.com/docs/en/channels-reference).

For another project, merge the `rhizome` server entry and hook entries into that
project's configuration. Set `command` to the **absolute path** of your built
Rhizome binary. Preserve existing MCP servers and hooks. Do not change the server
key unless you also update the hook server name, tool exclusions and channel flag.

The process starts **UNBOUND**. Initial `SessionStart` is skipped by Claude because
its MCP clients are not yet available; Claude 2.1.302 displays a non-blocking hook
notice. Submit a prompt to bind. Later `SessionStart`, `UserPromptSubmit`,
`PreToolUse` and `Stop` hooks refresh the binding. The negative-lookahead matcher
excludes `mcp__rhizome__*`; hooks do not ingest activity themselves. See
[MCP tool hook timing](https://code.claude.com/docs/en/hooks#mcp-tool-hook-fields).
Never ask Claude to call `observe_session` manually or invent an ID.

The official Go MCP SDK handles initialization and tools. A transport adapter uses
the SDK's concurrency-safe writer for `notifications/claude/channel`. It negotiates
MCP **2025-11-25**: testing found that Claude 2.1.302 disables unsolicited channel
notifications when the SDK negotiates its newer 2026 protocol family.
Stdout is exclusively MCP; application diagnostics go to stderr.

## Tools

| Tool | Behavior |
| --- | --- |
| `list_sessions()` | Bound peer identities, project, `session_name` and host labels, conflicts, local binding, subscription/reconnect/gap state, queue depth and counters. |
| `set_session_name(name)` | Name the caller's bound session for peers. Empty clears the override. Returns the effective name and its source. |
| `subscribe(session_id)` | Follow an exact ID. Idempotent; retains intent while absent. Explicitly re-arms that origin if delivery is paused. |
| `unsubscribe(session_id)` | Cancel follow and remove pending activity from that subscription. Direct messages are independent. |
| `publish(text, reply_to?)` | Announce to current subscribers. |
| `send(session_id, text, reply_to?)` | Direct message without a subscription. Returns a stable message ID and receiver queue acceptance. |
| `observe_session(...)` | Internal hook binding/refresh and transcript wake-up. |

Use `list_sessions` in each session, then ask one to subscribe to the other's exact
`session_id`. Display names and session names are labels, never addresses. Direct
integrations take precedence over passive observers of the same session. Multiple
direct nodes with one session ID remain ambiguous and cannot be routed. List results
are gossip snapshots; routing verifies identities over authenticated gRPC.

## Session names

Every bound identity advertises an optional `session_name` so sessions sharing a
directory can be told apart without comparing IDs. `display_name` stays the
project-directory label. The effective name is resolved in this order:

1. An explicit override set with `set_session_name(name)` by that session.
2. The agent's native title: Claude's `/rename` title (`custom-title` transcript
   record), then Claude's automatic title (`ai-title`); for Codex the thread name
   reported by the App Server.
3. A stable fallback `<display_name> · <agent_type> · <first 8 characters of
   session_id>`, identical across process restarts for the same session and project.

`set_session_name` operates only on the caller's verified bound session; unbound
calls fail. Input is trimmed, control characters and line breaks collapse to single
spaces, and the result must fit **96 UTF-8 bytes** or the call is rejected. Native
titles and fallbacks are cut on a rune boundary to the same limit. Empty or
whitespace-only input clears the override and reverts to the native title or
fallback. The response reports the effective name and its `source`.

Explicit overrides persist in `<runtime>/names/` (private, keyed by a hash of the
exact session ID) and are reloaded when the same session binds again, including
after a process restart. Another session in the same directory never inherits
them. A native title that appears or changes later refreshes the name unless an
override exists; a removed native title falls back. A name-only change refreshes
the gossip advertisement without touching `session_id`, `node_id`, generation,
streams, subscriptions, replay cursors or queues.

Names are optional, need not be unique and are never routing keys. Rhizome never
derives a name from prompts or transcript text; native titles come from the
agent's own metadata, and explicit names are set intentionally by the agent or
user. Names are shared with every trusted peer through discovery (gossip
metadata and the authenticated `Info` RPC), so treat them as team-visible labels
and treat peer names as untrusted text. Older peers without the field remain
usable and simply show no name. When gossip metadata would exceed its budget,
`session_name` and `project` are shortened before routing fields.

An accepted direct message is **queued**, not necessarily written, read or acted
upon. A lost RPC response can leave acceptance uncertain; calling `send` again
creates a new message. The wire receiver deduplicates retries of a stable message
ID within its bounded in-memory deduplication window. No exactly-once or semantic
acknowledgement is claimed.

## Identity and lifecycle

Each process has a random node ID. The exact hook-supplied Claude session ID plus a
local generation bind that process to its initialized stdio connection. Neither
cwd, transcript recency, parent PID nor the MCP SDK session ID participates in
routing. Unbound nodes bootstrap gossip but are not subscribable sessions.

Same-session hooks refresh metadata without resetting collection. A new top-level
session ID on a lifecycle/prompt hook retires the old binding, cancels outgoing
subscriptions, disconnects incoming streams, discards queued deliveries and
captures a fresh transcript baseline. Child-agent hooks do not bind. Unexpected
session changes on tool/stop hooks and delayed hooks for retired sessions are
rejected. Resuming a retired session requires a fresh MCP process. A changed
transcript path for the same session is rejected rather than silently following
an unrelated file. `SessionEnd` is advisory; stdio EOF and signals stop listeners,
collectors and streams and remove the registration.

## Discovery and authentication

Listeners allocate their actual ports automatically. `~/.rhizome/run`
contains a private cluster token, a registry lock and one endpoint registration
per live process. Set `runtime_dir` in the [configuration file](#configuration),
or override it with `RHIZOME_RUNTIME_DIR` for an isolated cluster; its
directory must be owned by you and private (0700). Token and registration files
are 0600. Concurrent launches serialize bootstrap with `flock`, probe candidates
with authentication before joining, and prune stale registrations. Registry files
bootstrap gossip; they are not a broker or persisted session state.

The token authenticates gRPC and supplies the memberlist encryption key; it is
never advertised or logged. By default all endpoints must be `127.0.0.1` and
gRPC is plaintext on loopback. Processes with the same user's token are trusted
cluster participants; this is not isolation from a malicious process running as
your user. Permission relay, internet peering, NAT traversal and relays are out
of scope; same-network devices are covered by LAN mode below.

## LAN mode

LAN mode peers Rhizome processes on different devices on the same network. It is
opt-in and still peer-to-peer: no broker, no rendezvous service. Without it,
behavior is exactly the loopback-only cluster above.

```sh
# Device A: create or print this machine's cluster token.
./rhizome token

# Every device: adopt the same token, enable LAN mode.
RHIZOME_TOKEN=<token> ./rhizome mcp --lan            # flag
RHIZOME_TOKEN=<token> RHIZOME_LAN=1 ./rhizome mcp    # or env, for .mcp.json
```

`RHIZOME_TOKEN` (or `--token-file`/`RHIZOME_TOKEN_FILE`) seeds a machine whose
runtime directory has no token yet; it must agree with an existing one. After the
first run the token file is enough. Rotate by deleting the token file on every
device.

| Flag / env | Default | Meaning |
| --- | --- | --- |
| `--lan` / `RHIZOME_LAN=1` | off | Bind and advertise on the network, TLS on gRPC. |
| `--bind-addr` / `RHIZOME_BIND_ADDR` | `0.0.0.0` | Listen address for gRPC and gossip. |
| `--advertise-addr` / `RHIZOME_ADVERTISE_ADDR` | first private interface | Address peers dial. Auto-detection failure is a startup error, never a silent loopback fallback. |
| `--join host:port` / `RHIZOME_JOIN=a,b` | none | Gossip endpoint of a node on another device (see `gossip_endpoint` in `list_sessions`). Repeatable. Unreachable seeds are logged, not fatal. |
| `--mdns` / `RHIZOME_MDNS=0` | on | Advertise and browse `_rhizome._tcp` so devices find each other without `--join`. |

Discovery: the per-machine registry still bootstraps same-device processes. Across
devices, one gossip join from any node on A to any node on B is enough; memberlist
propagates the rest of the membership. mDNS browses every 15 seconds and only joins
instances that advertise the same cluster tag (a one-way hash of the token). Both
mDNS and `--join` seeds are authenticated by memberlist itself: the push/pull is
encrypted with the token-derived key, so a seed with another token fails to join
and never becomes a peer. gRPC identity is verified by the authenticated `Info`
probe before any routing, exactly as on loopback.

Transport: in LAN mode each process generates an ephemeral self-signed certificate
and advertises its SPKI SHA-256 fingerprint in gossip metadata, which the gossip key
already authenticates. Dialers pin that fingerprint during the TLS 1.3 handshake, so
the bearer token is only ever sent to the node gossip named, and a peer without a
fingerprint is never dialed. No CA and nothing beyond the token to distribute.

Address policy: local mode accepts only `127.0.0.1`. LAN mode adds RFC 1918, IPv6
ULA and link-local ranges. Public addresses are rejected in both modes, for
advertised endpoints, gossip members and seeds. This is not a substitute for a host
firewall; `0.0.0.0` exposes the authenticated gRPC and encrypted gossip ports on
every interface. Processes in different modes on one device ignore each other and
leave each other's registrations alone; use one mode per device.

`list_sessions` shows a `host` label per session. Like `display_name` and
`project`, it is a label, never an address; route only by exact `session_id`.

**Trust boundary.** On loopback the boundary is "this user on this machine". In LAN
mode it is "anyone holding the token, on any device on the network". A token holder
can subscribe to transcript activity from every session on every device and send
messages to any of them. There is no per-device or per-user authorization inside a
cluster. Use a per-team token and treat it like a password.

macOS prompts to allow incoming connections for an unsigned binary bound to
`0.0.0.0`; allow it or LAN peers cannot reach you. On a Tailscale or WireGuard
overlay, bind and advertise the overlay address (its ranges are private); TLS and
the token still apply, so the design does not depend on the overlay.

## Activity, replay and bounded delivery

In direct Claude mode, only the hook-supplied top-level transcript is opened.
The optional [background observer](docs/service.md) discovers logs separately. First binding starts at its
current EOF, including discarding a partial historical line. Directory filesystem
notifications wake incremental JSONL reads, with one-second polling as fallback.
Incomplete lines are retained; oversized records are skipped with diagnostics.
Replacement, truncation and changed bytes at the read boundary produce a gap.
Record IDs and tool-use IDs deduplicate/correlate within bounded 4,096-entry
windows. Unknown formats increment parser-skip counters without logging content.

Activity includes assistant text, tool start/completion/failure names, and turn
completion. Thinking, user prompts, channel injections, bookkeeping, full tool
result bodies, child transcripts and Rhizome tool traffic are excluded. Summaries
are deterministic; no additional model is called. Automatic output from peer-only
turns is suppressed until a human prompt resumes work; intentional responses can still use `publish` or `send`. Turn completion
remains in publisher history but does not wake subscribers.

The publisher retains at most **five minutes, 1,000 events and 8 MiB**, evicting
at the first limit. New subscriptions begin at the current position. Replay
snapshot and live registration happen under the same lock. Reconnects use
exponential backoff with jitter and resume after the last locally accepted
sequence. Expired cursors and new publisher streams produce visible gap notices.
Each publisher stream has a 64-event queue; overflow disconnects it for replay
rather than blocking collection or other subscribers.

The receiver has a 256-event queue and a 4,096-entry deduplication window. Automatic
activity batches every two seconds per origin, with content capped at 8 KiB;
individual summaries are capped near 7 KiB with an explicit truncation marker.
Announcements and direct messages bypass the batching delay, but obey pausing.
Received, queued, written, dropped, parser skips and pauses are separate counters.
Written means written to MCP, not acknowledged by Claude. Queue overflow may
ultimately cause replay gaps. Write failures are counted as dropped.

**Process restarts lose replay history, subscription intent, deduplication state
and delivery cursors.** The new process waits for a hook again. Existing peers
retaining subscription intent reconnect and see the new stream as a restart gap.
There is no disk-backed event history or durable outbox.

## Feedback controls

Received envelopes are never published as local events. Self-origin echoes and
repeated event IDs are rejected, injected transcript records are excluded, and
Rhizome's own tool calls/results are filtered. `publish` and `send` share a limit
of ten calls per rolling minute per node.

After 30 peer notifications in a rolling minute, the receiver pauses further
peer delivery and writes one local pause notice. Incoming queues stay bounded.
Explicit `subscribe(session_id)` re-arms that origin; other paused origins remain
paused. A subsequent burst can trip the breaker again. Unsubscribe does not
re-arm delivery. Channel instructions discourage unsolicited work/replies.

These controls contain feedback; they do not guarantee silence. In the real
bidirectional smoke test, Claude generated new commentary saying it was not
replying. That commentary is genuine local activity and is streamed. Unsubscribe
or leave feeds paused when you do not want peer-triggered work.

## Collaboration skill

`skills/rhizome-collab/` is a user-invoked skill (Claude Code or Codex) that guides
an agent through listing peers, naming its own session after a concise task
description, judging relevance, subscribing deliberately, treating peer activity
and names as untrusted, and replying without feedback loops. The skill is
embedded in the binary; install it with:

```sh
./rhizome setup                     # interactive: pick agents with a checkbox list
./rhizome setup --agents all        # non-interactive: every known agent
./rhizome setup --agents claude     # or codex, or claude,codex
./rhizome setup --list              # show what was detected and where skills go
```

Setup detects Claude Code (`$CLAUDE_CONFIG_DIR` or `~/.claude`, or `claude` on
PATH) and Codex (`$CODEX_HOME` or `~/.codex`, or `codex` on PATH). The picker
pre-selects detected agents; space toggles one, `a` toggles all, enter confirms.
Without a terminal, `--agents` is required. The skill is written to
`<agent dir>/skills/rhizome-collab/SKILL.md`; re-running setup updates it in
place after upgrading Rhizome. An existing symlink at that path is left alone, so
a development checkout can still be linked instead:

```sh
ln -s "$PWD/skills/rhizome-collab" ~/.claude/skills/rhizome-collab   # Claude Code
ln -s "$PWD/skills/rhizome-collab" ~/.codex/skills/rhizome-collab    # Codex
```

Then run `/rhizome-collab` in the agent with an optional task description.

## Verification

```sh
task check                         # go vet ./... and go test -race ./...
task build
python3 scripts/smoke_stdio.py      # real processes, valid stdout, EOF, restart
python3 scripts/smoke_service.py    # observer/direct coexistence and restart
python3 scripts/smoke_stdio.py --lan  # same, LAN mode: separate registries, pinned TLS
```

Tests cover binding generations and stale hooks, simultaneous bootstrap,
ambiguous routing/authentication, replay boundaries/gaps, overflow, incremental
partial/duplicate/truncated transcripts, MCP handshake, rate limits, session
name precedence/validation/persistence/gossip budget, and the receiver circuit
breaker. The subprocess smoke tests use a private temporary
cluster and synthetic transcripts; they do not launch models.

For the interactive acceptance checklist and recorded results, see
[docs/verification.md](docs/verification.md). Re-run the interactive checks when
upgrading Claude, the Go MCP SDK, hook configuration, or transcript parsing.

To regenerate the checked-in gRPC/protobuf files, install `protoc`, then run
`task proto` (pinned Go generators install into `.bin`). Override the compiler
with `PROTOC=/path/to/protoc task proto`. Production service
`rhizome.v1.Channel` is separate from the legacy demo service.

## Original demo

Randomized subscriptions are available only via an explicit command:

```sh
./rhizome demo -gossip 7946 -grpc 9000
./rhizome demo -gossip 7947 -grpc 9001 -join 127.0.0.1:7946
```

The demo retains its original unauthenticated, live-only `sessions` channel and
fixed-port flags. It is not used by `rhizome mcp` and does not join its cluster.
