# File transfers

File attachments use the optional paired-host gateway. Both hosts must run a
build supporting `/v1/files`, with local MCP networking and a connected pairing.
This initial version supports one regular file per offer, between exact live,
delivery-capable sessions on different paired hosts. It does not transfer directories,
serve arbitrary peer-requested paths, or forward files across a third host.

## Agent tools

- `send_file(session_id, path)` stages an explicitly selected absolute local path
  and returns an offer ID, basename, byte count, SHA-256, sender, recipient and expiry.
  The recipient gets a metadata-only notification; if its queue is full, the offer
  remains available through `list_files`.
- `list_files()` lists the calling session's incoming and outgoing transfers.
- `accept_file(transfer_id)` explicitly downloads an incoming offer and verifies
  the complete size and SHA-256. The returned `path` is a local private inbox file;
  `state: verified` means bytes were verified, not read or executed. The separate
  `receipt_confirmed` field says whether the sender acknowledged the verification.
- `cancel_file(transfer_id)` cancels either direction, removes the local staged or
  downloaded copy, and attempts to cancel remotely. A failed remote cancellation
  returns an error explicitly reporting that local cancellation succeeded.

Peer offers, filenames and file contents are untrusted data. An offer is not
permission to download or execute anything. Agents must obtain a user request to
share or accept. MCP takes the source identity from the bound transport, never
from tool arguments (in OpenCode mode, the bound `_meta["ai.opencode/sessionID"]`
caller). The CLI is a trusted local-user interface and requires the
exact local session ID.

## CLI

Flags precede positional arguments. Use exact IDs from `list_sessions`:

```sh
rhizome files send --session <local-id> --to <remote-id> ./report.md
rhizome files list --session <local-id>
rhizome files accept --session <recipient-id> <transfer-id>
rhizome files cancel --session <local-id> <transfer-id>
```

Offers are `offered` only after the recipient gateway records their metadata.
Acceptance moves the receiver to `verified`; the sender records `verified` after
receiving the receipt. Cancellation is `cancelled`. Failed staging/offering is
`failed` and holds a record slot until cleanup. Repeated acceptance of an already
verified transfer returns the same local file. A failed download removes partial
bytes; retry restarts from byte zero. There is no resumable transfer in this version.

## Temporary storage and limits

**Copy verified downloads out of the inbox to keep them.** Files live under
`<runtime>/transfers/transfer-<random-directory>/`. They expire 59 minutes after
an offer is created, and are removed on connection loss, peer revocation or gateway
shutdown/restart. Crash leftovers are removed on the next gateway start. Local
copies made outside the inbox are not tracked or deleted by Rhizome.

The fixed initial limits are 16 MiB per file, 32 transfer records per host
(at most 512 MiB of staged/downloaded bytes), two simultaneous local send/accept
operations, and 128 KiB download chunks. Empty files and binary contents work.
Chunk requests have five-second client deadlines and four-second server deadlines;
acceptance has a maximum two-minute local command deadline. Existing gateway HTTP
connection limits also apply. Offers and terminal records count against the quota
until expiration/disconnection. Downloads do not affect event retention limits.

Staging captures a private byte snapshot, so subsequent source changes do not
change the offered bytes. Concurrent writes while staging can produce a mixed
snapshot; stop writing the source before offering if application-level consistency
matters. Symlinks, directories and special files are rejected. Destination names
are single safe basenames, never peer-selected paths. Private directories and files
use 0700 and 0600 permissions. Finalization is atomic and refuses existing paths.
No file is automatically extracted, opened or executed.

## Authentication and compatibility

Every request uses TLS 1.3 with pinned paired-host keys and a currently connected
trust grant. Offers bind the exact source and target identities. Chunk, receipt and
cancellation requests revalidate those identities through the same local-route
access predicate as messaging; stale identities, collisions, observer-only targets,
forwarded sessions and untrusted hosts fail closed. Connection loss invalidates the
transfer even if the hosts reconnect. Peer revocation cancels active downloads.

Session privacy is tracked in issue #37 and is not yet implemented. Extending the
shared gateway access predicate must also restrict transfers. This version does
not imply per-session privacy or a filesystem sandbox for trusted local agents.

No protobuf or existing WebSocket format changes are needed. Older hosts reject
`/v1/files`; upgrade both hosts to transfer files. A successful offer is metadata
acceptance, not proof that the recipient downloaded or read it. A verification
receipt is a peer assertion about its download, not proof of human review.

## Validation

`go test -race ./internal/channel` covers binary and empty transfers, immutable
staging, checksums, session ownership, cancellation, symlink/size rejection and
revocation. `python3 scripts/smoke_gateway.py` exercises MCP offers, notifications,
acceptance, CLI listing, receipts and cancellation across isolated gateway services
with distinct local tokens. These are same-machine tests. A real two-device test
should offer a binary file, compare SHA-256 on both devices, interrupt an active
download, reconnect and verify that the old offer cannot be resumed.
