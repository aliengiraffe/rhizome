# Cursor integration

Rhizome serves Cursor IDE agent chats and Cursor CLI (`agent`) sessions with
`rhizome mcp --agent cursor`. The agent is chosen per launch with `--agent`,
never from the [configuration file](../README.md#configuration), whose
`[cursor]` table holds the Cursor user directory and the one delivery policy
switch. Discovery, authenticated gRPC, subscriptions, announcements, direct
messages, replay limits, session names and feedback controls are shared with
Claude and Codex. Cursor mode does not advertise `claude/channel` and offers no
`observe_session` tool: binding comes from Cursor's shell hooks.

**Status:** implemented and verified with synthetic hook payloads
(`scripts/smoke_cursor.py`, race tests). The live checks against a Cursor
install are a manual checklist in [verification.md](verification.md) and have
not been recorded yet; the version contracts below are taken from the Cursor
hooks reference as of 2026-10-20 (`hooks.json` `version: 1`).

## Why Cursor is different

Two facts shape the design, both from Cursor's own documentation and forum:

- MCP `tools/call` requests carry **no conversation identity** (`_meta` is
  empty), and one stdio MCP process is shared by every chat in an IDE window.
- Nothing can push into an idle chat. Hooks can inject context only at tool and
  turn boundaries (`postToolUse.additional_context`, `stop.followup_message`).

So the Claude binding model (one process, one session, hook-bound) does not
carry over. Cursor mode is closest to Codex: per-conversation nodes inside one
MCP process, except that identity arrives through `rhizome hook --agent cursor`
rather than through transport metadata.

## Install

Build this checkout with `task build`, then:

1. Run `./rhizome setup --agents cursor` (or pick Cursor in the interactive
   picker). Setup detects Cursor from `~/.cursor` (or `[cursor].config_dir`) or
   a `cursor`, `agent` or `cursor-agent` executable on PATH, and installs at
   user scope: the skill (`~/.cursor/skills/rhizome-collab/SKILL.md`), the
   `rhizome` stdio server in `~/.cursor/mcp.json` (absolute binary path,
   `mcp --agent cursor`), and the eight hook entries in `~/.cursor/hooks.json`
   (`version: 1`). Every hook entry runs `<binary> hook --agent cursor`, is
   non-blocking (`failClosed: false`, 10 s timeout); `beforeMCPExecution` has
   `matcher: "rhizome"` and `stop` sets `loop_limit: 1`. The merge touches only
   Rhizome's entries, keeps other servers and hooks, backs up changed files as
   `<file>.bak-<timestamp>` and refuses to replace malformed files. The same
   definitions are in `examples/cursor/` for a manual merge or a project-scope
   `.cursor/` install; do not keep the same hooks at both scopes.
2. Approve the server in the IDE's MCP settings; for the CLI run
   `agent mcp enable rhizome` or pass `--approve-mcps`. Reload Cursor so it
   reads the new hooks.
3. If Rhizome's configuration file was selected explicitly
   (`--config`/`RHIZOME_CONFIG`), setup threads `--config <path>` into both the
   server entry and the hook command; otherwise hooks and the server resolve
   the default `~/.rhizome/config.toml` at launch, exactly like every other
   command.

Open a chat, submit a prompt and ask Cursor to call `list_sessions`. Expect
`binding: BOUND`, `collection_state: hooks` and `delivery_mode: hook_pull`.

## Binding

Every Cursor agent hook payload carries `conversation_id` and
`workspace_roots`. Rhizome uses `conversation_id` as the session ID and binds
on `sessionStart`, `beforeSubmitPrompt`, `postToolUse`, `postToolUseFailure`,
`afterAgentResponse`, `beforeMCPExecution` and `stop`; `sessionEnd` retires the
node, and the next hook for that conversation starts a fresh node with a new
node ID and generation. Hooks with `subagent_id` or `parent_conversation_id`,
`is_background_agent: true` payloads, `subagentStart`/`subagentStop` and every
other event are ignored. The project label is the tool `cwd` when present,
otherwise the first absolute workspace root; relative roots cannot bind.

Finding the MCP process: on start, `rhizome mcp --agent cursor` listens on a
0600 unix socket under `<runtime>/cursor/` and writes a registration with its
PID, its own working directory, and later the roots returned by the client's
`roots/list`. The two launchers differ here: the IDE's MCP helper starts the
process in the home directory and answers `roots/list` with the workspace;
the Cursor CLI starts it in the workspace and does not answer `roots/list`.
The hook reads the registrations, drops dead PIDs, and ranks each one against
the payload's `workspace_roots`: an exact match first, then a payload root
inside a registered root (the most specific wins; the home directory and `/`
never claim nested workspaces), then a registered root inside a payload root.
Exactly one candidate must remain in the best tier; zero or several, including
two windows or a window plus a CLI session on one workspace, fail closed: the
hook answers with Cursor's own default and logs a counter to stderr.
`list_sessions` reports `cursor_processes`, the number of live registrations,
to make that diagnosable. No process-tree walking is involved.

The hook forwards a minimized message only: event name, conversation ID,
workspace roots, `cwd`, tool name, assistant text (bounded to 7 KiB), stop
status and loop count, a digest of the tool input for Rhizome tool calls, and
a boolean saying whether the prompt was a Rhizome envelope. The prompt itself,
attachments, tool inputs and outputs, transcripts and `user_email` never leave
the hook process. Payloads above 2 MiB are dropped.

## Tool attribution

Tool calls still carry no identity, so one process may host several
conversations. Resolution order for every tool, including the history and file
tools:

1. A pending **ticket** from the `beforeMCPExecution` hook: the hook registers
   `{conversation_id, tool_name, sha256(canonical tool_input)}` for the
   `rhizome` server, valid for five seconds and consumed once. The `tools/call`
   whose name (ignoring any client prefix) and canonical arguments match is
   attributed to that conversation. A ticket with no input matches on the name.
   Several matching tickets are ambiguous and consume nothing.
2. Without a ticket, a process with exactly **one** conversation resolves to it.
3. Anything else returns the stable error `identity_ambiguous`; `send`,
   `subscribe`, `publish`, `set_session_name` and the rest never guess.

The shipped `beforeMCPExecution` entry answers `{"permission": "allow"}` for
Rhizome's own tools in that conversation. If you prefer Cursor's approval prompt
for them, remove the entry: single-chat windows keep working and multi-chat
windows fail closed. For other MCP servers the hook answers `ask`, which leaves
Cursor's normal behavior in place. Whether Cursor's `matcher` scopes this hook by
tool or server name is a live check; the hook filters by `mcp_server_name` in
either case.

## Collection

Collection is hook push only; no transcript is tailed.

- `afterAgentResponse.text` becomes `activity`.
- `postToolUse` / `postToolUseFailure` become `Tool completed: <name>` /
  `Tool failed: <name>`; Rhizome's own tools are skipped.
- `stop` with `status: completed` becomes the `turn` marker (never queued to
  subscribers).

Never collected: prompts, `afterAgentThought`, tool inputs and outputs, file
edits, subagent activity, aborted turns. A turn whose prompt was a Rhizome peer
envelope (the same `isPeerInput` check the Claude transcript normalizer uses)
is peer-only: its text, tool markers and turn marker are suppressed and counted
until the next real prompt. Explicit `publish` and `send` remain available.

The direct collector takes the per-session collection lease like the other
integrations. The background observer does not index Cursor logs (see History).

## Delivery

A Cursor node receives events through the shared bounded queue (256 events,
2-second/8 KiB automatic batches, 30-notification circuit breaker, explicit
`subscribe` re-arm). Instead of writing a channel, delivery parks each
formatted notification in a per-node **outbox** (32 entries, 64 KiB); when it is
full, events stay in the delivery queue rather than being dropped. Hooks drain
it:

- `postToolUse` / `postToolUseFailure` return `additional_context` with up to
  8 KiB of `<rhizome-peer-event>` blocks (origin IDs plus content, framed as
  informational and untrusted). This never starts work; the model sees it with
  the tool result it was already waiting for.
- `stop` returns `followup_message` when the turn completed, `loop_count` is 0
  and the outbox is non-empty. Cursor then auto-submits that text as the next
  user message, which consumes model usage and starts a turn; the shipped entry
  caps it with `loop_limit: 1`, and the turn is marked peer-only so its output
  is not re-collected. This is on by default so queued peer events reach a chat
  as soon as its turn ends; set `[cursor] followup_delivery = false` to deliver
  only alongside tool results (restart the MCP server to apply).
- `sessionStart` returns static usage instructions only, never queued events.

Events for an idle chat wait in the queue and outbox until the next turn.
`list_sessions` reports `delivery_mode: hook_pull` and `outbox_depth` for the
local node; peers see an ordinary live Cursor session and `delivery_capable:
true`, because `send` is accepted into the queue. Acceptance still means
queued, not read.

## Naming and history

Hooks and transcripts expose no native chat title, so a Cursor session's name is
an explicit `set_session_name` override or the fallback
`<display_name> · cursor · <first 8 characters of conversation_id>`. Overrides
persist under `<runtime>/names/`.

`search_sessions` and `read_session_history` are available from Cursor sessions
and query Claude and Codex history as documented in [history.md](history.md).
Cursor's own logs (`~/.cursor/projects/*/agent-transcripts`, the IDE SQLite
store, `~/.cursor/chats`) are **not indexed**: the JSONL format is undocumented
and reportedly merges reasoning into text, which the shared content policy
excludes, and the SQLite store holds credentials. Indexing can follow once
redacted fixtures exist.

## Known limitations and live checks

- Cursor's process layout, hook payload stability across releases, the
  `beforeMCPExecution` matcher semantics and MCP tool naming in `tool_name` are
  unverified live; run the checklist in [verification.md](verification.md) after
  installing or upgrading Cursor.
- Cursor loads third-party Claude Code hooks from `~/.claude/settings.json`
  when that option is enabled (always, for the CLI). Rhizome's Claude hooks are
  `mcp_tool` entries, which Cursor documents no support for; confirm they are
  ignored and that Cursor never launches a Claude-mode `rhizome mcp`. If it did,
  that process would stay UNBOUND and fail closed.
- Two IDE windows on the same workspace, or a window whose MCP client does not
  answer `roots/list` alongside another live process, fail closed for hooks.
- There is no idle wake. Peer events reach a chat at its next tool result or
  right after its turn ends (unless followup delivery is disabled).
- Hooks run per event as short-lived processes; a slow Rhizome never blocks
  Cursor (`failClosed: false`), at the cost of silently missing that event.
- Up to 64 conversations per MCP process; restart the server to release them.

## Verification

```sh
task check
task build
python3 scripts/smoke_cursor.py
```

The smoke starts a Cursor-mode process, a Claude-mode peer and real
`rhizome hook` subprocesses fed synthetic payloads. It checks binding, labels,
cross-agent discovery and subscriptions, hook-pushed activity and tool markers,
hook-pull delivery of direct messages and announcements, default-off and opt-in
followup delivery, ticket attribution with two conversations, other-server and
subagent payloads, peer-turn suppression, `sessionEnd`, EOF shutdown and
registration cleanup. It launches no model and needs no Cursor installation.

References: [Cursor hooks](https://cursor.com/docs/agent/hooks),
[Cursor MCP](https://cursor.com/docs/context/mcp),
[Cursor CLI](https://cursor.com/docs/cli/overview).
