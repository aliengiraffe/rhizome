# OpenCode setup and tested limitations

Tested against OpenCode **2.0.32** (2.x plugin API only; 1.x plugins do not load
in 2.x). Design: [docs/superpowers/specs/2026-10-19-opencode-support-design.md](superpowers/specs/2026-10-19-opencode-support-design.md).

OpenCode sessions are Rhizome peers through `rhizome mcp --agent opencode` plus a
small embedded relay plugin. The plugin connects to a private Unix socket owned by
the Rhizome MCP process. The plugin relays allowlisted events and executes
deliveries; all filtering and routing decisions are made in Go.

## Install

1. `task build`.
2. `./rhizome setup --agents opencode` installs the collaboration skill, writes
   the embedded relay plugin to `<config>/opencode/plugins/rhizome.js` and the
   display-only TUI plugin to `<config>/opencode/plugins/rhizome-tui/tui.js`
   (each reported as installed, updated or unchanged; an existing symlink is left
   alone). OpenCode discovers both from that directory; no configuration entry is
   needed. The TUI watches that directory; restart an already open TUI if peer
   toasts do not appear.
3. Merge [`examples/opencode/opencode.json`](../examples/opencode/opencode.json)
   into `~/.config/opencode/opencode.json`, using an absolute binary path. The
   server key must stay `rhizome`: Rhizome identifies its own tools by the
   `rhizome_` prefix. The MCP `command` must start the rhizome binary directly
   (no `sh -c`, `npx` or other wrapper), because the plugin pairs with the
   Rhizome process by OpenCode's process ID. Rhizome never edits OpenCode config.
4. Restart the OpenCode service: `opencode service restart`.
5. The plugin locates the socket only through `RHIZOME_RUNTIME_DIR` in the
   OpenCode service environment or the default `~/.rhizome/run`. If
   `RHIZOME_RUNTIME_DIR` is set for the MCP entry, or `runtime_dir` is set in
   `~/.rhizome/config.toml`, set the identical `RHIZOME_RUNTIME_DIR` in the
   OpenCode service environment, or the plugin and Rhizome will not find the same
   socket (Rhizome logs `opencode_runtime_from_config` when the directory comes
   from the file).

`rhizome mcp --agent opencode` resolves configuration like the other agents:
`runtime_dir`, `hostname` and the `[network]` table with flag > env > file >
default precedence, so `--lan`, `--tailscale` and their `RHIZOME_*` variables
apply to OpenCode session nodes. `rhizome setup` lists OpenCode in its picker
and `--agents` flag; it installs only the skill and plugins and never writes
OpenCode configuration. Which agent a process serves stays the per-launch
`--agent` flag.

OpenCode mode exposes twelve tools: the thirteen of Claude and Codex mode minus
`observe_session`. Every tool resolves the caller from `_meta`, including the
file-transfer tools (`send_file`, `list_files`, `accept_file`, `cancel_file`;
paired hosts only, see [file transfers](files.md)) and the read-only history
tools (`search_sessions`, `read_session_history`), which serve the local Claude
and Codex catalog, or with `host_id` an opted-in paired host's (see
[paired-host history](history.md#paired-host-history-opt-in)): OpenCode sessions
are not indexed by the history catalog or observed by the background service,
so they are never shared and there is no `[opencode.peer_history]` key. Because every tool needs a bound caller,
the history tools return `CALLER_UNVERIFIED` (`UNBOUND`) until the session's
first human prompt, unlike Codex mode.

## Identity and binding

- `_meta["ai.opencode/sessionID"]` on each MCP `tools/call` is the only caller
  identity. OpenCode sets it from the calling session, separately from
  model-written arguments. Missing or malformed values fail closed.
- A top-level session binds on its first human prompt, through the plugin. Child
  (subagent) sessions are refused. Synthetic inputs never bind.
- OpenCode runs one plugin instance and one Rhizome MCP process per project
  directory inside one service, and delivers every session event to every
  instance. The plugin therefore binds only sessions whose
  `ctx.session.get(...).location.directory` matches its own directory, and
  forwards frames only for sessions it bound.
- OpenCode periodically disposes and rebuilds a location instance (cause inside
  OpenCode unconfirmed; `opencode reload` forces it): the plugin is set up afresh
  and a new Rhizome process starts. So that idle sessions do not vanish, Rhizome
  persists the bound top-level session IDs with their last-activity time in
  `<runtime>/opencode-bindings/opencode-<parent pid>-<hash>.json` (mode 0600;
  keyed like the socket by the OpenCode process ID, which a reload keeps, and
  the first 32 hex characters of SHA-256 of the resolved directory; merged under
  a flock with atomic rename, at most 64 entries, entries idle over 24 h
  dropped). Another OpenCode process in the same directory never restores these
  sessions. Files of OpenCode processes that no longer exist are removed at
  startup.
  After every `hello` Rhizome sends `{"type":"restore","sessions":[...]}`; the
  plugin verifies each ID exactly like a prompt bind and answers with an ordinary
  `bind`, or `{"type":"unrestorable","sessionID":...}` when it is missing, a
  child or another location's; that removes the entry unless the session is
  currently bound in this process. Only sessions a human
  prompt bound ever enter the store; `session.deleted` and idle eviction remove
  them, a shutdown keeps them. A restored session starts like a fresh bind
  (human turn state) and keeps its stored activity time.
- Each bound session holds the per-session collection lease (see
  [service.md](service.md)) for its lifetime, so a second Rhizome process cannot
  collect the same session. During an instance reload the predecessor retires
  its sessions (keeping store entries) as soon as its plugin is gone and a
  successor owns the socket path (checked every 500 ms for 10 s after the
  disconnect, then every 5 s until a reconnect or exit); the successor retries a
  bind that finds the lease busy with doubling delays for 10 s, then every 5 s up
  to 60 s, then fails closed and keeps the store entry for the next prompt or
  reload.
- There is no `observe_session` in OpenCode mode; never invent an ID.
- Without the plugin (or before the first prompt) tools return `UNBOUND` with a
  setup hint.
- The OpenCode session title is the native-title source in the session-name
  precedence (see the README); `session.renamed` updates it.

## Collection

Published: assistant text, tool lifecycle from the `execute` tool hooks (Code Mode
inner tools by their real name; the `execute` wrapper and `rhizome_*` tools are
excluded as `Tool started|completed|failed: <name>`), turn completion, and titles
as native session names. Turns started only by Rhizome deliveries are suppressed
until a human prompt arrives. Inputs are applied when OpenCode delivers them into
the session's agent loop (`session.inbox.delivered`), not when they are queued
(`session.inbox.enqueued`), so a queued item never flips suppression mid-turn.
Never forwarded: reasoning, prompts, or tool outputs.
Text and tool-call IDs are deduplicated in 4,096-entry windows; existing rate
limits and the circuit breaker apply unchanged.

## Delivery

Events are delivered with `ctx.session.synthetic` and `delivery: "queue"`. This
wakes an idle session but never interrupts a running turn. Deliveries use the same
untrusted-peer envelope as Codex and are never republished.

- No connected plugin: nothing is written and events are retained in the bounded
  queue.
- At most one delivery is in flight per session. The plugin replies `ack` (counted
  as written) or `fail` (counted as dropped with the reason).
- A delivery written but not acknowledged within 5 s, or whose connection
  dropped, counts as dropped ("acceptance unknown; not retried") to avoid
  duplicate peer content.

### Peer messages in the TUI

OpenCode's TUI hides synthetic input, so the model sees a peer delivery but the
human does not. Delivery stays synthetic: sending peer content as a normal prompt
would present untrusted text as a human instruction and would count as a human
prompt for binding and peer-turn suppression. Instead the TUI plugin
(`rhizome-tui/tui.js`, OpenCode's V2 TUI plugin API: default export `{id, setup}`,
loaded from `<config>/opencode/plugins/<name>/tui.js`) shows an `info` toast when
a synthetic inbox item carrying Rhizome's peer envelope is enqueued
(`session.inbox.enqueued` via `ctx.data.on`) for a session whose directory is the
TUI's directory:

- Title: `Rhizome · <kind> from <first 8 characters of the sender session_id>`
  (only envelope `origin` fields; the envelope carries no agent type).
- Message: the envelope `content` without its leading `[<kind>]` tag, as plain
  text: control and bidirectional/zero-width formatting characters removed,
  whitespace collapsed, truncated to 200 characters with an ellipsis.
- When the session is not the one on screen, OpenCode adds an **Open** action.

The TUI plugin only displays. It makes no Rhizome decision, opens no socket or
file, sends nothing, logs nothing, shows each inbox item once (256-entry window)
and catches every error per event. Unknown sessions, other directories,
malformed envelopes and envelopes over 64 KiB show nothing.

## Lifecycle and limits

- Socket: `<runtime>/opencode-<parent pid>-<hash>.sock`, mode 0600, where `<hash>`
  is the first 12 hex characters of SHA-256 of the symlink-resolved working
  directory. The pid and directory only pair a plugin with its MCP process; they
  never route. The full path must be at most 103 bytes (use a shorter
  `RHIZOME_RUNTIME_DIR` otherwise). A socket file at the exact path is replaced
  at startup (during a reload the successor takes over the path); shutdown
  removes it only if it is still the process's own socket.
- One plugin connection at a time. Replacing a ready connection reports a
  disconnect then reconnect. On every (re)connect the plugin re-sends the bind
  of each session it has bound, right after `hello`, so a restarted Rhizome
  recovers its bindings. While disconnected, nodes stay bound, tools work,
  `collection_state` is `plugin disconnected` and delivery pauses. Every
  reconnect, and any plugin buffer overflow, produces an "OpenCode activity
  discontinuity" gap notice.
- Limits (additions only; existing retention, queue, dedup and rate limits are
  unchanged):

  | Limit | Value |
  | --- | --- |
  | Frame size | 64 KiB; any oversized frame disconnects |
  | Malformed frames | dropped and counted; more than 16 per connection disconnects |
  | Text | 16 KiB in the plugin (Go clips further); serialized text frames bounded to 60 KiB |
  | Other string fields | 1 KiB |
  | Plugin buffer | 256 frames while disconnected; drop-oldest plus an overflow marker that becomes a gap |
  | Plugin queued inputs awaiting delivery | 256 inbox items; oldest evicted |
  | Manager frame queue | 512 frames, one worker, order preserved; overflow drops frames, counts them and records a gap |
  | Delivery acknowledgement | 5 s |
  | Bound sessions per process | 64 (further binds are refused) |
  | Idle eviction | 24 h without binds, activity or tool calls |

- `session.deleted` makes the plugin send `forget`, which retires the node (it is
  idempotent; events can arrive more than once).
- A Rhizome restart loses state (subscriptions, replay, dedup, cursors). Sessions
  rebind on their next human prompt, or through the restore list when the plugin
  (re)connects.

## Why a closed session can still be `live`

`live` in `list_sessions` means a Rhizome node for that exact `session_id` is bound
and reachable. It does not mean a window is open, a model turn is running or
anyone is using the session, and Rhizome reports no idle time (the identity
carries no activity timestamp, and adding one would be a wire change).

Closing the OpenCode UI does not end an OpenCode session. The sessions live in
the OpenCode process (for example `opencode serve --service`), whose plugin
keeps the session's Rhizome node bound. The node goes away when:

- the session is deleted in OpenCode (`session.deleted` retires the node);
- 24 h pass without binds, activity or tool calls (idle eviction);
- the OpenCode process exits (a Rhizome restart alone can be undone by the
  restore list on the plugin's next connect, which is intended).

To clear a session you no longer use, delete it in OpenCode or stop the OpenCode
service. To check whether it is actually in use, look at its recorded activity
rather than its `live` status. OpenCode sessions are not in the history catalog
(see [history.md](history.md)), so ask the owner, or use the process list on the
host. Codex behaves the same way: its app-server daemon can keep a thread's node
alive after the terminal closes (see [codex.md](codex.md)).

## Trust boundary

The socket trusts processes running as the same user, like the cluster token. This
is documented, not hardened against same-user malware. The plugin's only write is
`ctx.session.synthetic` for delivery. Logs carry counters, types and lengths, never
content or names.

## Verification

- `task test-plugin` runs `node --test plugins/opencode/*.test.mjs` (Node 20+;
  skipped without `node`).
- `python3 scripts/smoke_stdio.py` (and `--lan`) covers the OpenCode bridge with
  a fake plugin: identity, binding, activity to a Claude peer, delivery and
  restore after a reload; with `--lan` the OpenCode process has its own runtime
  directory and joins the Claude peer over pinned TLS.
- Opt-in live smoke:
  `python3 scripts/smoke_opencode.py --live --model <provider/model>`. It also
  forces an instance reload (`opencode reload --server`, disposable server only)
  and checks restore and delivery without a new prompt. It runs
  real model turns against a standalone `opencode serve` with isolated
  config/state/cache, but uses your real OpenCode data directory for
  credentials. Its temp repository gets an initial commit, and it deletes only
  its own sessions (recorded IDs or the exact temp-project directory). Pass a
  model you have credentials for; free OpenCode models reject scripted use.
  `RZOC_KEEP=1` keeps the temp directory (it contains the server password; delete
  it), and `RZOC_T` sets the turn timeout.
- Tested version: 2.0.32. Interactive TUI parity is a manual checklist item in
  [verification.md](verification.md).

Known CLI caveat: from a non-interactive parent, run `opencode run` with stdin
closed (`</dev/null`); an open stdin was a confirmed cause of hangs. Hangs were
also seen when message text contained `ses_...` IDs; that cause is unconfirmed.
The plugin `ctx.session.synthetic` API handled such text fine.
