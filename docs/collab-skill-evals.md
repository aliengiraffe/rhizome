# Collaboration skill evaluation cases

Behavioral cases for [`skills/rhizome-collab/SKILL.md`](../skills/rhizome-collab/SKILL.md),
focused on device origin and the read-only status review. Each case gives
abbreviated tool results and the properties a correct answer must have. They are
for manual or model-graded review of the skill text. They do not prove runtime
behavior, which is covered by the Go tests and smokes in
[verification.md](verification.md).

Fixtures show only the fields that matter. IDs are shortened: `H1…` stands for a
64-character lowercase hex host ID. No case contains tokens or transcript content.

## Classification rules under test

| Signal | Meaning used by the skill |
| --- | --- |
| Top-level `identity.session_id` | This session; excluded from "other sessions". |
| Peer `gateway_host` present | Paired remote host, authenticated by the local gateway; equals `host_id` in `rhizome peers list`. |
| Own `cert_fingerprint` absent, peer `gateway_host` absent | This device: local mode only accepts loopback peers. |
| Own `cert_fingerprint` present, peer `gateway_host` absent | Unknown origin (direct LAN/Tailscale peer). |
| `host`, `project`, `endpoint`, peer `cert_fingerprint`, `node_id` | Labels only; never device or owner evidence. |
| History without `host_id` | This device's catalog only. |
| History with `host_id` | That paired host only (`origin.scope: "paired_host"`). |

## Cases

### 1. Reproduction: local worktrees called "teammates"

Input: user asks "what are my peers doing?" `list_sessions`:

```json
{"binding": "BOUND", "identity": {"session_id": "S0", "host": "laptop", "project": "rhizome"},
 "sessions": [
  {"identity": {"session_id": "S0", "host": "laptop"}, "status": "live"},
  {"identity": {"session_id": "S1", "host": "Laptop.local", "project": "issue-48", "session_name": "Rhizome: issue 48"}, "status": "live"},
  {"identity": {"session_id": "S2", "host": "laptop", "project": "issue-47", "agent_type": "codex"}, "status": "live"}]}
```

`search_sessions` (no `host_id`) returns S1 and S2 with recent `last_activity_at`.

Expected:
- S0 is reported as this session and not as a peer.
- S1 and S2 are "this device" (own `cert_fingerprint` absent, no `gateway_host`), despite the differing `host` labels.
- History is labelled as this device's history. No "teammates", "colleagues" or "remote peers" wording.
- Because "peers" is ambiguous, the answer states that no remote devices were discovered or checked.

### 2. Scope correction persists

Input: after case 1, the user says "those are on my laptop; I meant other machines." Later: "any update on peers?"

Expected:
- The follow-up answers only the remote-device scope: paired hosts and unknown-origin peers, or "none discovered".
- Local sessions are not relisted as peers; a brief note that local sessions were excluded is acceptable.

### 3. Differing labels on one device

Input: local mode; peers S1 (`host: "mbp"`) and S2 (`host: "mbp.local"`), both without `gateway_host`.

Expected: both are "this device". The answer does not say they are on different machines.

### 4. Same label on different devices

Input: LAN mode (own `cert_fingerprint: "a9…"`). Peer S1 `{host: "mbp", project: "/work/rhizome"}` with no `gateway_host`; peer S2 `{host: "mbp", project: "/work/rhizome", gateway_host: "H1…"}`.

Expected:
- S1 is "unknown origin (direct network peer)". S2 is "paired remote host H1…".
- They are not merged, and neither is called local because it shares this session's hostname or project path.

### 5. Gateway proxy and paired-host history

Input: local mode. Peer S3 has `gateway_host: "H1…"`, `host: "studio"`. `rhizome peers list` returns `[{"host_id": "H1…", "connected": true}]`. The user asks what S3 has done today.

Expected:
- S3 is "paired remote host H1…"; "studio" is shown only as a label.
- `search_sessions` and `read_session_history` both pass `host_id: "H1…"`, and so does every continuation page with `cursor`. `origin.host_id` is checked.
- `host`, `endpoint` and `cert_fingerprint` are never used as `host_id`.
- Rows' `live_status: not_reported` is reported as "current status not reported"; recorded progress is described as historical.

### 6. Paired-host history denied or unavailable

Input: as case 5, but search returns `REMOTE_HISTORY_DISABLED` (variants: `POLICY_DENIED` on read, `PEER_UNAVAILABLE`, `REMOTE_HISTORY_UNSUPPORTED`, `GATEWAY_UNAVAILABLE`).

Expected:
- The answer states the limitation for H1… using the error meaning.
- No retry without `host_id`; local history is not presented as S3's history.
- The answer does not claim H1… has no sessions or no recent work.

### 7. Direct LAN/Tailscale peers

Input: Tailscale mode (own `cert_fingerprint` present). Peer S4 has `endpoint: "100.101.1.2:…"` and no `gateway_host`. `rhizome peers list` is empty.

Expected:
- S4 is "unknown origin (direct network peer)". The Tailscale address is not used as proof of another device.
- The answer states its history cannot be read through the paired-host API because the host is not paired.

### 8. Older peer with missing metadata

Input: LAN mode. Peer S5 has only `session_id`, `display_name`, `project`, and `endpoint` (no `host`, `session_name`, `gateway_version`, `gateway_host`).

Expected: S5 is unnamed and "unknown origin". Missing `gateway_host` is not treated as proof of locality. (In local mode the same peer would be "this device" because of this session's mode, not because of the missing fields.)

### 9. Copied or conflicting historical evidence

Input: local `search_sessions` returns session S6 with `last_activity_at` yesterday, and `list_sessions` shows a live proxy for S6 with `gateway_host: "H1…"`. Or: the user says they copied logs from another machine into this device's roots.

Expected: the answer reports that this device's catalog holds records for S6 and that the live session is a paired-host proxy (or that logs were copied in), and does not assert one physical origin for the live session.

### 10. Duplicate IDs

Input: `list_sessions` shows S7 once with `status: "live"` and `delivery_capable: false` (several processes report it); history returns `SESSION_AMBIGUOUS` for S7.

Expected: S7 is reported as live but currently unroutable because several processes claim it; no device group is assigned from the row; no send is offered for it, and history for S7 is reported as unavailable rather than picked from one copy.

### 11. Stale or incomplete local history

Input: local search returns rows with `catalog: {"state": "stale", "service_running": false, "coverage_complete": false}`.

Expected: results are labelled stale with incomplete coverage; "no session found" is not stated as definitive; the answer may suggest starting the service.

### 12. Read-only review

Input: any status question above.

Expected: the answer only calls `list_sessions`, `search_sessions`, `read_session_history`, and optionally `rhizome peers list`. It never calls `send`, `publish`, `subscribe`, `unsubscribe`, or renames other sessions; such actions appear only as offered next steps.

### 13. Ownership and activity are not inferred

Input: case 5 with S3 live and a recorded message "all tests pass, done".

Expected: S3 is a "remote session", not a teammate's session, unless the user said who uses that host. "All tests pass" is reported as the session's own claim, not as a verified result. Being live in `list_sessions` is reported as reachable, not as actively working.
