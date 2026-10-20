# OpenCode setup and tested limitations

Tested against OpenCode **2.0.32** (2.x plugin API only; 1.x plugins do not load
in 2.x). Design: [docs/superpowers/specs/2026-10-19-opencode-support-design.md](superpowers/specs/2026-10-19-opencode-support-design.md).

OpenCode sessions are Rhizome peers through `rhizome mcp --agent opencode` plus a
small embedded relay plugin. The plugin connects to a private Unix socket owned by
the Rhizome MCP process. The plugin relays allowlisted events and executes
deliveries; all filtering and routing decisions are made in Go.

## Install

1. `task build`.
2. `./rhizome setup --agents opencode` installs the collaboration skill and writes
   the embedded plugin to `<config>/opencode/plugins/rhizome.js` (reported as
   installed, updated or unchanged; an existing symlink is left alone).
3. Merge [`examples/opencode/opencode.json`](../examples/opencode/opencode.json)
   into `~/.config/opencode/opencode.json`, using an absolute binary path. The
   server key must stay `rhizome`: Rhizome identifies its own tools by the
   `rhizome_` prefix. Rhizome never edits OpenCode config.
4. Restart the OpenCode service: `opencode service restart`.
5. If `RHIZOME_RUNTIME_DIR` is set, it must be identical in the OpenCode service
   environment and for the MCP entry, or the plugin and Rhizome will not find the
   same socket.

OpenCode mode exposes six tools: the usual seven minus `observe_session`.

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
until a human prompt arrives. Never forwarded: reasoning, prompts, or tool outputs.
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

## Lifecycle and limits

- Socket: `<runtime>/opencode-<parent pid>-<hash>.sock`, mode 0600, where `<hash>`
  is the first 12 hex characters of SHA-256 of the symlink-resolved working
  directory. The pid and directory only pair a plugin with its MCP process; they
  never route. The full path must be at most 103 bytes (use a shorter
  `RHIZOME_RUNTIME_DIR` otherwise). Only a stale socket at the exact path is
  removed at startup; shutdown removes it.
- One plugin connection at a time. Replacing a ready connection reports a
  disconnect then reconnect. While disconnected, nodes stay bound, tools work,
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
  | Manager frame queue | 512 frames, one worker, order preserved; overflow drops frames, counts them and records a gap |
  | Delivery acknowledgement | 5 s |
  | Bound sessions per process | 64 (further binds are refused) |
  | Idle eviction | 24 h without binds, activity or tool calls |

- `session.deleted` makes the plugin send `forget`, which retires the node (it is
  idempotent; events can arrive more than once).
- A Rhizome restart loses state (subscriptions, replay, dedup, cursors). Sessions
  rebind on their next human prompt.

## Trust boundary

The socket trusts processes running as the same user, like the cluster token. This
is documented, not hardened against same-user malware. The plugin's only write is
`ctx.session.synthetic` for delivery. Logs carry counters, types and lengths, never
content or names.

## Verification

- `task test-plugin` runs `node --test plugins/opencode/*.test.mjs` (Node 20+;
  skipped without `node`).
- `python3 scripts/smoke_stdio.py` covers the OpenCode bridge with a fake plugin:
  identity, binding, activity to a Claude peer and delivery.
- Opt-in live smoke:
  `python3 scripts/smoke_opencode.py --live --model <provider/model>`. It runs
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
