# OpenCode support design

Date: 2026-10-19. Status: approved design, awaiting spec review. Target: OpenCode
**2.0.32** (2.x plugin API only; 1.x plugins do not load in 2.x).

## Goal

Make local OpenCode sessions full Rhizome peers alongside Claude Code and Codex:
bound by verified identity, publishing selected activity, and receiving peer
events by push. Existing Claude and Codex behavior, limits, and invariants are
unchanged.

## Decisions

| Topic | Decision |
| --- | --- |
| Delivery | Wake the session. Deliver as `ctx.session.synthetic({ delivery: "queue" })`. A context-only (no wake) mode via the `context` hook is a possible follow-up, not part of this work. |
| Installation | `rhizome setup --agents opencode` installs the skill and the embedded plugin. Users add the MCP entry by hand from `examples/opencode/`. Rhizome never edits OpenCode config. |
| Which sessions join | Automatic: a top-level session binds on its first human prompt, like Claude. Nodes are dropped on session deletion or after 24 hours idle. |
| Architecture | A thin OpenCode plugin plus a private Unix socket to the Rhizome MCP process. Not chosen: a Go-only client of OpenCode's HTTP API, which needs the server password and uses unverified endpoints. |

## Spike evidence (OpenCode 2.0.32, 2026-10-19)

- MCP `tools/call` carries `_meta["ai.opencode/sessionID"]`. OpenCode's MCP client
  sets it from the calling session, separately from model-written arguments:
  `g.callTool({name, arguments: x.args ?? {}, ...x.sessionID === void 0 ? {} :
  {_meta: {"ai.opencode/sessionID": x.sessionID}}})`. Forged arguments passed
  through unchanged, but `_meta` always held the real ID.
- MCP tools are reached through Code Mode (`execute` running
  `tools.<server>.<tool>(...)`). `ctx.tool.hook("execute.before"/"execute.after")`
  fires for the inner MCP call with its real name (for example `spike_echo`).
- `ctx.event.subscribe()` yields typed events keyed by `sessionID`, including
  `session.inbox.enqueued` (`item.type` `user` or `synthetic`),
  `session.text.ended`, `session.step.ended`, `session.execution.succeeded`, and
  `session.renamed`.
- `ctx.session.synthetic` and `ctx.session.prompt` wake an idle session with
  either `delivery: "steer"` or `"queue"`. The model sees synthetic content.
  Text containing `ses_…` IDs is fine through this API. Only the CLI message
  argument hangs on such text.
- Plugins load from `<config>/opencode/plugins/*.js` as
  `export default { id, setup(ctx) }`. `opencode serve` requires a password.
- One MCP process served several sessions. The plugin and MCP server appeared to
  be instantiated per location (project directory) within the service process.

## Components

### Go (`internal/channel/`)

- `opencode.go`: `RunOpenCode()` and an `opencodeNodes` manager modeled on
  `codexNodes`.
  - It resolves the caller only from `_meta["ai.opencode/sessionID"]`. A missing,
    non-string, or malformed value fails closed.
  - It keeps one node per bound session, up to 64. Bind requests beyond that are
    refused with a clear error.
  - Nodes are retired on `forget` or after 24 hours without binds, activity, or
    tool calls.
  - Each node's `notify` sends a `deliver` request over the bridge.
- `opencode_bridge.go`: the Unix socket server, newline-delimited JSON framing,
  frame limits, a single active plugin connection, and acknowledgement tracking.
- `opencode_collect.go`: turns plugin events into Rhizome activity. **All
  filtering lives here**, not in the plugin.

Changes to existing code:

- `main.go` accepts `--agent opencode`.
- `mcp.go`: `makeServer(resolve, claude bool, …)` becomes an agent kind. OpenCode
  mode registers no `observe_session` tool.
- `setup.go` adds the `opencode` agent.
  - It is detected via `$XDG_CONFIG_HOME/opencode`, `~/.config/opencode`, or
    `opencode` on `PATH`.
  - Setup installs the skill and writes the embedded plugin to
    `<config>/opencode/plugins/rhizome.js`, reporting "installed", "updated", or
    "unchanged". An existing symlink is skipped.
- `name.go`: the OpenCode session title (from `bind` and `session.renamed`) is the
  native-title source in the existing name precedence.

### Plugin (`plugins/opencode/rhizome.js`, embedded with `go:embed`)

- Dependency-free JavaScript in the 2.x format:
  `export default { id: "rhizome", setup(ctx) }`.
- Connects to the socket path below, reconnecting with backoff from 250 ms up to
  15 s. With no connection, its handlers do nothing and never block OpenCode.
- Forwards a fixed allowlist of events with only the fields Go needs. It never
  forwards tool outputs or reasoning, and truncates assistant text to 16 KiB.
- Executes `deliver` requests with `ctx.session.synthetic` and replies with `ack`
  or with `fail` and a reason.
- Makes no filtering or routing decisions.

### Docs and examples

- `examples/opencode/opencode.json` holds the MCP entry, with an absolute binary
  path.
- `docs/opencode.md` covers setup, the tested version, the data flow, and
  limitations.
- README, AGENTS.md (map, flag, invariants), and `docs/verification.md` are
  updated in the same change.

## Bridge protocol

The socket is `<runtime>/opencode-<parent pid>-<hash>.sock`:

- `<runtime>` is the existing private Rhizome runtime directory.
- `<parent pid>` is the OpenCode process. The plugin uses `process.pid`.
- `<hash>` is the first 12 hex characters of SHA-256 of the absolute working
  directory. Rhizome uses its `cwd` and the plugin uses `ctx.location.directory`.
  Both are normalized with symlinks resolved.

The pid and directory are pairing keys only, never routing keys.

Frames are UTF-8 JSON objects, one per line, at most 64 KiB, each with a `type`
field.

Plugin to Rhizome:

| `type` | Fields | Meaning |
| --- | --- | --- |
| `hello` | `version`, `opencode` | Plugin protocol version and OpenCode version. A protocol mismatch closes the connection. |
| `bind` | `sessionID`, `parentID`, `directory`, `title` | A human prompt arrived. |
| `title` | `sessionID`, `title` | `session.renamed`. |
| `input` | `sessionID`, `kind` (`user` or `synthetic`), `rhizome` (bool) | An inbox item was enqueued. `rhizome` is true when the text holds a `<rhizome-peer-event>` envelope. The text itself is never sent. |
| `text` | `sessionID`, `messageID`, `text` | `session.text.ended`, assistant text. |
| `tool` | `sessionID`, `callID`, `name`, `phase` (`start`, `completed`, or `failed`) | From the `execute.before`/`after` tool hooks. |
| `turn` | `sessionID` | `session.execution.succeeded`. |
| `forget` | `sessionID` | `session.deleted`; the event name is still to be confirmed. |
| `overflow` | `dropped` | The plugin buffer dropped frames. |
| `ack` / `fail` | `id`, `reason?` | Delivery result. |

Rhizome to plugin: `deliver {id, sessionID, text}`.

## Data flow

1. **Binding.**
   - A `bind` with an empty `parentID` creates the session's node, or refreshes
     its metadata if it exists (project, display name, native title).
   - A non-empty `parentID` is refused and counted.
   - Synthetic inputs never bind.
   - The plugin relays only sessions whose `ctx.session.get` location directory matches its own instance's directory; OpenCode delivers every session event to every location's plugin instance, so unscoped relaying would bind one session in two Rhizome processes.
2. **Tool calls.**
   - The node comes from `_meta` only.
   - No node for the session returns `UNBOUND`, with the hint to install the
     plugin with `rhizome setup --agents opencode` and submit a prompt.
3. **Activity.**
   - `text` is published as `activity` and `tool` as
     `Tool started|completed|failed: <name>`. `rhizome_*` tools and the `execute`
     wrapper are excluded.
   - `turn` is published as `turn`; it is kept in history but does not wake
     subscribers.
   - **Peer-turn suppression:**
     - When a turn's inputs are all `rhizome` synthetic items, its text and tools
       are not published.
     - A `user` input resets suppression. This mirrors `peerResponse` in
       `transcript.go`.
   - Text and tool-call IDs are deduplicated within 4,096-entry windows.
4. **Delivery.**
   - `notify` sends `deliver`, using the Codex untrusted-peer envelope (a header
     line plus `<rhizome-peer-event>{json}</rhizome-peer-event>`).
   - `ack` counts as written.
   - `fail` counts as dropped and records the delivery error.
   - No connected plugin returns an `errDestinationUnavailable` equivalent and
     the events are retained in the existing bounded queue. A delivery that was
     written but not acknowledged within 5 s (or whose connection dropped) is
     counted as dropped, with "acceptance unknown; not retried", as for Codex
     queue failures. This prevents duplicate peer content.
   - At most one delivery is in flight per session.
5. **Retirement.**
   - `forget` or 24 hours idle closes the node through the existing close path:
     streams, registration file, and goroutines.

## Lifecycle and failure handling

- **Startup.** Create the socket with mode 0600, removing only a stale file at
  the exact path. Shutdown (stdin EOF or a signal) closes all nodes and removes
  the socket and registrations.
- **One connection.** A new plugin connection replaces the old one, which covers
  plugin reload on config change.
- **Plugin disconnected.**
  - Nodes stay bound and tools keep working.
  - `collection_state` becomes `plugin disconnected` and delivery pauses with the
    queue retained.
  - On reconnect, subscribers receive a gap notice ("OpenCode activity
    discontinuity").
- **Rhizome restart.** In-memory state is lost, as already documented. Sessions
  rebind on their next human prompt, and existing subscribers see the usual
  restart gap.
- **New limits.**

  | Limit | Value |
  | --- | --- |
  | Frame size | 64 KiB |
  | Plugin text truncation | 16 KiB; Go still clips to 7 KiB |
  | Plugin buffer | 256 frames, drop-oldest plus an `overflow` marker that becomes a gap |
  | Delivery acknowledgement timeout | 5 s |
  | Nodes per process | 64 |
  | Idle eviction | 24 h |

  Malformed frames are dropped and counted; more than 16 per connection, or any
  oversized frame, closes the connection. No existing retention, queue, dedup, rate, or circuit-breaker limit
  changes.

## Security and invariants

- **Stdout is MCP only.** Bridge traffic uses the socket, and logs go to stderr.
- **Exact session ID routing.**
  - Node keys are OpenCode session IDs.
  - Titles, directories, and pids are labels or pairing keys only.
- **No model-controlled identity.**
  - There is no `observe_session` tool in OpenCode mode.
  - `_meta` is set by OpenCode.
  - The socket is private to the user.
- **Child sessions never bind.**
- **No content or names in logs.** The bridge logs counters, types, and lengths.
- **Feedback controls unchanged.**
  - Peer-turn suppression, `rhizome_*` exclusion, and no republishing of received
    envelopes all apply.
  - The 10/min explicit limit and 30/min breaker are unchanged.
  - Deliveries are `synthetic`, so they never bind or reset suppression.
- **Read-only collection.** The plugin only reads events. Its only write is
  `ctx.session.synthetic` for delivery: no other turns, session mutation,
  permission answers, or config edits.
- **Trust boundary.** Like the cluster token, the socket trusts processes running
  as the same user. This is documented, not hardened against same-user malware.

## Testing

**Go tests.** Table-driven and race-clean, synchronized with explicit signals and
deadlines rather than sleeps. A fake plugin talks to the real socket. Cases:

- Caller resolution.
- Binding, including child refusal and the 64-node cap.
- Native titles.
- Collection filtering and peer-turn suppression.
- Overflow gaps and text truncation.
- Frame limits and connection replacement.
- Reconnect gaps.
- Delivery `ack`, `fail`, timeout, and retain.
- `forget` and idle eviction with an injected clock.
- Shutdown cleanup.

**Plugin tests.** `node --test` with a fake `ctx`, run by an optional
`task test-plugin` that is skipped when Node is absent. Node is a test-only
dependency.

**Stdio smoke (CI, no model).** Extend `scripts/smoke_stdio.py` to run
`rhizome mcp --agent opencode` with a fake plugin client. It covers the handshake,
`_meta` identity, binding, activity to a Claude peer, and delivery back.

**Live smoke (opt-in).** `scripts/smoke_opencode.py --live --model <provider/model>`:

- Isolated config, with all non-Rhizome tools denied.
- `serve --standalone`, never the shared service or its port.
- Two disposable sessions plus a Claude protocol client.
- No `ses_…` IDs in CLI message arguments.
- Sessions deleted afterwards.

## Verify first

Results (2026-10-19, OpenCode 2.0.32, `opencode serve` plus the V2 spike plugin):

1. Deletion event: `session.deleted` with `data: {sessionID}` (plus a
   `durable` block with `aggregateID`, `seq`, `version`; no `location`). Deleting
   a parent also emits one for each child session, children first. With two
   locations loaded in one server, each event reached the plugin subscriber
   twice, so `forget` must be idempotent.
2. One MCP process and one plugin instance per location: the server spawned a
   separate MCP process (distinct `pid`, `cwd` equal to the location directory)
   and ran `setup` once per location directory (each with its own trigger
   port). The `init` `location` has `directory` and `project`
   (`id`, `directory`, `canonical`). A plain `git init` directory with no commits
   reports project id `global`, the same as a non-git directory, so route and
   pair by `directory`, not `project.id`.
3. Subagent MCP calls: `_meta["ai.opencode/sessionID"]` carries the child
   session ID, which differs from the parent's. `ctx.session.get` on the child
   returns `parentID` (the parent session ID), `agent` (`general`) and `title`;
   top-level sessions have no `parentID` and no `agent` field. The plugin hook
   `tool.before` also reports the child `sessionID`. Models reach MCP tools
   through an `execute` sandbox, which fires `tool.before` for `execute` and
   then for `spike_echo`.
4. Interactive TUI parity with `run`/`serve`: not verified here; it is a manual
   check in Task 9.

## Out of scope

- OpenCode 1.x.
- Context-only delivery.
- Automatic MCP config editing.
- LAN or Tailscale.
- Persisting nodes across restarts.
- Cursor, Pi, Hermes, and OpenClaw.

## Completion criteria

- `task build`, `task check`, `python3 scripts/smoke_stdio.py`, and
  `git diff --check` pass.
- One recorded live smoke run on OpenCode 2.0.32.
- AGENTS.md, README, `docs/opencode.md`, and `docs/verification.md` are updated in
  the same change.
