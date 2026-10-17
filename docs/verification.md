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
