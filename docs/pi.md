# Pi integration (in progress)

Targets Pi **1.4.0** (`@earendil-works/pi-coding-agent`); earlier versions without
built-in MCP (before 0.99.0) are unsupported. Design, research and spike results:
[docs/superpowers/specs/2026-10-28-pi-support-design.md](superpowers/specs/2026-10-28-pi-support-design.md).

## Status

| Capability | State |
| --- | --- |
| Verified binding (nonce + parent PID over a private socket) | Implemented |
| `list_sessions` shows the live Pi session; Pi native title (`session_info`) | Implemented (title unverified live) |
| Peer delivery as `pi.sendMessage` custom messages, with acknowledgement | Implemented; `smoke_pi.py` delivers a peer `send` into a real Pi session (stub model) and checks the model saw the peer envelope |
| Activity collection (assistant text, tool start/end, turn end; prompts and tool bodies only with `[sessions].share_prompts_and_tools`) | Implemented; the same redaction, peer-turn suppression and gap reporting as OpenCode |
| History source (`search_sessions`, `read_session_history`, `agent_type: "pi"`), local only | Implemented: the observer indexes `pi.sessions_dir`, never tails it ([history.md](history.md#sources-identity-titles)) |
| `recent` rows, peer history (`[pi.peer_history]`) | Not implemented |
| `rhizome setup` / `uninstall --agents pi`, skill install | Implemented |
| Paired-host gateway (`pi` sessions are not exported) | Not implemented |

## Install

```sh
rhizome setup --agents pi      # skill + extension; add --list to preview
rhizome uninstall --agents pi  # removes only those Rhizome-owned files
```

Setup writes, under `$PI_CODING_AGENT_DIR` (default `~/.pi/agent`):

- `skills/rhizome-collab/`, the collaboration skill;
- `extensions/rhizome/index.js`, the embedded extension (reinstalled on every
  upgrade so it matches the binary's bridge protocol);
- `extensions/rhizome/rhizome.json`, generated: the absolute executable and,
  only when you selected a configuration with `--config`/`RHIZOME_CONFIG`, its
  path. It never holds a token.

Pi's own `settings.json` and `mcp.json` are never edited: the extension registers
`rhizome mcp --agent pi` for each session itself. Restart Pi (or `/reload`)
afterwards. A symlinked extension or skill is left alone, so a checkout you manage
keeps working; `uninstall` skips it as well.

`RHIZOME_BIN` in Pi's environment overrides the pinned executable. If `runtime_dir`
is set in `~/.rhizome/config.toml`, set the same `RHIZOME_RUNTIME_DIR` for Pi,
because the extension cannot read that file.

Without the extension nothing binds: `rhizome mcp --agent pi` exits because
`RHIZOME_PI_NONCE` is missing.

## Collection

The extension relays raw events of its own session over the private socket and
Go decides what is published (`internal/channel/opencode_collect.go`, shared with
OpenCode): `message_end` (user and assistant text), `tool_execution_start|end`
(top-level calls only; nested calls from codemode are skipped), and `agent_end`.
Rhizome's own tools (`mcp__rhizome__*`) never feed back. A delivery, once Pi feeds it to the agent, marks the
turn as a reaction to a peer, so it is not republished until the next human prompt.
Events that happen before the bind is written wait in a 256-entry queue; overflow
and reconnects are reported to subscribers as visible `gap` events.

## Verify

`go test -race ./internal/channel -run TestPi`, `task test-plugin` (extension
tests, Node 20+) and `python3 scripts/smoke_pi.py`. The smoke runs a real
`pi --mode rpc` with a loopback OpenAI-compatible stub registered through
`models.json` in a private agent directory (no credentials, no real provider),
a peer session in the same runtime, and covers bind, `/new` rebind, published
activity, peer `send` delivery and non-republication of the reaction.
