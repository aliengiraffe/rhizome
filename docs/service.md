# Background session observer

`rhizome service install` installs and starts a per-user observer. It discovers
coding agent logs without requiring Rhizome in those agents, including
headless sessions. Existing MCP/channel peers can list and subscribe to their
exact session IDs. A passive session cannot receive messages: `send` returns an
observation-only error until an integrated endpoint is available. The same
process is the single writer of the local [session-history catalog](history.md)
that `search_sessions` and `read_session_history` query, and that catalog is
what lets `list_sessions` show sessions whose process ended within
`sessions.recent_window` as `recent` (metadata only, never a delivery
destination; see the README's [Session status](../README.md#session-status)).
While the service is stopped those rows are withdrawn, not fabricated.

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
for the installation type and then offers to install and start this service,
also when it keeps an existing file; see the setup reference on
[creating configuration](reference.md#creating-the-configuration-file)) or by hand
(an empty file selects defaults), or select an existing file with `--config`.
Accepting that offer runs the install lifecycle below for the selected file and
reports success only once the observer holds its runtime lock.
The observer does not peer, so the `[network]` table has no effect on the service.
The service can also run an opt-in [host gateway](gateway.md) via `[gateway]`;
that gateway bridges local sessions without changing observer collection or history.
With `[gateway.history]` enabled, the same gateway also serves this catalog
read-only to directly paired hosts under the owner's lookback policy. The policy
is read when the service starts, so restart the service after changing it. See
[paired-host history](history.md#paired-host-history-opt-in). See the reference
[configuration table](reference.md#configuration) for keys and precedence.
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
`rhizome service run --runtime-dir ... --claude-root ... --codex-root ... --opencode-root ...`.
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
with one backup. `observer-health.json` reports last update, the running `version`, watched directories,
tracked files, active observed publishers, degraded coverage, parser skips,
suppressed events, and a `catalog` block (indexing state, indexed files and
sessions, pending work, last write, coverage, warnings, counters). `status` verifies the process lock before reporting running.
Foreground `run` logs to stderr. No transcript contents or tokens are logged.
Windows runtime directories use a protected inheritable current-user DACL;
Unix runtime directories must be private and user-owned.

## Automatic updates

With `[update] enabled = true` (the default) the installed observer is the one
Rhizome process that contacts a remote host: every `check_interval` (default
`24h`, minimum `1h`) it asks the GitHub Releases API for `aliengiraffe/rhizome`
whether a newer release exists and, when one does, installs it over its own
executable and restarts onto it. `mcp`, `config`, `setup`, `--version` and
`service install|start|stop|status` never check. Set `enabled = false`, or
`RHIZOME_UPDATE=0`, and the observer makes no update request at all; the
explicit `rhizome update` command still works, because it is a user action
(`--check`, the default, only reports; `--apply` installs). `rhizome config`
shows the effective policy and its source; see the
[configuration table](reference.md#configuration).

Eligibility: only releases strictly newer by SemVer precedence are
considered; never a downgrade, never a prerelease unless the running build is
itself a prerelease, never anything for `dev` or snapshot builds, and
mislabelled, draft or malformed releases are ignored. Only the asset names
that the release matrix in `internal/release/assets.go` defines for this
platform are downloaded, from download URLs the updater constructs itself;
URLs in release bodies or API responses are never followed. Requests are
HTTPS with certificate verification, bounded by timeouts, size caps
(1 MiB API response, 64 KiB manifest, 4 KiB signature, 128 MiB archive,
256 MiB extracted binary) and at most five redirects that must stay on GitHub
hosts. Standard proxy environment variables are honored.

Verification is fail closed; the installed binary is not touched until all of
it passes:

1. `rhizome_<v>_checksums.txt.sig` must be a valid Ed25519 signature over the
   manifest by a key compiled into the running binary
   ([release signing](releasing.md#release-signing-key)). No signature, an
   unknown key or a tampered manifest rejects the release on every platform.
2. The downloaded archive's SHA-256 must equal the manifest entry, and the
   manifest must list exactly the expected assets.
3. The archive may contain only `rhizome` (or `rhizome.exe`), `LICENSE` and
   `README.md` as regular files at its root; extra paths, directories,
   symlinks, traversal and oversize members reject it.
4. On macOS the extracted binary must pass `codesign --verify --strict` and
   carry a Developer ID Application signature with hardened runtime, a secure
   timestamp, identifier `com.aliengiraffe.rhizome` and Team ID `6BY6T468D8`.
   Ad-hoc or differently signed binaries are rejected. Notarization is not
   re-checked online.
5. The candidate is executed with `--version` and must report exactly the
   release version and the tag's commit.

Install writes the candidate as a private temporary file in the executable's
real directory (symlinks resolved) and renames it over the target, keeping the
previous binary as `rhizome.previous` until the new one is confirmed running.
When that directory is not writable by the user (typical for the package at
`/usr/local/bin`), the observer reports `not_writable` once with the manual
command and does not escalate, prompt or change the service definition; later
checks keep the status current without downloading again. Attempts are
serialized across processes by `<runtime>/update.lock`.

Restart goes through the native lifecycle in `internal/service`. A running
process cannot supervise its own replacement, so after installing onto itself
the observer starts a detached finisher, `rhizome update --finish --config
<path>`, from the new binary (a transient `systemd-run --user` unit on Linux
when available, a new session on macOS, a detached process outside the task's
job on Windows). The finisher stops and starts the service, waits up to 60 s
for the observer lock plus `observer-health.json` reporting the new version,
and otherwise restores `rhizome.previous` and starts the service again. The
new observer confirms the update on start and removes the backup. Without a
native definition (a foreground `service run` under a custom supervisor) the
observer exits non-zero with `restart required` so its supervisor restarts it
on the new binary. `rhizome update --apply` from a shell restarts an installed,
running service directly and rolls back the same way. MCP processes keep
running the old binary until their agent client restarts them; run
`rhizome setup` again if a release changes skills or hooks.

Status lives in `<runtime>/update-state.json` (0600): running version, last
check, last outcome code, available version, blocked reason, pending restart
and the next check. `service status` prints it after the health JSON, and the
terminal view shows it as "Version", "Last update check", "Last update
outcome", "Update available", "Update blocked" and "Update pending restart".
Outcome codes are stable: `up_to_date`, `update_available`, `no_release`,
`dev_build`, `updated`, `installed`, `restart_pending`, `restart_required`,
`rolled_back`, `restart_failed`, `not_writable`, `locked`, `rate_limited`,
`network_error`, `timeout`, `oversize`, `redirect_refused`, `http_error`,
`invalid_release`, `signature_invalid`, `digest_mismatch`, `archive_invalid`,
`codesign_failed`, `version_mismatch`, `install_failed`. Logs carry versions,
asset names, key ids and outcome codes only. Failed checks retry on the next
interval with exponential backoff (up to eight intervals). The finisher logs to
`<runtime>/update.log` and `update-finish.log`.

Existing installations predate the updater: the first auto-updating release
must be installed by hand, and no release is installable until the
[release signing key](releasing.md#release-signing-key) exists.
`RHIZOME_UPDATE_SOURCE_URL`, `RHIZOME_UPDATE_TRUSTED_KEYS` and
`RHIZOME_UPDATE_INITIAL_DELAY` are test hooks for
`scripts/smoke_update.py`: the source must be a loopback URL and the other two
are honored only with it, so no environment can point a real installation at
a differently keyed mirror.

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
events; a pre-existing file becomes searchable through `search_sessions` and,
when its last recorded activity is inside `sessions.recent_window`, appears in
`list_sessions` as `recent`. It is never listed as live or observed until new
activity is actually seen. The catalog
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
python3 scripts/smoke_update.py
```

Go tests exercise actual filesystem notifications with no polling fallback,
missing roots/new nested directories, both ownership orders, partial lines,
rotation, filtering, duplicate direct rejection, passive delivery rejection, and
catalog indexing without publishing. The service smoke uses separate
observer/MCP processes and synthetic logs to verify subscription handoffs,
direct delivery, daemon restart, no duplicate replay, pre-existing logs becoming
searchable and readable, a recently ended session listed as `recent` and refused
by `send`, and a stale catalog with withdrawn recent rows while the service is stopped.
The update smoke runs a real observer against a loopback fake release: a
tampered manifest signature is refused and a disabled loop makes no request
everywhere; on Linux a signed release is installed, the process exits for its
supervisor, and the restarted observer confirms the new version; on macOS the
locally built, unsigned candidate is refused by `codesign` policy. No models or real user logs are involved. Service-definition tests validate
XML and arguments; they do not prove installation/startup on another operating
system. Native install/login/recovery tests must be recorded per platform.

## Terminal interface

On usable terminal stdin/stdout (`TERM` must not be `dumb`), bare
`rhizome service` opens Bubble Tea management. It shows whether the native
definition exists, whether the observer holds its runtime lock, available
observer health counters, and the [automatic update](#automatic-updates) state. The last health snapshot may be stale when stopped;
this view does not infer native manager state from formatted command output.
Use `RHIZOME_CONFIG=/absolute/config.toml rhizome service` for a custom config.

Keys: `i` install, `s` start, `x` stop, `u` uninstall, `r` refresh, `q`/Esc/Ctrl+C
exit. Stop and uninstall require `y` confirmation (`n`/Esc declines). Actions
reuse existing lifecycle rules, including install override restrictions. Only
one request runs at a time; status refreshes after actions. Requests are bounded
and cancelled on exit; completed effects are not rolled back. Errors stay visible.
Arrows and PgUp/PgDn scroll; resizing and no-color terminals are supported.

`rhizome service status --tui [--config /absolute/config.toml]` opens the same
status view without lifecycle actions. Explicit subcommands remain one-shot;
`service run` never launches a UI. Bare noninteractive `service` remains a usage
error. An explicit `--tui` requires usable terminals and otherwise fails promptly.
