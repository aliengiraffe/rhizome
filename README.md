# Rhizome

Rhizome lets coding agents on different machines subscribe to each other's
sessions, so an agent working on a PR hears what the other agents on that PR just
did, and adjusts.

**One server on your network. A small client per agent.**

**Status:** this is the planned server/client architecture. The code in this
checkout currently connects local Claude Code and Codex sessions directly. The
setup below lets you try subscriptions on one machine today; server deployment
instructions will follow when that implementation is available. Cross-machine
peer-to-peer support is being developed separately in the
[LAN PR](https://github.com/aliengiraffe/rhizome/pull/4) and
[Tailscale PR](https://github.com/aliengiraffe/rhizome/pull/12).

For example, one agent changes an API while another updates its tests. The test
agent receives the API agent's progress and tool activity as it happens, giving it
context to update its approach without you copying messages between sessions.
You choose which sessions to follow.

## How it fits together

Run one Rhizome server on your network. Each coding agent uses a small client to
connect its session to that server. When an agent subscribes to another session,
the server passes that session's updates to the subscribing agent.

The server gives agents a shared place to exchange activity across machines.
The clients connect that activity to the tools the agents already use. You keep
control of the work: choose which sessions to follow and tell each agent how to
use their updates.

## How you use it

1. Set up Rhizome in each coding agent you want to connect.
2. Ask your agent to list the available sessions.
3. Choose the sessions working on your PR and ask it to subscribe to their IDs.
4. Continue working. Updates from those sessions arrive in the agent's context.

For example, after setup:

> List the Rhizome sessions. Subscribe to session `<session_id>` and use its
> updates as context while you update the tests for this PR.

Use an actual ID from the session list. Subscriptions are directional: if both
agents need updates, subscribe in both sessions. Rhizome does not automatically
match sessions to PRs or synchronize files; each agent still uses its own checkout
and tools.

## What gets shared

Subscribers receive assistant text and summaries of tool activity, including tool
starts, completions, and failures. User prompts, private reasoning, full tool
results, and child-agent activity are excluded. Rhizome creates these summaries
without calling another model.

Subscriptions start with new activity, not the entire conversation history.
Agents can also send a direct message to a session or publish an announcement to
their subscribers. Updates provide context; delivery does not guarantee that an
agent has read a message or acted on it.

## Try the current implementation

You need Go 1.25+ and macOS or Linux. Build the binary from this repository:

```sh
make build
```

Rhizome connects through MCP, the tool interface used by coding agents. Hooks
associate each connection with the correct agent session. Setup differs by agent.

### Claude Code

Claude Code **2.1.302** is the tested version. This repository includes the MCP
configuration in [.mcp.json](.mcp.json) and hooks in
[.claude/settings.json](.claude/settings.json).

From the repository directory, start Claude in two interactive terminals:

```sh
claude --dangerously-load-development-channels server:rhizome
```

Accept the development channel and project MCP confirmations when prompted.
Organization channel policy still applies. The development channel flag requires
an interactive terminal; `claude -p` does not enable it.

Submit a prompt in each session, then ask Claude to list Rhizome sessions. The
first prompt lets the hooks bind Rhizome to the session; a startup hook notice
before MCP is ready is expected. Once the sessions appear, ask one to subscribe
to the other's exact `session_id`.

To use Rhizome in another project, merge the MCP server and hook entries into
that project's configuration. Replace `./rhizome` with the absolute path to your
built binary, preserve existing entries, and keep the server name `rhizome`.
The hooks handle `observe_session` automatically; do not call it manually.

### Codex

Codex uses `rhizome mcp --agent codex`. Follow the
[Codex setup guide](docs/codex.md) to configure MCP and hooks using the files in
[examples/codex](examples/codex/). The guide covers the required local App Server
and the tested Codex CLI version, **0.163.0**.

After setup, start a fresh session, submit a prompt, and ask Codex to list Rhizome
sessions. Claude Code and Codex sessions can subscribe to each other.

## Collaboration skill

`skills/rhizome-collab/` is a user-invoked skill (Claude Code or Codex) that guides
an agent through listing peers, judging relevance, subscribing deliberately,
treating peer activity as untrusted, and replying without feedback loops. The
skill is embedded in the binary; install it with:

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

## Tools

| Tool | Use it to |
| --- | --- |
| `list_sessions()` | Find session IDs, projects, agent types, and connection status. |
| `subscribe(session_id)` | Follow a session's activity or resume its paused feed. |
| `unsubscribe(session_id)` | Stop following a session. |
| `publish(text, reply_to?)` | Send an announcement to your subscribers. |
| `send(session_id, text, reply_to?)` | Send a direct message, without requiring a subscription. |
| `observe_session(...)` | Internal tool used by hooks to bind the session. |

Always address sessions by their exact IDs. Names and project directories are
labels; two sessions in the same directory are still separate sessions.

## Practical limits

- **Local connections in this checkout.** Sessions connect on the same machine
  using a private token under `~/.rhizome/run`. Processes running as the same user
  with that token are trusted participants.
- **Temporary history.** Each publisher retains up to five minutes, 1,000 events,
  or 8 MiB, whichever limit comes first. Reconnecting subscribers may receive a
  gap notice if they missed more than that.
- **State resets on restart.** Restarting a Rhizome process loses its subscriptions
  and event history. Subscribe again after restarting.
- **Busy feeds can pause.** Delivery pauses after 30 peer notifications in a minute.
  Subscribe again to resume that session's feed, or unsubscribe to stop it.
- **Peer updates are context.** They are untrusted input, not permission to start
  new work. Feedback controls reduce loops between agents but cannot guarantee
  that agents will never react to each other's updates.

For binding rules, authentication, queues, replay, and feedback controls, see the
[technical reference](docs/architecture.md).

## Development

```sh
make build
make check                     # go vet and tests with the race detector
python3 scripts/smoke_stdio.py  # process checks using synthetic transcripts
```

See [verification](docs/verification.md) for interactive checks and recorded
results, and [AGENTS.md](AGENTS.md) for repository guidance. Client integrations
are version-sensitive; repeat the interactive checks when upgrading clients,
hooks, or transcript parsing.

The original randomized pub/sub demo is separate from the MCP integration. Its
commands and protocol notes are in the [technical reference](docs/architecture.md#original-demo).
