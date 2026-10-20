# Background session observer

`rhizome service install` installs and starts a per-user observer. It discovers
coding agent logs without requiring Rhizome in those agents, including
headless sessions. Existing MCP/channel peers can list and subscribe to their
exact session IDs. A passive session cannot receive messages: `send` returns an
observation-only error until an integrated endpoint is available. The same
process is the single writer of the local [session-history catalog](history.md)
that `search_sessions` and `read_session_history` query.

```sh
rhizome service install
rhizome service status
rhizome service stop
rhizome service start
rhizome service uninstall
```

Installation writes only the native service definition, containing the absolute
binary path and `service run --config <absolute TOML path>`. Keep that binary
installed at its recorded path. There is no separate service JSON snapshot.
Uninstall removes only the definition, preserving `config.toml`, agent logs,
tokens, coordination files, and diagnostic logs. It does not edit agent
configuration or start an agent.

Create `~/.rhizome/config.toml` before installing, with `rhizome setup` (it asks
for the installation type; see the README's
[Collaboration skill](../README.md#creating-the-configuration-file)) or by hand
(an empty file selects defaults), or select an existing file with `--config`.
The observer does not peer, so the `[network]` table has no effect on the service.
The service can also run an opt-in [host gateway](gateway.md) via `[gateway]`;
that gateway bridges local sessions without changing observer collection or history. See the README's
[configuration table](../README.md#configuration) for keys and precedence.
Installation rejects path overrides that differ from the file: put persistent
paths in TOML so a service launched without the installing shell's environment
uses the same values. Foreground `service run` still accepts temporary flags and
environment overrides. For example:

```toml
runtime_dir = "/absolute/private/run"
[claude]
projects_dir = "/absolute/claude/projects"
[codex]
sessions_dir = "/absolute/codex/sessions"
```

```sh
rhizome service install --config /absolute/config.toml
rhizome service run --config /absolute/config.toml
```

The default roots derive from the effective agent directories: Claude's
`projects` and Codex's `sessions`. MCP and the observer must share the same
runtime directory to coordinate collection. Use the same `--config` path for
management commands when installing with a custom file. Configuration is read
once per process; edits take effect on the next start. The shared `hostname`
setting also labels observer sessions; blank uses the OS short hostname.
Reinstalling the same running definition is idempotent. Stop/uninstall before replacing a definition;
installation refuses to switch away from a running observer's runtime. The
definition includes runtime metadata solely for this check, not for resolution.

For foreground diagnostics or a custom service manager, use
`rhizome service run --runtime-dir ... --claude-root ... --codex-root ...`.
Configuration/root paths are local inputs; peer messages cannot change them.
Roots must not overlap. Symlink entries and Claude `subagents` directories are
not followed.

## Platform lifecycle

| OS | Native facility | Startup / recovery | Diagnostics |
| --- | --- | --- | --- |
| macOS | User LaunchAgent, `com.rhizome.observer` | Login to the GUI user session; launchd restarts failures after throttling. `stop` unloads, `start` loads again. Requires a GUI login domain. | `service status`, runtime logs, `launchctl print gui/$(id -u)/com.rhizome.observer` |
| Linux | `systemd --user`, `rhizome-observer.service` | User-manager startup, restart on failure after five seconds. No automatic linger configuration; an administrator can configure linger if operation after logout is needed. | `service status`, `journalctl --user -u rhizome-observer` |
| Windows | Task Scheduler, `Rhizome Observer <user SID>`, current user's interactive token, least privilege | User logon; runs independently of the installing shell while the user is logged in. IgnoreNew prevents overlapping task instances; retries failures up to three times at one-minute intervals. No SYSTEM service or stored password. | `service status`, Task Scheduler, runtime logs |

Installation needs permission to register the current user's service/task. No
silent elevation is attempted. Linux without a user systemd manager and macOS
without a GUI login domain return the native manager's error. Windows task policy
can disallow registration; configure permission through the administrator rather
than changing the collector's user. The task does not run while the user is logged
out. Native service-manager status is separate from observer health.

Installed observers write JSON diagnostics to `observer.log`, rotated at 2 MiB
with one backup. `observer-health.json` reports last update, watched directories,
tracked files, active observed publishers, degraded coverage, parser skips,
suppressed events, and a `catalog` block (indexing state, indexed files and
sessions, pending work, last write, coverage, warnings, counters). `status` verifies the process lock before reporting running.
Foreground `run` logs to stderr. No transcript contents or tokens are logged.
Windows runtime directories use a protected inheritable current-user DACL;
Unix runtime directories must be private and user-owned.

## Collection and history boundaries

The primary mechanism is fsnotify's native backend on each OS. Every discovered
directory gets a watch; nearest existing parents cover roots created later.
Directory creation triggers traversal of that subtree. Startup inventory establishes
EOF baselines for existing files; creation notifications queued during inventory
ensure newly created files are consumed from byte zero. A one-second maintenance
timer publishes health, retires idle publishers, and continues bounded reads; it
does not poll unchanged files or rescan the tree. Watch errors/limits mark coverage
degraded and retry inventory at most every 30 seconds.

Claude identity comes from `sessionId` and `cwd`; sidechain records are excluded.
Codex identity comes from `session_meta` (`id` or `session_id`, `cwd`), rejecting
subagent sources. Codex `response_item` messages and tool calls/results provide
summaries; mirrored `event_msg` assistant text is ignored. These fixtures follow
locally inspected Claude 2.1.302 and Codex 0.163.0 JSONL structures. Unknown schemas
are not guessed. Thinking/reasoning, user text, full tool bodies, received peer
envelopes, Rhizome tool calls, and peer-only reactions are excluded. Tool arguments
and results are never included in summaries. Tool results without a known call
are ignored. Codex result summaries say completed, without inferring success from
arbitrary output text.

Incomplete lines wait for more bytes. A record is capped at 2 MiB and each read
pass at 8 MiB. There are at most 4,096 watched directories, 1,024 tracked files,
and 64 active observed publishers. Capacity exhaustion is visible as degraded
coverage; narrow roots or restart after moving old logs outside the watched tree.
Publisher and subscriber limits from the main README are unchanged. Historical
files are not advertised until activity is observed. Publishers idle for ten
minutes retire; a later append recreates one. “Observed (liveness unknown)” means
log activity was seen, not proof that the agent process remains alive.

Replacement/truncation baselines at current EOF with a gap instead of replaying
potentially duplicated history. Observer restart also baselines existing files.
There is no durable outbox: writes during downtime or ownership transitions may
be unavailable and replay gaps are reported. A conservative startup baseline
suppresses automatic output until a subsequent human prompt when previous
peer-only turn state cannot be established.

## Live observation versus historical indexing

Live observation starts every existing file at its current EOF and only
publishes bytes appended afterwards. Historical indexing is a separate pass over
the same inventory: every `.jsonl` file enumerated by the walk is queued and
parsed from its last checkpoint for identity, timestamps, and titles only, with
a 4 MiB budget per maintenance tick so backfill never stalls live reads. The
indexer does not create publisher nodes, acquire collection leases, or emit
events; a pre-existing file becomes searchable through `search_sessions` while
`list_sessions` still does not list it until new activity appears. The catalog
lives at `<runtime_dir>/history/catalog.json`, is bounded to 4,096 files, and
stays readable (reported as `stale`) after the service stops. Shutdown flushes
pending catalog changes and removes only `observer-health.json`. Limits,
tombstones, rebuilds, and root changes are documented in [history.md](history.md).

## Direct integration coexistence

All updated processes in one runtime share OS locks keyed by exact session ID.
A direct integration acquires its collection lease before establishing a baseline,
and keeps it until rebinding/shutdown. A duplicate direct collector is rejected
at binding; legacy direct/direct ambiguities still fail closed during routing.
An observer holds that lock only for an incremental scan. While a direct lease
exists it consumes logs without publishing, keeping parsing state current.
An epoch records each direct lease acquisition, including direct processes that
start and stop between observer scans.

Discovery presents the direct integration as the logical session whenever it is
available. The observer advertises `observation_only` on the wire, and
`list_sessions` exposes `delivery_capable`. Direct messages keep using the live
integration. After a direct integration exits, the observer establishes a fresh
boundary and reports a gap. Subscription changes involving an observer start
live rather than replaying a second collector's history. Distinct same-text
records are not collapsed. No end-to-end exactly-once delivery guarantee is made.
Upgrade all cooperating binaries: old binaries do not honor collection leases or
understand passive capabilities. Separate runtime directories intentionally do
not coordinate. LAN service hosting is outside this feature.

## Verification

```sh
task build
task check
python3 scripts/smoke_stdio.py
python3 scripts/smoke_service.py
```

Go tests exercise actual filesystem notifications with no polling fallback,
missing roots/new nested directories, both ownership orders, partial lines,
rotation, filtering, duplicate direct rejection, passive delivery rejection, and
catalog indexing without publishing. The service smoke uses separate
observer/MCP processes and synthetic logs to verify subscription handoffs,
direct delivery, daemon restart, no duplicate replay, pre-existing logs becoming
searchable and readable, and a stale catalog while the service is stopped. No models or real user logs are involved. Service-definition tests validate
XML and arguments; they do not prove installation/startup on another operating
system. Native install/login/recovery tests must be recorded per platform.
