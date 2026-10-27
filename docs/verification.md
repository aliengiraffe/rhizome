# Verification record and interactive checklist

Verified on macOS with Claude Code **2.1.302**, 2026-10-16. No older-client claim.
The production command is `rhizome mcp`; the randomized demo is separate.

## Automated checks

All passed:

- `go vet ./...`
- `go test -race ./...`
- `task build`
- `python3 scripts/smoke_stdio.py`
- `python3 scripts/smoke_stdio.py --lan`
- `git diff --check`

The Go suite exercises concurrent registry bootstrap, lock-gated registration
pruning (failed or timed-out probes of live owners keep their registration;
exited owners are pruned) and registration self-healing, distinct auto-allocated
ports, authentication rejection, duplicate live session IDs, binding refresh and
retirement, stale/child hooks, unsubscribe queue removal, transcript partial
writes, historical partial-line baselines, duplicate records, truncation,
filesystem-watch failure/poll recovery, bounded replay and expiry, concurrent
replay/live boundaries, slow subscriber disconnection, bounded receiver queues,
rate limits, channel handshake, prompt explicit delivery, 8 KiB batching and
30-notification circuit-breaker re-arm.

Session names (2026-10-19): unit tests cover label normalization (Unicode,
control characters, whitespace, instruction-like text, 96-byte rune-safe
bounds), stable fallbacks, explicit > native > fallback precedence including
title arrival/change/removal and override clearing, stale-generation fencing,
override persistence across a node restart and non-inheritance by another
same-directory session, gossip metadata with maximum-size name/project/session
ID still advertising with routing fields intact, Claude `ai-title`/`custom-title`
transcript records (pre-binding baseline pass and live updates, foreign and
malformed records ignored, no activity published), Codex `thread.name` through
the fake App Server (native title, change, confirmed removal, transient failure
kept), per-thread `set_session_name` isolation on a shared Codex connection,
unbound rejection for both agents, and a live rename observed by a subscribed
peer without any change to generation, node ID, stream, listeners or feed. The
stdio smoke exercises the fallback label, explicit naming visible to a peer
process, oversized rejection, persistence across a publisher restart and
clearing. The Claude record shapes were checked against local transcripts
(`{"type":"ai-title","aiTitle":…,"sessionId":…}` and
`{"type":"custom-title","customTitle":…,"sessionId":…}`); the Codex field was
checked against the generated App Server v2 schema for 0.163.0.
`scripts/smoke_codex.py --live` passed against the running 0.163.0 daemon after
this change (it does not exercise naming itself). Interactive rendering of names
in either client UI was not re-run for this change.

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
- `task check` (`go vet ./...`, `go test -race ./...`) and
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

## Background observer (#10, 2026-10-19)

Verified on macOS in this implementation:

- `make build`, `make check` (including race tests), and existing stdio smoke.
- Native-notification tests for Claude/Codex logs, late-created nested roots,
  startup history boundaries, partial lines, rotation, filtering, ownership
  handoff in both orders, duplicate direct rejection, and passive delivery errors.
- `scripts/smoke_service.py`: separate observer and MCP processes, discovery,
  direct delivery during coexistence, observer takeover, restart, and no duplicate
  replay. Uses synthetic logs and an isolated runtime.
- Native LaunchAgent install, identical reinstall, status, stop, start, status,
  and uninstall, with a temporary HOME/config/runtime and synthetic log roots.
  The temporary service was removed. No user agent configuration was changed.
- `scripts/smoke_codex.py --live`: existing authenticated daemon, disposable
  threads, real assistant collection and bidirectional queue/Claude-protocol
  delivery. Confirms direct Codex regression coverage; this is not a Windows
  App Server delivery claim.
- Linux amd64 and Windows amd64 cross-compilation; platform service definition
  XML/argument tests. Cross-compilation does not validate native service execution.

Not exercised here: actual logout/login or reboot startup, native Windows Task
Scheduler and Linux systemd installation/recovery, and Windows ACL behavior at
runtime. Run those platform smoke checks before claiming runtime verification
on those operating systems. See [service.md](service.md) for scope and limits.

## LAN mode (2026-10-16)

Automated, all on one macOS host:

- `internal/channel/lan_test.go`: address-policy table for both modes (loopback,
  RFC 1918, link-local, ULA, public v4/v6, malformed); two LAN-mode nodes with
  separate runtime directories and a shared token joined via `--join`, exercising
  subscribe, announce, direct send and replay-gap semantics over pinned TLS; a
  node with a different token refused at gossip (`No installed keys could
  decrypt`) and `Unauthenticated` at gRPC; a forged fingerprint failing the
  handshake with the bearer never sent; unpinned and public endpoints refused
  before dialing; an unreachable `--join` seed logged without aborting startup;
  local mode ignoring LAN advertisements and LAN mode ignoring unpinned or
  public peers; a shared registry across modes leaving the other mode's live
  registration intact; conflicting, malformed, file-sourced and `rhizome token`
  tokens.
- `python3 scripts/smoke_stdio.py --lan`: the full stdio scenario with one
  runtime directory per process, `--lan --bind-addr 127.0.0.1 --mdns=false`,
  token seeding via `RHIZOME_TOKEN_FILE`, `--join` seeding, publisher restart
  rejoining through the surviving peer, and EOF cleanup of every registry.
- Two real `rhizome mcp --lan` processes with distinct runtime directories on the
  actual Wi-Fi interface: bind `0.0.0.0`, advertise auto-detected as the private
  interface address, mDNS advertisement and browse found the other node and
  joined gossip within the first browse (`seed_joined source=mdns`). mDNS logs
  one informational line when IPv6 multicast is unavailable; IPv4 is sufficient.

Not verified here: two physically separate devices. Repeat the manual steps below
when changing discovery, TLS pinning, the address policy or the mDNS library.

### Two-device manual steps

1. On device A, `./rhizome token`; copy the value to device B. Build on both.
2. Start `claude --dangerously-load-development-channels server:rhizome` on each
   with the project `.mcp.json` carrying `RHIZOME_LAN=1` (and `RHIZOME_TOKEN` on
   B's first run). Allow the macOS incoming-connection prompt on both.
3. In each, call `list_sessions` after the first prompt. Verify the other
   device's session appears with its `host` label, a non-loopback `endpoint`,
   and a 64-character `cert_fingerprint`. If mDNS is blocked, add
   `RHIZOME_JOIN=<other gossip_endpoint>` and restart; verify `seed_joined` in
   stderr.
4. Subscribe from A to B's exact session ID, publish a marker from B and verify
   it arrives in A with B's metadata. Send a direct marker from B to A without a
   subscription and verify acceptance and delivery.
5. Start a third process with a different token (`RHIZOME_RUNTIME_DIR` to an
   empty private directory, no `RHIZOME_TOKEN`). Verify it never lists the
   cluster's sessions and its stderr shows gossip decryption failures only.
6. Disconnect B from Wi-Fi during an active subscription. Verify A shows
   `reconnecting`, then `following` again after reconnection with a visible gap
   notice if B's retention expired.
7. Exit both; verify each device's runtime directory keeps only `token` and
   `registry.lock`.

## Tailscale mode (2026-10-19)

Automated, all on one macOS host:

- `internal/channel/tailscale_test.go`: address-policy table for all three modes
  (CGNAT boundaries `100.63.255.255`, `100.64.0.1`, `100.127.255.254`,
  `100.128.0.1`, the Tailscale ULA, RFC 1918, link-local, public);
  `--lan --advertise-addr 100.x` still rejected with the LAN message; Tailscale
  mode rejecting a private advertise address and refusing to fall back to an
  interface address; Tailscale-only options rejected in other modes; parsing of
  a `tailscale status --json` fixture (self IP, sorted online/offline peers,
  MagicDNS suffix and flag, MagicDNS-off dial hosts, `NeedsLogin`,
  empty `TailscaleIPs`, malformed JSON); startup
  failing without listeners when the CLI is missing, the backend is `Stopped` or
  no IPv4 exists; target selection skipping self, offline devices, devices
  already in the cluster and peers with nothing to dial, by MagicDNS name or by
  IP; a loopback integration with an injected daemon view: beacon held by the
  first process, a second process on the busy port reaching the first purely
  through enumeration (no `--join`, no mDNS), `host` labels, subscribe, announce
  and direct send over pinned TLS, a `--join localhost:<beacon>` seed resolved
  preferring IPv4, the holder exiting and a remaining process re-acquiring the
  beacon with its gossip identity unchanged, and a new device still discovering
  through it; LAN-mode and Tailscale-mode nodes with the same token on one device
  refusing each other's gossip and never listing each other; a wrong-token
  tailnet device logged, retried and never a peer.
- `go vet ./...`, `go test -race ./...` (three consecutive runs),
  `git diff --check`, `python3 scripts/smoke_stdio.py` with and without `--lan`.
  `govulncheck ./...` reports only pre-existing advisories in the unchanged
  `grpc` and `x/net` dependencies.

Single device on a real tailnet (macOS, standalone Tailscale 1.104.1, MagicDNS
on, one other device online but not running Rhizome):

- `rhizome mcp --tailscale`: `ready` with `mode=tailscale`, bind and advertise
  `100.93.74.31`, `magicdns=delta.tail2fdf06.ts.net`, `beacon_port=7946`. `lsof`
  showed the beacon listening on `100.93.74.31:7946` only, coexisting with the
  demo's unauthenticated `127.0.0.1:7946`.
- The first tick enumerated the other device and tried
  `nicos-macbook-pro.tail2fdf06.ts.net:7946`: MagicDNS resolved it to
  `100.114.80.29`, the address passed the policy, and the join was refused
  (`connection refused`), logged once at warn and retried every tick. The
  process kept running.
- A second process on the same device logged `beacon_busy` with
  `beacon_port=ephemeral`, and its `--join delta.tail2fdf06.ts.net:7946`
  produced `seed_joined addr=100.93.74.31:7946` through the first process's
  beacon forwarder.
- When the first process exited, the second logged `beacon_acquired` on its next
  tick with its gossip endpoint unchanged.
- `--lan --advertise-addr 100.93.74.31` still fails with `must be a loopback,
  private or link-local IP`. `RHIZOME_TAILSCALE=1 RHIZOME_GOSSIP_PORT=7950
  RHIZOME_DISCOVERY_INTERVAL=30s` matched the flags. `--tailscale --mdns`
  reported `mdns=true`; without it, `mdns=false`.
- Not reproducible here: a missing CLI (the macOS app bundle is found even with
  an empty `PATH`), a stopped daemon and a logged-out state. Those paths are
  covered by the fixture tests.

Not verified here: two physically separate devices. Run the steps below on
macOS plus one Linux or macOS device and record the result in the PR.

### Two-device Tailscale steps

1. Both devices on one tailnet, `tailscale status` showing each other online.
   On device A, `./rhizome token`; copy the value to device B. Build a checkout
   containing Tailscale mode on both.
2. Start `claude --dangerously-load-development-channels server:rhizome` on each
   with the project `.mcp.json` carrying `RHIZOME_TAILSCALE=1` (and
   `RHIZOME_TOKEN` on B's first run). No `--join`, no `--advertise-addr`. Check
   each stderr `ready` line: `mode=tailscale`, the device's `100.x` address, its
   MagicDNS name and `beacon_port=7946`.
3. Within one discovery interval (20 s), call `list_sessions` on each after the
   first prompt. Verify the other device's session appears with `host` equal to
   its configured hostname override (or OS short hostname), a `100.x` `endpoint`
   and a 64-character `cert_fingerprint`. The joining side's stderr shows
   `seed_joined source=tailscale addr=100.x.y.z:7946`.
4. Subscribe from A to B's exact session ID, publish a marker from B and verify
   it arrives in A with B's metadata. Send a direct marker from B to A without a
   subscription and verify acceptance and delivery.
5. Start a third process on A with A's runtime directory (same token): stderr
   shows `beacon_busy`, and B's `list_sessions` lists it once bound. Exit A's
   first process: the third logs `beacon_acquired` within one interval, and a
   fresh process on B still joins A via `seed_joined`.
6. Start a process with a different token (`RHIZOME_RUNTIME_DIR` to an empty
   private directory, no `RHIZOME_TOKEN`). Verify it logs
   `tailnet_peer_unreachable` with `No installed keys could decrypt`, never lists
   the cluster's sessions, and is never listed by the cluster.
7. If the tailnet ACL is not "allow all", confirm the devices can reach each
   other's beacon port and ephemeral ports (an own-devices rule suffices);
   otherwise the join in step 3 times out and is retried each tick.
8. Run `tailscale down` on B and start `rhizome mcp --tailscale`: it must exit
   non-zero with `tailscale is not running (state "Stopped")` and bind nothing.
   Bring Tailscale back up before step 9.
9. Exit both; verify each device's runtime directory keeps only `token` and
   `registry.lock`.

## Setup configuration wizard

Automated: `setup_config_test.go` and `setup_gateway_test.go` cover legacy and
gateway modes, scripted wizard defaults and existing-value editing, custom
config/runtime paths, noninteractive flags, full-schema and environment isolation,
history-policy preservation, explicit disable, malformed/unknown input, address
and mode conflicts, repeated updates, backup collisions, private permissions and
non-regular destinations. Generated files round-trip through the shared resolver.
The gateway smoke rewrites the paired host's configuration through setup before
restart, checks unchanged effective settings and gateway state bytes, then verifies
reconnection and message delivery without enrollment; revocation remains covered.

Not automated: real terminal prompts or cross-device/Tailscale connectivity.
With a throwaway HOME, verify Enter keeps stored choices, keeping the file leaves
its bytes unchanged, and fresh defaults are local/gateway off. With Tailscale
stopped, setup warns or explains prerequisites, and service startup must fail
closed. Follow the two-device gateway checklist separately. No Cursor smoke is
provided by this repository; no additional integration support is implied.

### Setup service step (#57, 2026-10-21)

Automated with an injected service step, never a real service manager:
`setup_service_test.go` drives `installService` through a fake manager in an
isolated `HOME` (missing service installed and started, running service
accepted without restart, install error with nothing installed, startup
timeout that keeps the definition, cancellation, invalid or missing file never
reaching the manager, exact custom `--config` path and runtime). `TestSetupConfig`
covers the line prompts: create/replace/keep each followed by the
"install and start" question, decline leaving the file byte-identical, accept
calling the installer with the exact path, installer failure after the file was
written, EOF at the question, and `--network` never asking. `tui_test.go`
(`TestWizardKeptConfigOffersService`, `TestWizardServiceOptInAndPartialFailure`)
covers the wizard with kept and `--network` configurations, review text,
decline, accept, cancellation before the step, and partial failure.
`scripts/smoke_tui.py` walks the "Install and start" field on a PTY with the
default answer. The real lifecycle commands are unchanged and remain covered by
`internal/service` command-generation tests and `scripts/smoke_service.py`
(foreground `service run`). Native installation and startup through launchd,
systemd or Task Scheduler were not performed for this change on any platform;
use the manual checklist under "Terminal interfaces" on a disposable account.

## Session history catalog (#25, 2026-10-20)

Automated on macOS:

- `internal/channel/history_test.go`: pre-existing agent log fixtures
  indexed without creating publishers; child transcripts, identity-less files
  and foreign-session records excluded; native titles, explicit overrides and
  fallbacks; timestamps from supported records only; AND filters, exact IDs,
  exact project paths (no subtree), case-folded title substrings, inclusive
  `updated_after`/exclusive `updated_before`, unknown timestamps last and
  excluded by date filters, deterministic ordering with tie-breakers; search
  pagination over a fixed snapshot while the catalog changes, single-use
  forward-only cursors, tampered/mismatched/foreign-process/evicted cursors;
  catalog states (missing, stale, indexing, degraded, corrupt, newer schema,
  foreign scope, world-readable); read content policy (prompts, thinking, tool
  bodies, Rhizome tool calls and results, peer envelopes and reactions across
  page boundaries, sidechains, other-session records, mirrored Codex text,
  reasoning, function outputs), stable event IDs across pages and snapshots,
  user-only sessions, invalid arguments and unknown fields; fixed EOF snapshots
  ignoring later appends, truncation and rename replacement (`SOURCE_CHANGED`),
  symlink refusal, tombstoned deletions and duplicate identities; response and
  record budgets (`event_too_large`, `oversized_record`, `incomplete_tail`)
  with responses under 1 MiB; live-status join (`live`, `recent`,
  `not_discovered`, self not delivery-capable; shared derivation and `--debug`
  substatus in `status_test.go`) and untouched live state;
  registration in both agent modes with schema/decoder agreement, Codex
  `CALLER_UNVERIFIED`; incremental checkpoints, replacement re-indexing, visible
  limits and path-free health output.
- Recent-only advertisements (`internal/channel/recent_test.go`,
  `internal/config/sessions_test.go`): `sessions.recent_window` default,
  overrides, zero, invalid units, negatives and overflow; the inclusive cutoff,
  unknown and future timestamps with the five-minute skew tolerance, expiry
  under a fixed clock, bounded newest-first selection with an `omitted` count;
  `list_sessions` with a stopped service, live → recent → live transitions
  without duplicates, idle live sessions beyond the window, `send` refused
  before queueing, window `0s`; and across three isolated gateways: host
  stamping, send/file refusal, no forwarding, withdrawal while the owner's
  observer is stopped, peers advertising none, revocation and a stopped
  gateway. The service and gateway smokes exercise the same through real
  processes. Cross-device behavior remains covered by the manual checklists.
- `task build`, `task check` (`go vet ./...`, `go test -race ./...`),
  `git diff --check`, `python3 scripts/smoke_stdio.py` with and without
  `--lan`, and `python3 scripts/smoke_service.py`, which now writes logs for
  each supported agent type before the observer starts, checks
  `CATALOG_UNAVAILABLE` first, then searches and reads them from a separate MCP
  process, pages with cursors,
  verifies the stale catalog while the observer is stopped, and asserts that no
  historical text reached any channel notification.

Not exercised here: real coding agent clients calling the new tools, native
service installation on Linux/Windows with a large pre-existing log tree, and
any cross-device scenario (history was local by design at that point; see the
paired-host section below).

## Secret redaction (#84, 2026-10-26)

Automated on macOS (`internal/channel/redact_test.go`):

- Filter table: no-finding passthrough is byte-identical; GitHub, AWS, Slack,
  generic API key (value only, assignment context kept) and multi-line private
  key rules; two rules in one text; repeated secrets; empty input.
- Fail closed: scanner construction error, scan error, 50 ms timeout against a
  stalled scanner, findings outside or not placeable in the text, and empty
  spans all return `[REDACTED:unscanned]` within the budget; overlapping spans
  merge into one marker.
- 32 goroutines sharing the scanner under `-race`; a 2 MiB record with ~23k
  findings redacts in about 0.6 s and a benign 2 MiB record is unchanged.
- `Node.activity` redacts before retention and clipping (a secret straddling
  the 7 KiB clip leaves no fragment) and counts `Redacted`/`RedactionFailures`;
  a broken scanner masks the event.
- `send` and `publish` deliver secret-like text verbatim even with a scanner
  that fails everything.
- Live collection for Claude transcripts, Codex rollouts and OpenCode frames
  emits the marker, and `read_session_history` for all three agents (local and
  through the paired-host access path) returns the identical text with
  `redacted: true`; unredacted events carry no flag.
- Benchmark: about 22 µs per benign 7 KiB record and 47 µs with a secret.

Not covered: real credentials (fixtures use synthetic values; Betterleaks
allowlists documented example keys), encoded secrets (not decoded by default),
and a live peer subscribed across machines (the stdio and gateway smokes cover
the transport; the filter runs before the text enters it).

## Paired-host history sharing (#44)

Automated on macOS, same machine only:

- `internal/config/history_test.go`: omitted defaults (sharing off, 30-day
  default window), inheritance, the global master gate over agent overrides,
  per-agent enable/disable and lookback combinations, `all`, the 106,751-day
  overflow bound, malformed values (zero, sign, leading zeros, units, empty,
  wrong types), misspelled and unsupported-agent tables, UTC cutoffs, the policy
  fingerprint, and effective `rhizome config` output.
- `internal/channel/history_peer_test.go`: deterministic-clock policy for
  15d/30d/`all`, the inclusive cutoff at the exact boundary and 1 ns before it,
  unknown `last_activity_at`, mixed old/new and undated events, date filters
  that cannot widen access, direct IDs outside the window, hidden sessions that
  look absent, `POLICY_DENIED` independent of existence, pages after the clock
  advances (search and read), cursor rejection across hosts, grants, policy
  changes and local/remote, per-host snapshot bounds, host qualification
  overriding peer claims, and a three-gateway test with independent keys,
  tokens and catalogs: default denial, remote search and paginated reads of
  inactive sessions, host/session ID collisions, unknown/self/second-hop hosts,
  unsupported peers, restart (`CURSOR_EXPIRED`), revocation, and the MCP relay.
- `python3 scripts/smoke_gateway.py`: real services and MCP processes. Host A
  opts in with Claude 15d and Codex disabled. B searches and reads A's
  pre-existing log with the old record withheld, gets `POLICY_DENIED` for Codex,
  A gets `REMOTE_HISTORY_DISABLED` from B, C cannot reach A through B, and no
  history text appears in channel notifications.

Manual two-device checklist (not automated):

1. Pair two machines in LAN or Tailscale gateway mode and confirm `rhizome
   peers list` shows both connected.
2. On the serving host, leave `[gateway.history]` absent. Searching with
   `host_id` from the other host must return `REMOTE_HISTORY_DISABLED`.
3. Enable it with `[claude.peer_history] lookback = "15d"` and
   `[codex.peer_history] enabled = false`, check `rhizome config`, and restart
   the service. Search and read an ended Claude session. Older events must
   be counted as `outside_lookback`, and Codex reads must be `POLICY_DENIED`.
4. Start paging with `limit: 1`, narrow the policy, restart the serving
   service, and confirm that the cursor is `CURSOR_EXPIRED`.
5. Revoke the requesting host and confirm that the next page is
   `PEER_UNAVAILABLE`; local history on both hosts keeps working.

## Hostname override (#29)

Automated coverage includes setup prompting and retry, explicitly clearing an existing
value with backup, noninteractive `--hostname`, resolver source reporting,
64-byte UTF-8 and control-character validation, rune-safe gossip trimming,
authenticated identity probes in local/LAN/Tailscale modes for both agent types,
and observer propagation. The stdio smokes list peers with the same custom host
label and exercise exact-session routing. Live two-device checks and real
terminal prompts remain manual; the checks above describe how to exercise them.

## Paired-host gateway

Run `python3 scripts/smoke_gateway.py` after `task build` for three isolated
services with independent tokens, TLS invitation redemption, bidirectional
send/subscribe, restart, non-transitivity, and revocation. Go tests cover
logged link stop reasons and the short-link dial backoff. This is one-machine
coverage. See [gateway verification](gateway.md#verification) for the manual
two-device LAN/Tailscale checklist and known compatibility limits.

## Setup agent installation (#55)

Automated in `setup_agents_test.go`: the installed hook groups equal
`.claude/settings.json` and `examples/codex/hooks.json`; server entries for
default and custom homes, explicit configuration (missing file refused) and a
resolvable Codex executable; merges that keep unrelated servers, hooks, mixed
hook groups, comments and numbers while removing stale Rhizome entries; backups
with the same mode, symlinked files edited through the link, malformed JSON/TOML
and inline Codex tables rejected without replacement, read-only directories
leaving the original intact; and the real entry point in a subprocess with an
isolated home containing spaces (`--list` read-only, `--network` alone
config-only, one agent selected leaves the other untouched, `--agents all` with
an explicit configuration, idempotent repeat). Live validation is not automated:
with a throwaway `HOME`, run `rhizome setup --agents all --network local`, start
Claude Code with the channel flag and Codex from another directory, confirm each
shows the `rhizome` server and its hooks, and that `list_sessions` reports
`BOUND` after the first prompt. Cursor is not supported on this base, so no
Cursor check applies.

## Collaboration skill device origin (#49)

The skill is prose, so no automated check proves how an agent applies it.
`setup_test.go` (`TestInstallSkill*`) verifies that `rhizome setup` installs the
embedded skill byte-for-byte, reports `unchanged` on a repeat run and `updated`
after a change, and leaves symlinks alone. For the behavior itself, review an
agent's answers against [collab-skill-evals.md](collab-skill-evals.md): local
worktrees on one device, label mismatches and collisions, gateway proxies,
paired-host history errors, direct LAN/Tailscale peers, missing metadata,
conflicting history, ambiguous IDs, stale catalogs, and scope corrections.

## OpenCode history (2026-10-21)

OpenCode sessions are cataloged from OpenCode's SQLite database (see
[history sources](history.md#sources-identity-titles)). Verified on macOS against
OpenCode 2.0.32:

- `task build`, `task check` (race) and `git diff --check` pass apart from
  `TestNotarizeSubmissionRequiresAccepted` in `internal/release`, which also fails
  on a clean `main` in this environment. Unit tests build a fixture database with
  the 2.x tables (WAL mode) and cover catalog rows and native titles, child
  exclusion, content policy (reasoning, prompts, tool inputs/outputs, `execute`
  and `rhizome_*` calls, peer-only turns), pagination and the seq snapshot
  boundary, tombstones after deletion, rename and activity refresh, an
  unsupported schema, a removed database, symlink and path confinement,
  oversized and malformed rows, the lookback policy on events, the config keys
  and the observer's event filter.
- `python3 scripts/smoke_service.py` PASS with a real observer and MCP process
  over a database created before the service started, including reads while the
  service is stopped.
- A read-only pass over the real local database (11 sessions, written by a
  running `opencode serve`) cataloged every session and read each one to
  completion with complete coverage and no gaps; only counts and event kinds were
  inspected, never content.

Not verified: OpenCode versions other than 2.0.32 (the v2 schema is new and may
change; a mismatch is reported as `unsupported`), Windows, sharing OpenCode
history with a paired host over the gateway (the policy and bounds are unit
tested through the shared access code), and databases large enough to stress the
8 MiB scan bound or the 4,096-session limit.

## OpenCode 2.0.32 (2026-10-19)

- Spike findings (full detail in
  [the design spec](superpowers/specs/2026-10-19-opencode-support-design.md)):
  MCP `tools/call` carries `_meta["ai.opencode/sessionID"]` set by OpenCode, and
  forged arguments do not change it; subagent calls carry the child session ID, and
  `ctx.session.get` on a child returns `parentID`; `session.deleted` has
  `data.sessionID`, fires for children first and reached subscribers twice with two
  locations loaded (so `forget` is idempotent); OpenCode runs one MCP process and
  one plugin instance per project directory, and a `git init` directory without
  commits has project id `global`, so pairing uses the directory; models reach
  MCP tools through `execute`, which fires tool hooks for the inner tool by its
  real name; `ctx.session.synthetic` with `delivery: "queue"` wakes an idle session.
- `task build`, `task check`, `task test-plugin` and `git diff --check` pass.
- `python3 scripts/smoke_stdio.py` PASS, including OpenCode bridge identity,
  activity and delivery with a fake plugin.
- `python3 scripts/smoke_opencode.py --live --model opencode-go/deepseek-v4.1-flash`
  PASS ("PASS: OpenCode v2.0.32 live binding, activity to a Claude peer and
  synthetic delivery"); three passing runs. The Claude endpoint is an MCP protocol
  harness, not an interactive Claude model.
- Instance reload restore (2026-10-20): `opencode reload --server <url>` on the
  disposable server reloads the location like the service's periodic reloads (fresh
  plugin `setup`, new Rhizome process; the old one exits about 2 s later). The live
  smoke now binds, reloads, waits for the session on a new Rhizome node without a
  prompt, and confirms a `send` reaches the session export: PASS ("... synthetic
  delivery, and restore after an instance reload"), two runs. The first attempts
  exposed that the exiting process unlinked the successor's socket (same path),
  fixed in the same change. Earlier smoke runs inherited the caller's `PWD`, so
  their sessions lived in the caller's directory location; the smoke now sets
  `PWD` to the temp repository.
- Known CLI caveat: run `opencode run` with stdin closed from non-interactive
  parents (confirmed cause of hangs); hangs were also seen with message text
  containing `ses_...`, cause unconfirmed.

After rebasing onto central configuration, LAN/Tailscale modes, the service,
history, host pairing and file transfer (2026-10-20, macOS): `task build`,
`task check` (race), `task test-plugin`, `python3 scripts/smoke_stdio.py` and
`--lan` (the OpenCode process joins the Claude peer over pinned TLS from its own
runtime directory), `scripts/smoke_service.py`, `scripts/smoke_gateway.py`,
`git diff --check` and the live smoke above with an isolated `RHIZOME_CONFIG`
all pass. Not exercised: Tailscale mode, a paired remote host or file transfer
with a live OpenCode session (unit tests cover gateway acceptance of
`opencode` identities and the OpenCode tool set only).

TUI toast for peer deliveries (2026-10-21): `plugins/opencode/rhizome-tui.js`
is unit-tested (`task test-plugin`: toast for an enqueued Rhizome synthetic item;
none for user prompts, other synthetic items, malformed envelopes, unknown
sessions or other directories; once per inbox item; control-character stripping
and truncation; throwing APIs contained) and `setup_test.go` covers installing it.
In a disposable OpenCode 2.0.32 (isolated `XDG_*` directories, `opencode serve`
plus a TUI attached with `--server` in a private tmux), the TUI log showed
`setup` of `rhizome.tui` with no plugin errors; a synthetic item with Rhizome's
envelope posted through `POST /api/session/{id}/synthetic` rendered the toast
"Rhizome · direct from 4f3a9c1e" with the stripped, single-line excerpt and an
**Open** action, and a plain synthetic item showed nothing. Not yet checked: a
real Rhizome delivery into the user's own TUI after `rhizome setup`.

Manual checklist (not yet run): in an interactive OpenCode TUI session, submit a
prompt, run `list_sessions` and confirm `BOUND`, `collection_state: watching` and
the session title; then subscribe from a Claude session and confirm activity and
delivery in both directions, and that each delivery to the OpenCode session shows
a "Rhizome · <kind> from <id>" toast. Interactive TUI parity with `run`/`serve` is
otherwise unverified.

## Terminal interfaces (#48, 2026-10-21)

`python3 scripts/smoke_tui.py` passed on macOS using isolated Unix PTYs and a
fake local gateway. It checks setup back navigation, cancellation without writes,
review/apply and 0600 config; service stop confirmation/decline and refreshed
health; read-only status ignoring mutation keys; peers empty/unavailable states,
exact host identity, refresh, plain JSON, and terminal-control sanitization;
24-column/8-row resize, `NO_COLOR`, interrupted outstanding gateway requests,
redirected explicit-TUI rejection, and restored terminal modes/alternate screen.
BSD's transient `PENDIN` bookkeeping flag is excluded from mode comparison.
Model tests cover mode-specific answers, defaults, inline validation, keep/backup,
selected skills only, opt-in install-and-start dispatch after created, replaced
or kept configuration, partial failure, duplicate actions, and worker cancellation. `service run` remains covered by the service smoke.

The UI dependency is pinned to stable Bubble Tea v1.3.10; the only additional
direct helper is its existing `x/ansi` dependency for terminal-cell wrapping.
The full vulnerability scan reports 18 existing reachable advisories in the Go
standard library, `golang.org/x/net`, and gRPC. A baseline scan of the pre-change
commit with the same toolchain found the same advisory/module/version set;
this change adds no reachable findings. This is not a clean vulnerability scan.

Manual terminal checklist for releases (not a claim of native lifecycle checks):

- Run setup in an ordinary terminal and a narrow window. Select/deselect agents,
  enter invalid values, go back, switch LAN/Tailscale/local, review, and cancel.
  Verify no files changed. Apply in a disposable home and inspect keys/permissions.
- On a disposable service installation, exercise install/start/stop/uninstall,
  decline and accept confirmations, refresh, and interrupt a pending action.
  Verify completed actions remain accurately reported and the shell is usable.
- Open status and peers views with real paired hosts, navigate/refresh/resize,
  stop the service, and check visible errors. Exit with q, Esc, and Ctrl-C.
- Repeat on Linux and Windows, including a no-color terminal. Native service
  mutation and human visual checks on those platforms were not performed here;
  the recorded terminal run is automated macOS PTY coverage, not cross-platform
  visual or native lifecycle verification.

## Release pipeline (#46, 2026-10-21)

Automated and local evidence, macOS 26 arm64 with GoReleaser 2.17.1:

- `go test -race ./internal/release/...` covers SemVer acceptance and rejection
  (optional `v`, prerelease and build metadata, leading zeros, malformed input,
  oversized components), prerelease classification, installer-version mapping,
  the asset matrix, manifest completeness, tamper, duplicate, stray-file and
  missing-asset failures, and bounded archive extraction and version smoke. It
  also drives `scripts/release` with stubbed Apple tools: every non-`Accepted`
  notarization outcome (including `Invalid` with exit 0, malformed JSON, a
  missing id and a nonzero exit) fails; missing notary credentials,
  identities or macOS fail before any signing tool runs; ad-hoc signatures and
  signatures without hardened runtime or a timestamp fail; and a killed binary
  or wrong version fails the execution gate. On macOS a real unsigned package
  is built and its payload verified, and a mismatched payload is rejected.
  These stubs do **not** validate real signing or notarization.
- `rhizome --version` tests run with an invalid config file and a missing
  `RHIZOME_CONFIG` and confirm that nothing is written.
- `goreleaser check`, `actionlint` and `shellcheck` pass.
- Local snapshot and tag-mode (`1.4.0-rc.2+b.7`) GoReleaser runs produced all
  seven assets. The ad-hoc universal binary verified and ran natively and as
  the x86_64 slice under Rosetta. The package payload matched the archive
  binary and ran. Checksums and the native darwin smoke passed. Linux amd64
  and arm64 archives ran in Alpine containers.
- `GOOS=windows` and `GOOS=linux` `go vet ./...` pass for amd64 and arm64.

- The first pull request snapshot run (Actions run 39646167543, 2026-10-21)
  passed every job. It ran with no environment and no secrets, and the macOS
  build hook executed the ad-hoc binary natively and its x86_64 slice. Package
  payload verification passed. Each native smoke host verified the manifest and
  ran its extracted archive, which reported the expected snapshot version and
  commit: `ubuntu-latest` (x86_64), `ubuntu-24.04-arm` (aarch64),
  `windows-latest`, `windows-11-arm`, `macos-15` (arm64) and `macos-15-intel`
  (x86_64). The macOS hosts also re-verified the package payload. Publish was
  skipped. On `windows-11-arm`, Git Bash's `uname` reports x86_64 because MSYS
  is emulated; the arm64 archive is what was executed.

Pending (requires the `release` environment and a pushed tag): real Developer
ID signing, notarization `Accepted`, stapling, Gatekeeper assessment, draft
staging and publication, and release downloads. Run a `v0.1.0-rc.1`
prerelease and record the results here
([releasing.md](releasing.md#validating-before-a-stable-release)).
