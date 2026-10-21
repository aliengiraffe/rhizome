# Invite and join paired hosts

Host pairing connects two per-user Rhizome services without sharing or replacing
local cluster tokens. Each service bridges its local MCP/observer sessions over
one bidirectional TLS connection. Trust is pairwise: A–B and B–C do not expose A
to C. Agent logs, configuration, and administrative commands are never available
to a remote host. Session history is served only when the owner opts in through
[`[gateway.history]`](#session-history-sharing); pairing alone grants no history access.

## Configure and run

Upgrade and restart both services and their local MCP processes. Older processes
without gateway capability are not exported; older clients ignore gateway proxy
advertisements. Keep MCP networking in its default local mode. Legacy
shared-token LAN/Tailscale gossip remains available separately, but cannot be
combined with gateway pairing in the same configuration. Bound OpenCode sessions
(`agent_type: "opencode"`) are exported like Claude and Codex sessions, but only
over links whose websocket handshake negotiated the `rhizome.gateway.opencode.v1`
subprotocol. A paired host whose build predates OpenCode support selects none, so
the link stays up and that host sees only Claude and Codex sessions; upgrade both
hosts to share OpenCode sessions. Without this negotiation such a host would
reject the snapshot and drop the link.

On each host, put the persistent settings in its existing TOML configuration:

```toml
network.mode = "local"
gateway.mode = "lan"
gateway.listen = "192.168.1.10:7947"
# Optional when the reachable endpoint differs from the listen endpoint:
# gateway.advertise = "192.168.1.10:7947"
```

Use each laptop's own IP. Alternatively, use `gateway.mode = "tailscale"` and omit
addresses to use the running Tailscale daemon's IPv4 with port 7947. Both hosts
must use the same gateway mode and already have network access to each other.
Tailscale mode fails if its CLI/daemon/IP is unavailable, even with explicit
addresses. Pairing does not grant tailnet membership or bypass its ACLs.

Then start or restart the existing service with the selected configuration, or
run it in the foreground:

```sh
rhizome service run --config /absolute/path/config.toml
```

Service installation is still explicit; `invite` and `join` do not install or
start it. Networking defaults to off and commands fail when the gateway is not
running. LAN mode with no listen address selects a private interface; detection
failure is an error. Addresses must be concrete IP:port values (not wildcard
binds, DNS names, or port zero). Public Internet addresses are unsupported.

| Configuration | Environment | Foreground service override |
| --- | --- | --- |
| `gateway.mode` (`off`, `lan`, `tailscale`) | `RHIZOME_GATEWAY_MODE` | `--gateway-mode` |
| `gateway.listen` | `RHIZOME_GATEWAY_LISTEN` | `--gateway-listen` |
| `gateway.advertise` | `RHIZOME_GATEWAY_ADVERTISE` | `--gateway-advertise` |

Resolution uses flag > environment > file > default. Installed services pin the
configuration path; installation rejects overrides not present in that file.
Use `rhizome config` to inspect effective values and their sources.

## Pair and manage

On the inviting host:

```sh
rhizome invite --expires 10m
# Prints: rhizome join rhz1_...
```

Share the complete command privately. On the other host:

```sh
rhizome join rhz1_...
# Or, to avoid putting the bearer invitation in shell history:
rhizome join --stdin
```

For `--stdin`, supply the invitation code and end stdin. Expiry is between one
minute and 24 hours; the default is ten minutes. Flags, including `--config` and
`--runtime-dir`, precede positional arguments. A successful join reports the
cryptographic host ID and an established connection; session discovery follows
asynchronously. A pairing that was persisted before a connection failure retries
automatically. Retry the same invitation to recover an interrupted enrollment.

```sh
rhizome peers list
rhizome peers invitations
rhizome peers revoke-invite <invitation-id>
rhizome peers revoke <host-id>
```

Lists never reveal invitation secrets. Failed enrollment can leave a visible
pending peer; revoke that exact host ID to remove it. Revoking an invitation
blocks its redemption/recovery, but does not revoke an already paired host.
Revoking a host closes its connection, removes its proxy routes, fences locally
queued events from that trust grant, and prevents reconnect. Fresh explicit
pairing is required. Old invitations used by that host cannot undo revocation.
Subscriptions to a revoked host stop and require a new explicit subscribe after
re-pairing. Delivery already in flight can finish; content handed to an agent or
received by a peer cannot be recalled.

Paired hosts currently see **all bound local sessions from upgraded processes**.
Session privacy is tracked separately in [issue #37](https://github.com/aliengiraffe/rhizome/issues/37)
and is not implemented by this feature. The export/access predicate in
`internal/channel/gateway_link.go` is its integration point. Do not interpret
pairing as selective session sharing.

## Protocol and trust boundaries

An invitation is a `rhz1_` prefix and base64url JSON containing version 1, network
mode, endpoint, host public-key fingerprint, invitation ID, and a random 256-bit
secret. The endpoint locates the host; the pinned fingerprint authenticates it.
It is not a short lookup code and needs no rendezvous server. An unreachable or
changed endpoint requires a fresh invitation or restored reachability.

The gateway uses TLS 1.3 with an Ed25519 host key. The joiner pins the server's
SPKI fingerprint before sending the secret; TLS client authentication proves
possession of the joining host key. `/v1/enroll` admits unknown client identities
only with a valid invitation. `/v1/connect` requires persisted peer authorization
and upgrades to a WebSocket carrying bounded JSON frames. No local bearer token
crosses that connection. TLS session resumption is disabled on the server so it
cannot bypass revocation checks.

Admission atomically binds an invitation to one host. The same host may finish
an interrupted admission before expiry, while another host cannot reuse it.
The joiner persists a pending host and an invitation receipt before enrollment,
then activates trust after validating the server response. Join receipts and
revocation tombstones prevent old invitation retries from undoing local
revocation, independently of the other host's clock. An inviter's revocation
also removes its redeemed invitation records. The stable host key survives
service restart; a new certificate is generated from it on startup (one-year
validity). Restart before certificate expiry. Replacing the identity key
requires explicit re-pairing.

The private `<runtime>/gateway/` directory contains versioned `state.json`
(host key, peer grants, hashed invitation secrets, receipts/tombstones), an OS
service lock, and ephemeral `control.json`. State uses private permissions and
atomic replacement. The CLI uses a private random control credential over
loopback; remote listeners expose no administration. This is not a boundary
against another program running as the same OS user.

Both hosts attempt reconnect with bounded exponential backoff and jitter
(2–32 s). A connection that ends within five seconds counts as a failure
whichever host dialed it, so a connect-then-drop loop backs off instead of
rebuilding every proxy each second; a longer-lived connection resets the
backoff. The service logs each connection's end with a stable `reason` (for
example `stale_snapshot`, `invalid_snapshot`, `read_error`, `replaced`,
`shutdown`), a host ID prefix, uptime and the backoff, and logs rejected
connections (`duplicate`, `pending`, `untrusted`); these lines carry no session
data. A single surviving connection carries both directions; if both connect, the
lexicographically smaller host ID's outgoing connection wins. Disconnects cancel
requests and remove proxies. Snapshots refresh each second; ten seconds without
a snapshot invalidates the connection, even if other frames continue arriving.

The service maps each advertised remote identity to a local protocol-2 proxy
with `gateway_host`. Only original local identities with `gateway_version = 1`
are exported; remote endpoints are stripped and never dialed. Requests must
match an advertised source owned by the authenticated connection and an exact,
unambiguous local target. Cross-host session-ID collisions fail closed, including
observer/direct collisions. Remote observation-only sessions remain unable to
receive messages. Proxy events carry a local trust grant that is checked again
before delivery; a new grant after re-pairing cannot revive old queued content.
Existing normalizers, suppression, deduplication, and live replay rules apply.

Bounds: 16 peers, 128 issued invitations, 128 combined join receipts/revocation
tombstones, 64 advertised sessions per host, 128 active proxies per gateway,
64 pending and 64 serving requests per connection, 64 queued frames per connection
and per request, and 256 KiB per frame/state read. Enrollment is limited to four
requests/second with a burst of eight; network sockets and administrative
requests are bounded separately. Saturated queues close the connection rather
than growing without bound. At most 64 eligible sessions are exported; larger
local inventories need a future pagination contract. A local probe pass has a
three-second budget and may temporarily withdraw unavailable identities.

## Verification

`go test -race ./internal/channel -run TestGateway` covers pairing, identity
bounds, token preservation, bidirectional routing, activity streams, restart,
non-transitivity, collisions, atomic redemption, expiry, and revocation.
`python3 scripts/smoke_gateway.py` runs three isolated observer/MCP process pairs
on loopback with distinct tokens. It exercises the CLI, actual TLS connections,
normalization, service restart, and non-transitive discovery.

`go test -race ./internal/channel -run PeerHistory` covers history sharing
across three isolated gateways: default denial, per-agent policy, cursor binding
across hosts, restart and revocation, second-hop and unsupported peers.

These do not establish cross-device or live-agent coverage. For a manual check,
configure two machines in LAN or Tailscale mode, pair them, list their verified
sessions, explicitly send/subscribe, restart a service, and revoke the peer.
Verify private addressing, unchanged local token files, reconnect, withdrawal,
and failed post-revocation sends. Follow [Codex live smoke prerequisites](codex.md)
when validating actual Codex delivery; synthetic MCP tests do not substitute for
that opt-in workflow.

## Session history sharing

A host owner can let paired hosts search and read its session-history catalog
through `search_sessions`/`read_session_history` with an exact `host_id`. This
is off unless the serving host's file sets `gateway.history.enabled = true`. A
global `lookback` (default `30d`, or `all`) and per-source-agent
`[claude.peer_history]`/`[codex.peer_history]` overrides limit what is shared.
The serving service reads the policy at startup, so restart it after a change;
the restart also expires every outstanding remote cursor.

Requests use the pinned-TLS `/v1/history` endpoint. They are accepted only from
currently trusted, connected peers, are never forwarded, and are authorized on
the serving host before catalog enumeration and on every page. The serving
gateway reads the observer's catalog and native logs with the same confinement
and content filters as local reads, and it never exposes paths or the admin API.
It admits 4 concurrent history requests, 2 per peer. Older gateways answer
`REMOTE_HISTORY_UNSUPPORTED`. Messaging and local history are unaffected. Exact
semantics, outputs and error codes are in
[paired-host history](history.md#paired-host-history-opt-in).

## File attachments

Paired hosts can offer files to exact live sessions through the authenticated
`/v1/files` TLS endpoint. Older gateways return an unsupported-endpoint error;
normal messaging remains compatible. File bytes do not enter WebSocket frames,
gossip, or activity notifications. See [file transfers](files.md).

## Paired-host terminal view

`rhizome peers list --tui` opens a read-only Bubble Tea view on terminal
stdin/stdout with `TERM` other than `dumb`. Up/down (or j/k) select hosts;
PgUp/PgDn scroll details, `r` refreshes, and `q`/Esc/Ctrl+C exits. The view
shows the exact `host_id`, `endpoint`, `connected`, and `pending` fields from
the local gateway API. These are paired hosts, not discovered agent sessions.
Display text is sanitized to prevent terminal control injection. Empty results,
unavailable service, and request errors are shown explicitly. Refresh is manual,
bounded, and cancellable; there are no pairing or revocation actions.

Plain `rhizome peers list` continues to print JSON, including on terminals.
Explicit `--tui` fails for redirected input/output or `TERM=dumb`. No tokens or
invitation secrets are fetched by this view.
