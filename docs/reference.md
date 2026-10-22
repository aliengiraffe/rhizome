# Setup and protocol reference

For an introduction and quickstart, see the [README](../README.md).
This reference covers configuration, agent installation, tools, networking,
and delivery behavior. Run shell examples from the repository root when using
a source build.

## Configuration

MCP, setup, and every service action read `~/.rhizome/config.toml` once at
startup. A missing default file keeps the built-in defaults. Select another file
with `RHIZOME_CONFIG` or the subcommand's `--config /absolute/path.toml` flag
(`service run --config ...` for service definitions). An explicitly selected
file must exist. Reading configuration never creates directories or files.

Precedence is **explicit flag > nonempty environment variable > file > default**.
Run `rhizome config` to print every effective key, its source, and the selected
file path (including “not found”). It also accepts the runtime override flags in
the table for diagnostics; `setup --hostname` only writes the file. All startup diagnostics and errors go to stderr.

| TOML key | Environment | Flag | Default |
| --- | --- | --- | --- |
| `hostname` | — | setup `--hostname` (writes file) | empty: OS hostname |
| `runtime_dir` | `RHIZOME_RUNTIME_DIR` | service `--runtime-dir` | `~/.rhizome/run` |
| `claude.config_dir` | `CLAUDE_CONFIG_DIR` | — | `~/.claude` |
| `claude.projects_dir` | — | service `--claude-root` | effective `claude.config_dir` + `/projects` |
| `codex.bin` | — | mcp `--codex-bin` | `codex` |
| `codex.home` | `CODEX_HOME` | — | `~/.codex` |
| `codex.sessions_dir` | — | service `--codex-root` | effective `codex.home` + `/sessions` |
| `opencode.data_dir` | — | service `--opencode-root` | `$XDG_DATA_HOME/opencode`, else `~/.local/share/opencode` |
| `network.mode` | `RHIZOME_LAN`, `RHIZOME_TAILSCALE` | mcp `--lan`, `--tailscale` | `local` |
| `network.bind_addr` | `RHIZOME_BIND_ADDR` | mcp `--bind-addr` | LAN: all interfaces; Tailscale: the Tailscale IP |
| `network.advertise_addr` | `RHIZOME_ADVERTISE_ADDR` | mcp `--advertise-addr` | first private interface; Tailscale: the Tailscale IP |
| `network.join` | `RHIZOME_JOIN` | mcp `--join` | none |
| `network.mdns` | `RHIZOME_MDNS` | mcp `--mdns` | on in LAN mode, off in Tailscale mode |
| `network.gossip_port` | `RHIZOME_GOSSIP_PORT` | mcp `--gossip-port` | `7946` (Tailscale only) |
| `network.discovery_interval` | `RHIZOME_DISCOVERY_INTERVAL` | mcp `--discovery-interval` | `20s` (Tailscale only) |
| `gateway.history.enabled` | — | — | `false`: paired hosts cannot search or read history |
| `gateway.history.lookback` | — | — | `30d`; positive whole days or `all` |
| `claude.peer_history.enabled`, `codex.peer_history.enabled` | — | — | inherit `gateway.history.enabled`; can only narrow it |
| `claude.peer_history.lookback`, `codex.peer_history.lookback` | — | — | inherit `gateway.history.lookback` |

```toml
runtime_dir = "~/.rhizome/run"
hostname = "" # optional device display label; blank uses the OS hostname

[claude]
config_dir = "~/.claude"
# Omit, or leave empty, to derive from config_dir.
projects_dir = ""

[codex]
bin = "codex"
home = "~/.codex"
# Omit, or leave empty, to derive from home.
sessions_dir = ""

# Optional. Omit the table, or use mode = "local", for loopback only.
[network]
mode = "local"   # or "lan", or "tailscale"
```

`hostname` controls the `host` label in session listings for both supported agents
and observer sessions. A missing, empty, or whitespace-only value uses the OS
hostname at startup (the portion before the first dot, bounded to 64 bytes).
Custom labels are trimmed, must be valid UTF-8, contain no control characters,
and fit in 64 bytes. Labels need not be unique and never affect routing or DNS.
Restart MCP processes and the observer after changing this setting. This applies
in every network mode; Tailscale users who want their previous MagicDNS or
Tailscale display label can set it explicitly.

The `[network]` table selects how `rhizome mcp` peers; see [LAN mode](#lan-mode)
and [Tailscale mode](#tailscale-mode) for what each option means. Options that
the chosen mode would refuse at startup (`join` in local mode, `gossip_port` outside
Tailscale mode, an unknown `mode`) are rejected when the file is read. The
background service uses `[gateway]` for paired-host networking; `[network]`
controls MCP gossip and must be local when pairing is enabled. `rhizome setup`
can write this file for you ([Collaboration skill](#collaboration-skill)).

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

The `gateway.history` and `*.peer_history` keys are file-only. They let directly
[paired hosts](gateway.md) search and read this device's session history
for the configured source agents and lookback windows. Sharing is off unless
`gateway.history.enabled = true`, and the service applies changes on restart.
See [paired-host history](history.md#paired-host-history-opt-in).

The cluster token is **never configuration**: it remains at `<runtime_dir>/token`
with 0600 permissions. MCP and the observer should share `runtime_dir` through
this file; per-process overrides can intentionally isolate them. Installed
services pin the absolute TOML path and reread it on restart; see
[service configuration](service.md) for installation requirements.

## Codex

Local Codex sessions use `rhizome mcp --agent codex`, with the same thirteen tools and
cross-agent subscriptions. Incoming events use `codex queue`; outgoing activity
comes from read-only App Server polling. Each caller thread has an independent
node even when Codex shares an MCP connection. See [Codex setup and tested
limitations](codex.md) and the samples in `examples/codex/`.

## Install

Tagged releases on the [GitHub Releases page](https://github.com/aliengiraffe/rhizome/releases)
ship a signed and notarized macOS installer (`rhizome_<version>_macos.pkg`, which
installs `/usr/local/bin/rhizome`) and a universal macOS archive. They also ship
**unsigned** Linux and Windows archives for amd64 and arm64, and a SHA-256
manifest (`rhizome_<version>_checksums.txt`). Verify downloads with
`shasum -a 256 -c rhizome_<version>_checksums.txt`, then check the build with
`rhizome --version`. The installer never touches `~/.rhizome` and never installs
or restarts the observer. Run `rhizome setup` and `rhizome service install`
yourself, and after an upgrade run `rhizome service stop` and then
`rhizome service start`. Tag conventions, prereleases, verification, upgrades,
uninstall and platform limits are in [docs/releasing.md](releasing.md).
To build from source, follow the steps below.

## OpenCode

Local OpenCode 2.x sessions use `rhizome mcp --agent opencode` (twelve tools; no
`observe_session`) plus an embedded relay plugin installed by
`rhizome setup --agents opencode`. Configuration and `[network]` modes apply as
for the other agents. A session binds on its first human prompt; its
identity is `_meta["ai.opencode/sessionID"]`, child sessions are refused. The
plugin talks to Rhizome over a private Unix socket, relays allowlisted activity
(assistant text, tool names, turn completion, titles) and delivers peer events
with `ctx.session.synthetic` (queued, waking idle sessions). OpenCode's TUI hides
synthetic input, so setup also installs a display-only TUI plugin that shows a
toast with a sanitized excerpt of each peer delivery. Add the MCP entry
by hand from `examples/opencode/`; tested with OpenCode **2.0.32**. See
[OpenCode setup and limits](opencode.md).

## Claude setup

Building from source requires Go 1.27.3+ and [Task v3](https://taskfile.dev/docs/installation).
The MCP integration is tested on macOS and Linux.
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

For every other project, `./rhizome setup --agents claude` registers the server
at user scope (`~/.claude.json`) and installs the same hooks in
`~/.claude/settings.json`; see [Installing into agents](#installing-into-agents).
A project `.mcp.json` entry named `rhizome` takes precedence over the user-scope
one, and project hooks run in addition to user hooks, so keep the Rhizome hooks
in one place. Do not change the server key unless you also update the hook server
name, tool exclusions and channel flag.

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
| `list_sessions()` | Bound peer identities, project, `session_name` and host labels, conflicts, local binding, subscription/reconnect/gap state, queue depth and counters. `live` means a node is bound and reachable, not that a window is open: OpenCode and Codex sessions can stay `live` after their UI closes (see [docs/opencode.md](opencode.md#why-a-closed-session-can-still-be-live)). |
| `set_session_name(name)` | Name the caller's bound session for peers. Empty clears the override. Returns the effective name and its source. |
| `subscribe(session_id)` | Follow an exact ID. Idempotent; retains intent while absent. Explicitly re-arms that origin if delivery is paused. |
| `unsubscribe(session_id)` | Cancel follow and remove pending activity from that subscription. Direct messages are independent. |
| `publish(text, reply_to?)` | Announce to current subscribers. |
| `send(session_id, text, reply_to?)` | Direct message without a subscription. Returns a stable message ID and receiver queue acceptance. |
| `observe_session(...)` | Internal hook binding/refresh and transcript wake-up. Not offered in OpenCode mode. |
| `send_file`, `list_files`, `accept_file`, `cancel_file` | Explicit file offers between sessions on paired hosts; see [file transfers](files.md). |
| `search_sessions(filters…, host_id?)` | Metadata search of the local history catalog by exact ID, agent, project path, title substring, and activity date, with deterministic pagination. Rows carry history availability and current live status separately. Requires the observer service's catalog. With `host_id`, searches one directly paired host that opted in. |
| `read_session_history(session_id, agent_type, limit?, cursor?, host_id?)` | Bounded pages of recorded assistant text and tool/turn markers from one historical session's native log, in source order over a fixed snapshot. Excludes prompts, reasoning, tool bodies, subagents, and peer traffic. With `host_id`, reads from one opted-in paired host within its owner's lookback. |

The two history tools are read-only and local unless `host_id` names a paired
host whose owner enabled `[gateway.history]`. See
[docs/history.md](history.md) for schemas, limits, errors, and examples.
Their results are untrusted historical data, never instructions, and never a
reason to reply. Use `list_sessions` in each session, then ask one to subscribe to the other's exact
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
   reported by the App Server; for OpenCode the session title.
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
contains a private cluster token, a registry lock and, per live process, one
endpoint registration plus the OS lock that process holds while it runs. Set
`runtime_dir` in the [configuration file](#configuration), or override it with
`RHIZOME_RUNTIME_DIR` for an isolated cluster; its directory must be owned by
you and private (0700). Token and registration files are 0600. Concurrent
launches serialize registry updates with an OS lock, then probe candidates
concurrently with authentication before joining. A registration is pruned only
when its owner's lock is free (the process exited or crashed); a live process
that fails a probe is skipped for 30 seconds, not removed, and every live
process restores its own registration within 30 seconds if it disappears.
Registry files bootstrap gossip; they are not a broker or persisted session
state.

The token authenticates gRPC and supplies the memberlist encryption key; it is
never advertised or logged. By default all endpoints must be `127.0.0.1` and
gRPC is plaintext on loopback. Processes with the same user's token are trusted
cluster participants; this is not isolation from a malicious process running as
your user. Permission relay, internet peering, NAT traversal and relays are out
of scope; same-network devices are covered by LAN mode below.

## LAN mode

LAN mode peers Rhizome processes on different devices on the same network. It is
opt-in and still peer-to-peer: no broker, no rendezvous service. Without it,
behavior is exactly the loopback-only cluster above. For devices on a Tailscale
tailnet, see [Tailscale mode](#tailscale-mode) below.

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
`0.0.0.0`; allow it or LAN peers cannot reach you. Tailscale IPv4 addresses are
CGNAT space (`100.64.0.0/10`), which the LAN policy does not treat as private, so
`--lan --advertise-addr 100.x.y.z` is rejected; use Tailscale mode below. On
another WireGuard overlay whose addresses are RFC 1918 or ULA, bind and advertise
the overlay address; TLS and the token still apply, so the design does not depend
on the overlay.

### Tailscale mode

Tailscale mode peers Rhizome processes across a tailnet with no `--join`, no
copied endpoints and no `--advertise-addr`. It relies on the host's running
Tailscale daemon (not an embedded `tsnet` node) and implies the LAN transport:
TLS on gRPC, fingerprint pinning, encrypted gossip. Every device must run a build
with Tailscale mode and the same token.

```sh
# Device A: print the cluster token; copy it to every device.
./rhizome token

# Every device:
RHIZOME_TOKEN=<token> ./rhizome mcp --tailscale             # flag
RHIZOME_TOKEN=<token> RHIZOME_TAILSCALE=1 ./rhizome mcp     # or env, for .mcp.json
```

| Flag / env | Default | Meaning |
| --- | --- | --- |
| `--tailscale` / `RHIZOME_TAILSCALE=1` | off | Bind and advertise the Tailscale IP, enumerate tailnet peers, LAN transport. Takes precedence over `--lan`. |
| `--gossip-port` / `RHIZOME_GOSSIP_PORT` | `7946` | Beacon port each device tries to hold (below). |
| `--discovery-interval` / `RHIZOME_DISCOVERY_INTERVAL` | `20s` | How often online tailnet peers are enumerated and joined. |
| `--bind-addr`, `--advertise-addr` | this device's Tailscale IPv4 | Explicit values still win. `--bind-addr 0.0.0.0` exposes the ports on every interface and brings back the macOS prompt. |
| `--mdns` | off | mDNS cannot cross the tunnel; pass `--mdns` to force it on for mixed setups. |
| `--join host:port` | none | Still works, with MagicDNS names: `--join desktop.<tailnet>.ts.net:7946`. |

At startup Rhizome runs `tailscale status --json` (the CLI is looked up on `PATH`,
then on macOS at `/usr/local/bin/tailscale`, `/opt/homebrew/bin/tailscale` and
`/Applications/Tailscale.app/Contents/MacOS/Tailscale`, with `TERM` set when unset
so the app binary acts as the CLI under launchd; verified with the standalone
macOS build 1.104) and binds gRPC and gossip to this device's Tailscale
IPv4, so nothing is exposed on the physical LAN. The `host` label uses the
configured `hostname` or OS hostname, independently of MagicDNS. Every discovery
interval it enumerates online peers and, for each device not yet in the cluster,
attempts a gossip join against `<magicdns name>:<beacon port>` (the Tailscale IP
when MagicDNS is disabled). As with mDNS and `--join`, memberlist's token-keyed
push/pull is the authentication step: a device without Rhizome, or with another
token, fails to join, is logged as `tailnet_peer_unreachable` (once at warn, then
at debug) and is retried on the next tick. Gossip uses memberlist's WAN profile so
DERP-relayed peers do not flap.

**Beacon port.** MagicDNS has no SRV records, so a remote device needs a
predictable port to try. gRPC and gossip ports stay ephemeral per process; in
addition, the first process on a device listens on the beacon port (default 7946)
and forwards those TCP connections to its own gossip listener. One reachable
process per device is enough: the join returns the whole membership with real
addresses. Later processes on the same device log `beacon_busy`, are reachable
through the holder, and re-acquire the port on their next discovery tick after the
holder exits, so the device stays discoverable. The `demo` subcommand's
unauthenticated loopback cluster also uses 7946, but on `127.0.0.1`; it cannot
join a Tailscale cluster.

**Tailnet ACLs.** Discovery needs TCP to the beacon port; gossip (TCP and UDP)
and gRPC (TCP) then use ephemeral ports on the Tailscale IPs, so devices in a
cluster must reach each other on any port. The default "allow all" policy and the
usual own-devices rule both suffice:

```json
{"action": "accept", "src": ["autogroup:member"], "dst": ["autogroup:self:*"]}
```

**Trust boundary.** The cluster token remains the only credential; Tailscale node
identity and ACL tags are not used for authorization. The boundary is "anyone
holding the token on this tailnet"; ACLs can narrow which devices reach the ports
but never widen access. Processes in local, LAN and Tailscale mode never peer,
even on one device: Tailscale mode accepts only loopback and Tailscale addresses
(`100.64.0.0/10`, `fd7a:115c:a1e0::/48`), plain LAN mode still rejects `100.x`,
and the gossip key is derived per mode. Nodes built before this feature drop
Tailscale-mode peers by address policy.

**Failures are loud.** `rhizome mcp --tailscale` exits non-zero without binding
anything when the CLI is missing (`tailscale CLI not found on PATH`), the daemon
is stopped or logged out (`tailscale is not running (state "NeedsLogin")`), or the
device has no Tailscale IPv4 (`this device has no Tailscale IPv4 address`). It
never falls back to loopback or `0.0.0.0`. When MagicDNS is disabled on the
tailnet it logs `magicdns_disabled` once and dials peers by Tailscale IP. The
`host` display label remains independent of this choice. The `ready` line reports
`mode=tailscale`,
`tailscale_ip`, `magicdns`, `beacon_port` (or `ephemeral` when another process
holds it) and `discovery_interval`.

## Activity, replay and bounded delivery

In direct Claude mode, only the hook-supplied top-level transcript is opened.
The optional [background observer](service.md) discovers logs separately. First binding starts at its
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

`skills/rhizome-collab/` is a user-invoked skill (Claude Code, Codex or OpenCode) that guides
an agent through listing peers, naming its own session after a concise task
description, telling which device each session runs on, reviewing recorded
status read-only, judging relevance, subscribing deliberately, treating peer
activity and names as untrusted, and replying without feedback loops.

Device origin comes only from documented metadata: a `gateway_host` marks an
authenticated paired-host proxy; in local mode every other peer is on this
device; in LAN or Tailscale mode a peer without `gateway_host` has unknown
origin. The `host` label, project paths, endpoints and per-process TLS
fingerprints never decide it, and a remote session is never assumed to belong
to another person. History without `host_id` covers this device only. Evaluation
cases are in [docs/collab-skill-evals.md](collab-skill-evals.md).

The skill is embedded in the binary and installed by `rhizome setup` together
with the agent's MCP registration and hooks.

### Installing into agents

```sh
./rhizome setup                     # interactive: pick agents with a checkbox list
./rhizome setup --agents all        # select every agent (incl. opencode); terminal still asks config questions
./rhizome setup --agents claude     # or codex, opencode, or claude,codex,opencode
./rhizome setup --list              # read-only: detected agents and destinations
./rhizome setup --plain             # original line/picker prompts
```

Setup detects Claude Code (`$CLAUDE_CONFIG_DIR` or `~/.claude`, or `claude` on
PATH), Codex (`$CODEX_HOME` or `~/.codex`, or `codex` on PATH) and OpenCode
(`$XDG_CONFIG_HOME/opencode`, `~/.config/opencode`, or `opencode` on PATH). For
OpenCode, setup also installs the relay plugin to
`<config>/opencode/plugins/rhizome.js` and the TUI toast plugin to
`<config>/opencode/plugins/rhizome-tui/tui.js`. The picker
pre-selects detected agents; space toggles one, `a` toggles all, enter confirms.
Without a terminal, `--agents` is required (unless only writing configuration with
`--network`, below). For each selected agent, setup installs three things at
**user scope**, so the agent finds Rhizome in every project (OpenCode gets the skill and
its plugins; its MCP entry is added by hand):

| Agent | MCP registration | Hooks | Skill |
| --- | --- | --- | --- |
| Claude Code | `mcpServers.rhizome` in `~/.claude.json` (or `<CLAUDE_CONFIG_DIR>/.claude.json`) | `hooks` in `<config dir>/settings.json` | `<config dir>/skills/rhizome-collab/` |
| Codex | `[mcp_servers.rhizome]` in `<CODEX_HOME>/config.toml` | `<CODEX_HOME>/hooks.json` | `<CODEX_HOME>/skills/rhizome-collab/` |
| OpenCode | not edited; add it by hand ([docs/opencode.md](opencode.md)) | none; the plugins below relay events | `<config>/opencode/skills/rhizome-collab/` |

The registered command is the absolute path of the running `rhizome` executable
with `mcp --agent <name>`, so keep that binary where it is or re-run setup after
moving it. `--config <absolute path>` is added only when the file was selected
with `--config`/`RHIZOME_CONFIG` (the default `~/.rhizome/config.toml` resolves
at launch), and only if it exists. A non-default Claude config directory or
Codex home is pinned in the entry's `env` (`CLAUDE_CONFIG_DIR`/`CODEX_HOME`),
and a Codex executable found on PATH is pinned with `--codex-bin`. The token is
never written anywhere. Hooks are the same definitions as `.claude/settings.json`
and `examples/codex/hooks.json`. Setup resolves the destinations after the
configuration questions, so directory answers apply to the same run.

Writes are format-aware merges that touch only Rhizome's entries: other servers,
hooks, matchers, preferences and credentials are kept, a changed file is first
copied to `<file>.bak-<timestamp>`, and a symlinked file is edited through the
link. A malformed file is reported and left untouched, and a failed write keeps
the original. Re-running setup is idempotent: it repairs a stale registration
(for example after moving the binary), refreshes the skill after an upgrade
(`unchanged` when current), never duplicates hooks, and does not rewrite or back
up a file that needs no change. An existing skill symlink is left alone, so a
development checkout can still be linked instead:

```sh
ln -s "$PWD/skills/rhizome-collab" ~/.claude/skills/rhizome-collab   # Claude Code
ln -s "$PWD/skills/rhizome-collab" ~/.codex/skills/rhizome-collab    # Codex
```

Setup reports each file and backup, then the steps it cannot do for you:

- **Claude Code**: restart it and start with
  `claude --dangerously-load-development-channels server:rhizome`; accept the
  channel and hook prompts. Project `.mcp.json` entries named `rhizome` override
  the user-scope entry; project hooks run in addition, so remove the Rhizome
  hooks from a project (this checkout ships them) or from
  `~/.claude/settings.json`. Setup never edits projects.
- **Codex**: start a fresh session, review the Rhizome hooks in `/hooks` and
  trust them. Do not keep the same hooks at project scope.

Then run `/rhizome-collab` in the agent with an optional task description.

### Creating the configuration file

With terminal stdin/stdout and `TERM` other than `dumb`, setup uses a Bubble Tea
wizard when answers are needed. Enter advances, Shift+Tab goes back while retaining
answers, arrows/space select choices, Ctrl+U clears a text field, and Esc/Ctrl+C
cancels. Review shows the selected skills, configuration and service decision before
Apply; cancelling before Apply writes nothing. After Apply begins, completed operations
remain and are reported even if a later step fails or is cancelled. PgUp/PgDn
scroll long views. `--plain` retains the original prompts (and their immediate
skill installation behavior). Fully specified `--agents` plus `--network`
invocations stay noninteractive.

Setup asks how Rhizome will be used and writes
`~/.rhizome/config.toml` (or the file selected with `--config`/`RHIZOME_CONFIG`,
which setup may create):

1. **Gateway pairing**: `off` (default), `lan`, or `tailscale`, with optional
   concrete listen/advertise IP:port endpoints. This connects per-user services
   through explicit invite/join, independently of the local cluster token.
2. **MCP network**: `local` (default), or legacy shared-token `lan`/`tailscale`
   gossip. Pairing requires local MCP networking; incompatible choices fail.
   Legacy mode options retain their existing meaning. Tailscale probing is
   advisory in the wizard; startup still fails closed if the daemon is unavailable.
3. **Hostname override**: Enter keeps the stored label; `-` clears it to use the
   OS hostname. Optional gateway endpoints also accept `-` for automatic selection.
4. **Directories**: optionally change runtime scope, Claude config directory,
   Codex home or executable. Changing runtime scope selects different token,
   host identity and pairing state; the old state remains in the old directory.
5. **Service**: asks whether to install and start the background service
   (default no). The question follows the configuration step whether the file
   was created, updated, or kept, and runs the same lifecycle as
   `rhizome service install --config <path>`: a missing service is installed
   and started, a stopped compatible one is started, and a running one counts
   as success without a restart. Setup reports success only after the observer
   is running and names the configuration file it used. On failure the written
   configuration and any installed definition stay in place for diagnosis;
   declining leaves the service untouched.

Prompts default to stored choices, without persisting shell overrides. Keeping
an existing file leaves it byte-identical (the service step is still offered). Updating merges selected settings and
preserves all other supported keys, including history policies, agent options,
and runtime scope. Switching MCP transport resets that transport's options;
explicit gateway `off` clears gateway endpoints but preserves history policy.
Updates normalize TOML formatting/comments, validate before replacement, and
save a private recoverable `config.toml.bak-<timestamp>` backup (with a suffix
on collisions). New files are 0600 under a 0700 directory. Setup never writes
tokens or changes host keys, trust, invitations, or revocations.

Without a terminal, use `--network` or `--gateway-mode`:

```sh
./rhizome setup --config /absolute/path/config.toml --gateway-mode lan --gateway-listen 192.168.1.10:7947
./rhizome setup --gateway-mode tailscale --network local --replace-config
./rhizome setup --gateway-mode off --replace-config
./rhizome setup --network local --hostname "Work laptop"
./rhizome setup --network local --hostname "" --replace-config # restore OS hostname
./rhizome setup --network lan --join 192.168.1.20:7946 --mdns=false
./rhizome setup --network tailscale --gossip-port 7950 --discovery-interval 45s
```

`--gateway-advertise IP:port` sets a separate reachable endpoint; an explicit
empty gateway endpoint restores automatic selection. `--runtime-dir /absolute/path`
changes stored runtime scope. Omitted flags preserve stored values. To switch an
existing legacy network to pairing, explicitly pass `--network local`; to switch
back, pass `--gateway-mode off` with the desired `--network`. History sharing is
never enabled by setup; its policies remain file-only.

Restart the service and MCP processes to apply changes. Saved configuration is
not proof of connectivity; use [gateway pairing and verification](gateway.md).
Combine with `--agents` to also install the selected agents; the configuration
is written first so its directories decide the destinations. `--network`,
`--gateway-mode` and `--list` never install or start the service. Without a
terminal and without `--network` or `--gateway-mode`, setup installs the selected
agents, leaves configuration untouched and says so on stderr; it refuses to
register an explicit `--config` file that does not exist yet. The terminal screen
replaces a file by merging its answers into the stored one, so settings it does
not ask about (gateway pairing, history policy, agent options) survive; use the
flags or `--plain` to change gateway pairing.

Bare `rhizome service` opens [service management](service.md#terminal-interface)
on usable terminals. `rhizome service status --tui` and
`rhizome peers list --tui` provide read-only views. Plain `peers list` always
prints JSON. Explicit `--tui` fails promptly for redirected streams or `TERM=dumb`;
help/list commands and `service run` never open a TUI. Screens need no color.

## Verification

```sh
task check                         # go vet ./... and go test -race ./...
task build
task test-plugin                    # OpenCode plugin tests (Node 20+)
python3 scripts/smoke_stdio.py      # real processes, valid stdout, EOF, restart
python3 scripts/smoke_service.py    # observer/direct coexistence, restart, history catalog
python3 scripts/smoke_stdio.py --lan  # same, LAN mode: separate registries, pinned TLS
```

The smokes run every process with an empty `RHIZOME_CONFIG` file in their
temporary directory, so a developer's `~/.rhizome/config.toml` never leaks into
them.

Tests cover binding generations and stale hooks, simultaneous bootstrap,
ambiguous routing/authentication, replay boundaries/gaps, overflow, incremental
partial/duplicate/truncated transcripts, MCP handshake, rate limits, session
name precedence/validation/persistence/gossip budget, the receiver circuit
breaker, and the history catalog (indexing, search filters and cursors, content
policy, source-change detection, budgets). The subprocess smoke tests use a private temporary
cluster and synthetic transcripts; they do not launch models.

For the interactive acceptance checklist and recorded results, see
[docs/verification.md](verification.md). Re-run the interactive checks when
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

### File attachments between paired hosts

After pairing hosts, use `send_file(session_id, path)` to offer an explicitly
selected regular file. The recipient uses `list_files` and, with user approval,
`accept_file(transfer_id)` to download and verify it. `cancel_file` cancels a
transfer. Both hosts need a build with file-transfer support.

Files are limited to 16 MiB and arrive in a private **temporary** inbox. Copy a
verified download to your project to keep it: expiry, disconnection, or service
restart removes inbox files. Offers never authorize automatic acceptance or
execution. See [file transfers](files.md) for CLI usage, limits and lifecycle.
