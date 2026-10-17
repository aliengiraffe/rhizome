# Verification record and interactive checklist

Verified on macOS with Claude Code **2.1.302**, 2026-10-16. No older-client claim.
The production command is `rhizome mcp`; the randomized demo is separate.

## Automated checks

All passed:

- `go vet ./...`
- `go test -race ./...`
- `make build`
- `python3 scripts/smoke_stdio.py`
- `git diff --check`

The Go suite exercises concurrent registry bootstrap, distinct auto-allocated
ports, authentication rejection, duplicate live session IDs, binding refresh and
retirement, stale/child hooks, unsubscribe queue removal, transcript partial
writes, historical partial-line baselines, duplicate records, truncation,
filesystem-watch failure/poll recovery, bounded replay and expiry, concurrent
replay/live boundaries, slow subscriber disconnection, bounded receiver queues,
rate limits, channel handshake, prompt explicit delivery, 8 KiB batching and
30-notification circuit-breaker re-arm.

The Python smoke starts actual binary processes with independent stdio pipes and
a private cluster. It checks every stdout line as JSON, initialization, UNBOUND
startup, same-directory binding, announcements, direct messages, transcript
notifications, publisher process restart/gap, rebind fences and EOF shutdown with
registration cleanup. It uses no models or external services.

## Interactive Claude results

Two actual interactive Claude processes used the repository `.mcp.json` and
`.claude/settings.json` in the same working directory, with the development channel
flag. Native tools were initially disabled and Rhizome tools explicitly allowed.
A resumed session enabled only `Read` to exercise the non-Rhizome tool hook.

| Check | Observed result |
| --- | --- |
| MCP protocol/channel handshake | Channel registered with MCP 2025-11-25. The latest SDK's default 2026-07-28 negotiation initially caused Claude to skip channel registration; the adapter now constrains negotiation, with a regression test. |
| Initial SessionStart | Claude skipped the MCP-tool hook with its documented non-blocking notice. Rhizome logged UNBOUND before any prompt. |
| First prompt | UserPromptSubmit called `rhizome/observe_session`; two distinct IDs bound independently despite identical cwd/display names. |
| Hook result parsing | Claude accepted the tool's `{}` text as hook JSON for prompt, stop, Read, clear and compact events. |
| Recursion exclusions | Rhizome list/subscribe/send/publish calls did not invoke PreToolUse; a normal Read did invoke PreToolUse successfully. |
| Direct send | A unique direct marker from A appeared as a channel injection in B with A's origin metadata and a stable message ID; B reported receiving it. |
| Announcement | B's published marker arrived in A's channel after subscription. |
| Assistant activity | B's assistant marker and subsequent new work reached A; the reverse subscription delivered A's activity to B. |
| Literal echo filtering | Channel injections were `attachment/queued_command` records and were not republished. Rhizome tool calls/results were excluded. |
| Peer-triggered feedback | Both models generated new commentary about peer activity even when told not to reply. This is permitted local activity, not literal envelope recirculation. The automated test, rather than this variable-speed model run, verifies the 30-notification threshold and explicit re-arm. |
| `/clear` | Later SessionStart succeeded. B advanced to generation 2 with a new session ID, no subscriptions and queue depth 0. A received session-ended for B's old binding. |
| `/compact` | Later SessionStart succeeded; B kept its new session ID and generation 2 with no subscriptions. |
| `--resume` with fresh MCP process | A retained its Claude ID, acquired a different node ID, bound at local generation 1 and had no subscriptions or queued traffic. Old process state was not restored. |
| Exit | Test Claude processes were stopped; their Rhizome registrations were removed. |

The process-level test separately checks a publisher restart while its subscriber
stays alive. Delayed old hooks, replay races, queue saturation and circuit-breaker
thresholds are deterministic automated scenarios, not claims about manually
forcing every failure in the interactive UI.

## Repeatable manual steps

1. Build, open two terminals in this project, and run
   `claude --dangerously-load-development-channels server:rhizome` in each.
   Accept local development channel and project MCP consent when shown.
2. Submit a prompt in each asking for `list_sessions`. Compare exact IDs, not
   display names; check initially empty subscriptions and queues.
3. In A, subscribe to B's exact ID. In B, publish a unique marker, then produce a
   distinct assistant text marker. Verify both arrive in A with B's metadata.
4. Send a direct unique marker from A to B without requiring B to subscribe.
   Verify the sender reports accepted and B receives the correctly addressed
   channel message. Acceptance alone does not prove Claude read it.
5. Subscribe in both directions if testing peer-triggered work. Stop the test with
   unsubscribe or `/clear`; model commentary can generate further activity.
6. Run `/clear` in B, query status and verify the new generation has no old queue
   or subscriptions. Run `/compact` and verify the generation stays unchanged.
7. Exit A and resume its Claude ID. Verify a new node incarnation and no restored
   subscription intent. Read README.md with the built-in Read tool and inspect the
   debug log for one successful PreToolUse hook, with none for Rhizome tools.
8. For transport/replay failure cases run the automated suite and subprocess test.
   Debugging uses Claude's `--debug-file` and Rhizome stderr; do not commit private
   transcripts or tokens as fixtures.

## Codex 0.163.0 (2026-10-16)

- Generated schemas and CLI help checked against installed `codex-cli 0.163.0`.
  The running local daemon also reported 0.163.0. Unix control transport is
  WebSocket, not newline JSON or a raw `app-server proxy` pipe.
- Two same-directory diagnostic threads exposed different `_meta.threadId`
  values. A deliberately conflicting `mcpServer/tool/call._meta.threadId` was
  overwritten by Codex. The sanitized request contract is captured in
  `internal/channel/testdata/codex-0.163.0-call.json`. These probes used separate
  processes; the shared-process case is exercised concurrently by Go SDK tests.
- Interactive TUI hook review/trust was exercised through the normal `/hooks`
  flow. A real `UserPromptSubmit` bound the session automatically; an independent
  status read confirmed `BOUND`, its exact thread UUID, and `watching`. No manual
  binding call was made for that TUI thread. Missing `agent_id` placeholders failed
  in the first attempt; the final sample omits unavailable optional fields. The
  model's subsequent `list_sessions` call waited for ordinary tool approval and
  was interrupted during cleanup; automatic hook binding itself succeeded.
- `scripts/smoke_codex.py --live` passed against the running daemon: two new
  Codex threads, exact routing despite conflicting request metadata, direct
  Codex-to-Codex queue handoff, real generated `ACK` and turn completion collected
  through read-only pagination, Codex-to-Claude channel notification, and reverse
  Claude-protocol-to-Codex queue delivery. Disposable threads were archived.
  The Claude endpoint in this test is an MCP protocol harness, not another
  interactive Claude model. The earlier Claude interactive results above remain
  the evidence for actual Claude channel rendering.
- `make check` (`go vet ./...`, `go test -race ./...`) and
  `scripts/smoke_stdio.py` pass. Codex tests cover missing/malformed caller
  metadata, conflicting hooks, nullable transcript path, concurrent callers on
  one MCP connection, baseline/delta deduplication, tool completion, excluded
  content, rollback gaps, bounded pagination, unavailable/unloaded delivery,
  shell-safe queue arguments, cancellation, nonzero exit, bounded CLI output,
  and rejection of mutating App Server methods. Existing replay/backpressure and
  circuit-breaker tests exercise the shared delivery path.
- Ephemeral threads reject pagination. New durable threads reject pagination
  until their first message is materialized; this known empty state is treated
  as an empty live baseline. Unsupported history remains visibly paused.
- Installed a user MCP entry and four hooks on this laptop, with a timestamped
  config backup and unrelated config text preserved. The normal TUI flow trusted
  those four definitions. No daemon restart, hook-trust bypass, remote endpoint,
  or env-manager runtime change was used.

Remaining manual coverage: interactive `/clear`, fork/resume/compaction and MCP
restart in Codex, an active-turn queue smoke, and a simultaneous real Claude model
plus real Codex model smoke. These platform lifecycle cases are not claimed as
verified by the automated protocol tests. Process restart still intentionally
loses subscriptions, replay, deduplication and delivery cursors.
