# Pi support: research and design sketch

Status: **sketch, not implemented.** Nothing here is verified against a live Pi
session yet. Items marked *(unverified)* must be checked before coding.

Researched against `@earendil-works/pi-coding-agent` **1.4.0** (npm `latest`,
modified 2026-10-21; repo `earendil-works/pi`, source read at `6eb99ad`). The
older `@mariozechner/pi-coding-agent` package is deprecated; `0.74.2` remains
under the `legacy-node20` tag. Pi 1.x changed enough (built-in MCP, session
format v3) that this sketch targets 1.x only.

## What Pi is (relevant facts)

| Area | Fact | Source doc (`packages/coding-agent/docs/`) |
| --- | --- | --- |
| Process model | One in-process agent per `pi` process; `/new`, `/resume`, `/fork` swap the session inside the same process. | `how-pi-works.md`, `extensions.md` (`session_start.reason`) |
| Sessions | JSONL, tree (`id`/`parentId`), header line has `id`, `cwd`, optional `parentSession`. Path `~/.pi/agent/sessions/--<cwd-with-dashes>--/<ts>_<session-id>.jsonl`. Overrides: `--session-dir`, `PI_CODING_AGENT_SESSION_DIR`, `sessionDir` setting; config root `PI_CODING_AGENT_DIR`. Format version 3. | `session-format.md`, `sessions.md`, `settings.md` |
| Titles | `session_info` entry (`name`), set by `/name`, RPC or `pi.setSessionName()`. | `session-format.md` |
| Message entries | `message` entries carrying `user`, `assistant`, `toolResult`, `system` roles; plus `custom_message`, `compaction`, `model_change`, `label`, `usage`. | `session-format.md`, `message-types.md` |
| Extensions | TypeScript/JS modules loaded **in-process**; auto-discovered from `~/.pi/agent/extensions/*.js` or `*/index.js`, or `packages`/`extensions` in `settings.json`. Project-local ones load only after project trust. | `extensions.md` |
| Extension events | `session_start` (`startup/reload/new/resume/fork`), `session_shutdown`, `session_info_changed`, `before_agent_start`, `agent_start/end/settled`, `turn_*`, `message_start/update/end`, `tool_execution_*`, `input` (source `interactive/rpc/extension`). | `extensions.md` |
| Injecting input | `pi.sendMessage(customMessage, {deliverAs: steer\|followUp\|nextTurn, triggerTurn})` adds a *custom* (non-user) message that participates in context. `pi.sendUserMessage` impersonates the user. | `extensions.md` |
| MCP | Built-in client (stdio and streamable HTTP; no SSE). Servers from `~/.pi/agent/mcp.json`, `.pi/mcp.json`, or `pi.registerMcpServer(name, config)` per session (`command/args/env/cwd`). Tools are named `mcp__<server>__<tool>`. | `mcp.md`, `extensions.md` |
| MCP identity | The client sends **no session identity** in `_meta` (only `progressToken`). Pi sets nothing like `ai.opencode/sessionID`. | `packages/mcp/src/client.ts` |
| Other modes | `--mode rpc` (JSONL over stdio, `rpc-commands.md`), JSON event stream, TypeScript SDK (`createAgentSession`). | `rpc-commands.md`, `json.md`, `sdk.md` |
| Skills | Agent Skills layout, `~/.pi/agent/skills/` and `~/.agents/skills/`. | `skills.md` |
| Hooks | No separate hook system; extensions are the only in-process hook surface. | `how-pi-works.md` |

## Design constraints from AGENTS.md

- Identity must be verified, never self-declared; Rhizome never invents a
  session ID or calls `observe_session` for Pi.
- Route only by exact `session_id`; Pi deliveries must be synthetic, never
  prompts, and must not feed back into collection (`session.inbox.delivered`).
- Collection and history stay read-only and local; no transcript content or
  session names in logs.
- Stdout is MCP only; limits (retention, queues, dedup) are not loosened.
- Setup merges only Rhizome-owned entries, backs up, and is reversed by
  `rhizome uninstall`.

## The identity problem

Claude identifies via hooks, Codex via per-thread nodes, OpenCode via
`_meta["ai.opencode/sessionID"]` plus PID pairing with the plugin. Pi offers
none of these directly: a static `mcp.json` entry would spawn a `rhizome mcp`
process that cannot tell which Pi session is calling, and one `pi` process can
host several sessions over its lifetime.

### Recommended: extension-registered MCP server plus bridge socket

Mirror the OpenCode shape (relay-only plugin, all decisions in Go), with one
change: bind through a per-session nonce instead of PID alone.

1. A small embedded Pi extension (`plugins/pi/rhizome.js`) runs on
   `session_start`. It reads `ctx.sessionManager.getSessionId()` and
   `getCwd()`, generates a 128-bit nonce, and calls
   `pi.registerMcpServer("rhizome", { command: <abs rhizome>, args: ["mcp",
   "--agent", "pi"], env: { RHIZOME_PI_NONCE: nonce, RHIZOME_RUNTIME_DIR? } })`.
   The server key must stay `rhizome` so Rhizome's own tools (`mcp__rhizome__*`)
   are recognizable.
2. The extension connects to a private Unix socket in the runtime directory
   (the OpenCode bridge's socket owner/permissions rules) and sends
   `bind {nonce, session_id, cwd, pid}`.
3. The `rhizome mcp --agent pi` process, spawned by Pi with that nonce in its
   environment, completes the bind only when the nonce matches a pending bind
   **and** the bridge peer's PID is the MCP process's parent (OS-verified). The
   session identity is then Pi-process-verified plus nonce-matched, not
   model- or prompt-supplied.
4. On `session_shutdown`/`session_start` with reason `new|resume|fork` the
   extension calls `pi.unregisterMcpServer("rhizome")`, then registers again with
   a fresh nonce for the new session. One MCP process maps to exactly one
   session, which keeps routing exact and lets stale identities fail closed.

Tradeoffs: the env nonce is readable by same-user processes, the same trust
boundary as the existing PID pairing; the nonce only removes the one
process/many sessions ambiguity. `registerMcpServer` is documented as
"registered later connect right away" so rebinding on `/resume` should work
*(unverified)*.

### Rejected alternatives

- **Static `~/.pi/agent/mcp.json` entry:** no session identity; would force a
  heuristic (cwd, newest file) that AGENTS.md forbids.
- **Binding from the JSONL log alone:** fine for history and observer
  collection, not for live routing, because it cannot prove which process owns
  the session.
- **RPC mode / SDK wrapper:** requires Rhizome to own the Pi process; users run
  interactive `pi`.

## Pieces

| Concern | Approach | Reuse |
| --- | --- | --- |
| Agent type | New `pi` agent in `rhizome mcp --agent` and setup picker. | `setup_agents.go` agent table |
| Install | Skill to `~/.agents/skills/` or `<PI_CODING_AGENT_DIR>/skills`; extension to `<PI_CODING_AGENT_DIR>/extensions/rhizome/index.js`. No MCP config entry (the extension registers it). No hooks. | OpenCode plugin install path (`agent.plugins()`), embedded `skills/` tree |
| Uninstall | Strip skill and extension via existing `skillNames`/`plugins()` lists; symlinked paths skipped. | `uninstall.go` |
| Live collection | Extension relays allowlisted events (`message_end`, `tool_execution_end`, `agent_settled`) for its own session to the bridge; Go does all filtering, same content policy as other agents. | `opencode_collect.go` normalizer shape |
| Delivery | Bridge sends `deliver` frames; the extension calls `pi.sendMessage({customType:"rhizome", display:true, content}, {deliverAs:"followUp"})`, with `triggerTurn:false` while streaming and `true` only when idle. Custom messages are non-user, matching "peer deliveries stay synthetic". Ack within 5 s, counted when delivered into the loop. | OpenCode bridge limits |
| Feedback loops | Extension never relays `customType:"rhizome"` messages back; Go also filters them. | `delivery.go` suppression |
| Titles | `session_info` entry → native title tier in the session-name precedence. | `name.go` |
| History | File source under `<pi sessions root>/--*--/*.jsonl` using watches, confinement and symlink refusal; normalize through `activitySink` from byte zero. Handles tree sessions by following the active branch (last entry's ancestry) *(open question: branches in the catalog)*. | `history*.go` Claude/Codex sources |
| Recent rows | Pi recorded timestamps feed `[sessions].recent_window` like Claude/Codex. | `recent.go` |
| Config | New `[pi]` table (`sessions_dir` default `~/.pi/agent/sessions`, honoring `PI_CODING_AGENT_DIR`/`PI_CODING_AGENT_SESSION_DIR`) and `[pi.peer_history]`; `--agent pi` stays a launch flag. | `internal/config/` |
| Gateway | Export `pi` sessions only over links that negotiate a new `rhizome.gateway.pi.v1` subprotocol; older peers drop links on unknown agent types. | `rhizome.gateway.opencode.v1` precedent |
| Tools | Same twelve tools as OpenCode mode (no `observe_session`); every call resolves the caller from the bound identity. | `opencode.go` |

## Open questions

1. ~~Does `registerMcpServer` on `session_start` connect before first use?~~
   Confirmed (spike). The double `session_start` is explained (see spike results);
   `fork` is still untested.
2. ~~Env/cwd passed to the stdio child?~~ Confirmed. Check the nonce cannot leak
   into pi logs or `/mcp` output.
3. ~~`getSessionId()` equals the JSONL id?~~ Confirmed.
4. ~~Does `sendMessage` wake an idle session and reach a streaming one?~~
   Confirmed (spike). Still open: which boundary is safe to call `sendMessage`
   from when triggered inside an event handler (re-entrancy).
5. Project trust: global extension dir loads without trust; confirm an untrusted
   project cannot shadow it with `.pi/extensions`.
6. Branching: should history index only the active branch, or all entries
   (labelled by branch)? The contract in `docs/history.md` assumes a linear log.
7. Pi's Rust port (`pi_agent_rust`) and its ACP mode are different programs;
   out of scope.

## Spike results (pi 1.4.0, isolated `PI_CODING_AGENT_DIR`, RPC mode)

A stub extension registered a stub stdio MCP server on `session_start` with a
per-session nonce in `env`, and unregistered on `session_shutdown`. Driven with
`get_state` then `new_session`. Scripts: scratchpad `spike/` (not committed).

- **Confirmed:** a server registered in `session_start` connects at startup;
  `env` and `cwd` reach the child; the child's parent PID is the `pi` process
  (OS-verifiable); `ctx.sessionManager.getSessionId()` equals the id in the
  JSONL filename and header; `unregisterMcpServer` closes the child (stdin EOF,
  SIGTERM on quit); re-registering on the next session spawns a fresh child
  with the new nonce. Pi sends no session identity in MCP `_meta`.
- **Double `session_start` (explained):** in RPC mode the `new_session`,
  `switch_session`, `fork` and `clone` commands call `runtimeHost.*()`, which
  already rebinds through the `setRebindSession` callback, and then call
  `rebindSession()` again (`src/modes/rpc/rpc-mode.ts`). Each rebind runs
  `bindExtensions()`, which emits `session_start`, so extensions see it twice
  with the same session id (reproduced for `new` and `resume`). Interactive
  and print modes register only the callback, so they should emit it once
  *(read from source, not run interactively)*. Consequence: the extension must
  be idempotent per session id (keep the nonce for an id already bound, or
  replace it and let the bridge accept only the latest), because the first
  registration's child may never spawn while the second's does. Worth an
  upstream issue, but Rhizome must not depend on a fix.
- **Delivery (second spike, stub model provider registered by the extension, so
  no credentials or network):** `pi.sendMessage({customType:"rhizome",...})`
  called from a timer in the extension, in RPC mode.
  - Idle, `triggerTurn: true`: starts a run immediately (`agent_start`, then a
    model call). Idle push works.
  - Idle, `triggerTurn: false`: stored with no turn; it joins the context of the
    next model call.
  - Streaming, `deliverAs: "steer"` or `"followUp"`: delivered when the current
    assistant message ends, inside the same run (one `agent_start`/`agent_end`,
    one extra model call), never as a new user prompt. With no `deliverAs` and
    `triggerTurn: true` it behaved like `steer` and raised no error.
  - The provider receives these custom messages with role `user`. Pi does not
    mark them as non-user for the model, so Rhizome must frame the text itself
    as untrusted peer activity, as the Claude channel does.
  - `agent_settled` reports `isIdle() === true`; `agent_end` and
    `agent_before_settle` report false.
  - Harness note: an undrained stdout pipe blocks pi (backpressure) and looks
    like a hang; the RPC consumer must drain stdout.
- **Not yet tested:** the `fork` reason, `sendMessage` called from inside an
  event handler (re-entrancy), interactive (non-RPC) mode, project-trust
  shadowing, a real provider, and delivery when the user is typing in the TUI.

## Parity goal

Target: the same experience as Claude Code today, with documented exceptions.

| Claude Code today | Pi equivalent | Parity |
| --- | --- | --- |
| Same MCP tools | Same tools minus `observe_session` (as OpenCode). Pi names them `mcp__rhizome__*`, so the bundled skill works unchanged. | Full |
| Hook binding (first prompt) | Extension binds at `session_start` via nonce and verified socket. | Full or better |
| Peer events pushed as they happen | `pi.sendMessage` (`triggerTurn` when idle, `steer`/`followUp` mid-run). *(unverified)* | Full |
| Live collection | Extension relay plus observer reading the JSONL files. | Full |
| Titles, `recent`, history, files, paired hosts | `session_info`, file source, `rhizome.gateway.pi.v1`. | Full |
| `setup` / `uninstall` | Skill plus extension; no MCP or hook entries. | Full |

Gaps that could cost parity:

1. **Tool exposure.** Pi can route MCP tools through codemode or tool search
   (`exposure`/`toolExposure`). The extension must register the server with
   `exposure: "direct"` (other values: `codemode`, `deferred`, `hidden`) so Rhizome's tools are first-class.
2. **Extension not loaded** (`--no-extensions`, `--no-mcp`): observer-only
   (`recent` metadata, no delivery), the same as Claude without hooks. Document it.
3. **Branched sessions:** needs a rule for which branch counts (see questions).
4. **Version drift:** pin a minimum Pi version in setup and the extension and
   fail closed on older ones (MCP and extension APIs changed between 0.99 and 1.4).

## Suggested phases

1. **Identity and delivery spike (first; everything depends on it).** Verify
   open questions 1-4 on a live `pi` 1.4.0 with a stub extension and stub MCP
   server; then extension + bridge handshake showing a `live` Pi session in
   `list_sessions`. Go/no-go for the design.
2. **Collection and delivery:** relay, `sendMessage` delivery, loop suppression,
   direct tool exposure, opt-in live smoke (`scripts/smoke_pi.py`).
3. **History:** Pi file source, titles, recent rows, `search_sessions` /
   `read_session_history`, `[pi]` config. Independent of phases 1-2 and can run
   in parallel.
4. **Gateway, setup and docs:** subprotocol, `setup`/`uninstall`, AGENTS.md
   invariant, `docs/pi.md`, skill and `docs/collab-skill-evals.md` updates.

## Sources

- https://github.com/earendil-works/pi (docs under `packages/coding-agent/docs/`)
- https://www.npmjs.com/package/@earendil-works/pi-coding-agent
