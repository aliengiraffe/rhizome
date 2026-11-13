# Session privacy

Sessions are public by default. `set_session_visibility(visibility="private")`
disconnects the calling session from Rhizome collaboration. Only a verified,
bound caller may use this tool; it has no target-session argument. Use
`visibility="public"` to make it available again. `list_sessions` retains the
caller's local identity and reports `visibility`, even while private.

For human management, including inactive sessions and choices made before a
session starts:

```sh
rhizome session visibility <exact-local-session-id>
rhizome session visibility <exact-local-session-id> private
rhizome session visibility <exact-local-session-id> public
rhizome session visibility --config /absolute/config.toml <exact-local-session-id> private
```

Flags precede the ID. The CLI operates only in the selected local runtime; an ID
is never resolved to a remote host. It accepts an exact ID before that session is
known locally, so check the ID carefully. Names, project paths and PIDs are not
identities. Inspection returns JSON with `session_id`, `visibility` and `applied`.

## Enforcement and completion

Policy is stored separately from history in private, atomically replaced files
under `<runtime>/visibility/`, keyed by SHA-256 of the exact session ID. MCP,
observer and gateway access use the same policy. Missing choices mean public;
malformed, non-regular, non-private or unreadable records deny access. Upgrade
all local Rhizome processes before using privacy; an older running process cannot
acknowledge the new policy.

A private session exposes no session ID, name or project in gossip or RPC
identity responses. Cached endpoints cannot publish, send, subscribe, replay or
receive for that session. The observer does not create a replacement publisher
when a private direct integration exits. Private sessions are excluded from
recent metadata and local and paired-host history, regardless of catalog age.
Reads of a private session, including existing cursor requests, return
`SESSION_NOT_FOUND`. The observer removes its catalog rows and stops indexing
beyond the identity needed to recognize a private source. Original logs remain.

Changes are serialized within the runtime. The initial atomic private-policy
replacement is the revocation point for new access. Both a private transition
and explicit re-publication first use this private barrier. Existing history
queries finish before that replacement; new queries wait for the transition.
Native watches on the policy directory, with bounded reconciliation, notify all
registered local nodes. Each node drains affected in-flight collaboration
handoffs, closes streams, cancels subscriptions and removes retained and queued
content. The observer acknowledges after flushing the filtered catalog. Process
locks distinguish live processes from exited ones.

Success (`applied: true`) means every live registered local node and the running
observer acknowledged revocation. Gossip withdrawal and metadata already copied
to a paired host converge separately. A ten-second deadline, an old process, or
an I/O failure can leave a **persisted private choice with incomplete application**.
The command/tool reports an error, and the CLI reports `applied: false`; inspect
and retry after resolving the failing process. A request to make the session
public never bypasses an incomplete private barrier. A process started later
checks persisted policy before exposure. A crashed writer leaves private intent
in place and can be retried.

Re-publication creates a fresh live stream and collector baseline. Subscriptions
are not restored automatically; explicitly subscribe again. File collectors
skip to current EOF, Codex takes a new baseline, and OpenCode/Pi wait for a fresh
human input. Their relay frames carry `emitted_at` milliseconds captured before
buffering; pre-publication or unstamped frames are excluded after a transition.
Rerun `rhizome setup --agents opencode,pi` to install updated relays. Older remote
subscribers attempting to resume a revoked stream are denied; they must start a
new explicit subscription.

## Limits

Privacy does not delete native transcripts, retract metadata or content already
handed to another process/agent, or protect against other programs running as the
same OS user. It is not a peer-specific ACL or a viewer-local ignore filter.
After explicit re-publication, source logs may again be read through history;
private-interval exclusion applies to live collaboration replay.

A session first made private through its tool may already have been advertised.
An already-persisted private choice prevents exposure on restart. Project
exclusions and private-by-default configuration are separate follow-up work.

## Verification

`visibility_test.go` covers persistence, corrupt policy, incomplete process
acknowledgement, active revocation, cached endpoints, concurrent transitions,
startup and stale history cursors. Run `python3 scripts/smoke_visibility.py`
after building for real-process CLI/MCP/observer, handoff and restart coverage.
The usual loopback, LAN, gateway and injected-daemon Tailscale tests still apply.
Real Codex/OpenCode model-backed checks and two-device networking remain separate
from these synthetic tests; see [verification](verification.md).
