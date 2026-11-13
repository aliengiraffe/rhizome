# Experimental local dreaming (P0)

`rhizome dream` is hidden and explicit-only. It consolidates this device's local
coding-agent history into user-scope memory. It never runs automatically, creates
an agent session, calls an agent daemon, or writes an agent's native memory files.
There is no MCP memory API and no team merging. Memory itself never leaves this
host; the only shared artifact is an optional, reviewed digest (P1, below).

## Opt in and run

**A dream sends redacted local history and existing user memory to OpenAI using
`store: false`. Each run spends ChatGPT plan usage.** Secret filtering reduces
accidental disclosure; it is not a promise that arbitrary sensitive information
will be recognized. Review the scope before enabling this new egress boundary.

Sign in with `rhizome auth oai` ([authentication](oai.md)), then explicitly edit
the user configuration file (normally `~/.rhizome/config.toml`):

```toml
[dream]
enabled = true
include_prompts = false
include_tool_bodies = false
lookback = "7d"
model = ""
max_input_bytes = 262144
```

All six keys are file-only. There are no dream environment overrides, policy
flags, or setup questions. Defaults are shown above except `enabled`, which
defaults to **false**. Lookback accepts `1d` through `365d`, not `all`.
`max_input_bytes` accepts 4096 through 1048576. Empty model selects the first
visible model in server order, as for `test oai`.

```sh
rhizome dream --dry-run
rhizome dream
rhizome dream show
rhizome dream show --topic preferences
rhizome dream log
rhizome dream rollback 1
```

`--dry-run` still calls the model and spends usage, prints validated proposed
operations to stdout, and leaves the memory tree unchanged, including when it
does not exist. It does not reserve a revision. Show and log are read-only and
work without sign-in or enabled egress; rollback is also local and requires no
model call. Rollback appends a revision restoring the exact items at the target
revision, including their timestamps and provenance. It does not rewind history
progress. Revisions start at 1.

Before inference, stderr reports input bytes and session count. Success reports
revision and change counts. Errors never include model output or transcripts.
Only show and dry-run intentionally print memory content to stdout. Usage-limit
errors retain the OAI foundation's `subscription_sharing_usage_limit_exceeded`
code and usage link. Disabled policy makes zero network requests; missing sign-in
prints the `rhizome auth oai` hint before creating memory state.

## History and model boundaries

The input uses the observer's local metadata catalog and the same confined
snapshot readers and normalizers as `read_session_history`, starting at byte
zero. Run the observer to populate the catalog first. A stopped observer's last
catalog is usable but may omit recent history. No remote catalog is read.

Sessions and events must have recorded timestamps inside the lookback; future or
unknown timestamps are excluded. Each revision records per-session progress
cursors: a session read to its end is done through its last activity, and a
session cut by a bound resumes after its last sent event. Sessions with unread
events are selected oldest activity first, ties ordered by agent and exact
session ID; each session's events remain in source order. Work is bounded to 64
sessions, 128 history pages, a 30-second read deadline, and the configured
input-byte cap (including instructions and existing memory). History that does
not fit one input is therefore not skipped: run `rhizome dream` again until
stderr reports `remaining_sessions=0`. When a cut splits events sharing one
timestamp, the entire trailing group is held back for the next run. If no
complete group fits, the run fails without advancing progress and asks for a
larger input budget. An input with no eligible events makes no model call; it records a
revision without memory changes only to mark scanned sessions done. A nonempty
input makes exactly one memory call (180-second OAI deadline), plus one digest
call when `[gateway.dream]` is enabled (see below). Cursors before the
lookback are pruned. Stores written before cursors existed resume after their
last run time.

User prompts and tool bodies have independent opt-ins, unrelated to
`[sessions].share_prompts_and_tools`. The serving-host sharing policy and existing
history responses do not change. Peer envelopes, metadata injections, Rhizome
tool traffic and peer-only turns remain excluded. No collection leases, nodes,
live queues, log writes, or agent-daemon requests are involved.

The shared secret filter scans normalized text, the **assembled request**, and
every returned item text. A scan that cannot safely preserve request framing or
source metadata fails closed. Instructions explicitly label history and memory
as untrusted data and require absolute UTC dates derived from each source event's
timestamp for relative expressions such as “yesterday.” Semantic date conversion
is the model's responsibility; tests verify the instruction and absolute output,
not arbitrary natural-language truth.

The strict version-1 response is an object with `version` and `operations`.
Operations are `add`, `update`, `remove`, or `merge`. Each has an ID and 1–64
unique sources copied exactly from input events: `session_id`, `agent`, empty
`host_id`, and UTC `at`. Updates replace content and evidence. A merge updates an
existing ID and removes its distinct `merge_ids`; removals have no text or topic.
Adds, updates and merges have topic/text and optional future UTC `expires_at`.
Expiration is stored metadata; P0 does not automatically delete expired items.

IDs and topics use 1–64 lowercase ASCII letters, digits, underscores or hyphens,
starting with a letter or digit. Text is nonblank, at most 1024 UTF-8 bytes, with
no control characters. Unknown fields, unsupported versions, invented source
sessions/agents/timestamps, duplicate targets, more than 64 operations, or more
than 200 resulting items reject the **whole** proposal before any revision or
materialized memory is written. Prompt injection cannot authorize a new source
or a path; the model never writes files.

## Private revision store

The store is `<runtime_dir>/memory/user/`:

- `log.jsonl`: authoritative append-only revisions (schema version 1), operation
  sets, counts, cutoff, and resulting items; rollback records its target.
- `MEMORY.md`: deterministic topic index, at most 200 lines.
- `topics/<topic>.md`: deterministic memory text with source evidence.

Items record ID, scope (`user`; `private` and `team` reserved), topic, text,
creation/update/optional expiry timestamps, evidence count, and sources.
Directories are 0700 and files 0600. `<runtime_dir>/memory/dream.lock` is a
nonblocking OS lock held for the full run or rollback. Concurrent writers fail
fast. Dry runs acquire an existing lock but do not create a missing memory tree
or lock; they never commit their read snapshot.

Each file replacement is atomic. Commits preserve the exact old log bytes and
append one revision via a synced temporary file and rename, so interrupted
appends cannot leave half a JSON line. The log is bounded to 32 MiB; exceeding
that limit refuses a revision rather than pruning history. Markdown files are
rebuildable caches written before the log commit, not a multi-file transaction:
an I/O failure can leave stale caches. `dream show` and `log` always read the
committed log; a subsequent successful revision rebuilds caches. Keep a backup
of the private store before manual maintenance. Symlinked store directories or
files are refused, and topic paths never come directly from model text.

## Team digests (P1)

With `[gateway.dream].mode` set to `review` or `auto` (default `off`; see
[gateway digest sharing](gateway.md#dream-digest-sharing)), `rhizome dream` makes
a **second model call** after committing the memory revision, which spends more
plan usage, and proposes a short digest of candidate team knowledge. With mode
`off` no digest is generated and P0 behavior is unchanged.

- **Inputs.** Only this run's already-sent, redacted events from sessions that
  are public under [session privacy](privacy.md) and have a project identity,
  plus memory items whose every source is such a session. Private sessions and
  peer data never contribute. The assembled digest request is redacted and
  framing-checked again, under the same `max_input_bytes` cap (newest events
  kept).
- **Project identity.** Derived by the program from the session's project
  directory: the nearest `.git` (a linked worktree's `commondir` is followed)
  is read without running git, with bounded reads and symlinks refused, and
  `remote "origin"` is normalized to `host/owner/repo`. The host is lowercased,
  and the scheme, user information and credentials, port, query and `.git`
  suffix are dropped, so `git@host:owner/repo` and
  `https://host/owner/repo.git` match. Without a usable remote the key is
  `local:<sha256(project_path)[:12]>`, which never matches across machines.
  Credentials are never stored or sent.
- **Validation.** The strict version-1 reply has `version` and `entries`. Each
  entry has `type` (`working_on`, `decision`, `gotcha`, `blocker`), `text`
  (nonblank, at most 512 UTF-8 bytes, no control characters), `project`, 1–16
  distinct `sources` (`session_id`, `agent`) copied exactly from the request,
  all belonging to `project`, and optional `expires_at` (future UTC, at most
  30 days; default 7 days). At most 16 entries and 8 KiB per digest. Any invalid
  entry rejects the whole digest; the memory revision is kept and the command
  reports that the digest was not created. Text is redacted before storage and
  again before it is served.
- **Storage.** `<runtime>/memory/digests/<id>.json` (0700/0600), written under
  `memory/dream.lock`. At most 64 are kept; fully expired digests are pruned.
  Dry runs print the proposed digest and store nothing.

Review with the hidden commands (they never call a model or the network):

```sh
rhizome dream digest list
rhizome dream digest show <id>
rhizome dream digest approve <id> [--entries 1,3]
rhizome dream digest reject <id>
rhizome dream digest list --peers
```

In `review` mode a digest is `pending` until `approve`, which publishes only
the selected 1-based entries (all by default); `reject` withdraws it for good.
In `auto` mode digests are `published` at creation, still redacted twice.
`list --peers` shows entries received from paired hosts, grouped by project key
and labelled untrusted; they are attributed to a host ID, not a person, and are
never merged into memory.

## Verification

Automated tests use synthetic transcripts, fake OAI endpoints and disposable
state. They cover policy defaults/bounds, independent content opt-ins, lookback
and since selection, request/reply redaction, strict validation, fabricated
provenance injection, rollback, lock contention, dry runs, and OAI request/SSE
behavior. Digest tests cover remote normalization (SSH, HTTPS, credentials,
ports, `.git`, worktrees, no remote, symlinked metadata), private-session
exclusion, strict validation and bounds, review/auto status, and the serving
policy matrix. Run the repository's required build/check and stdio, LAN, service,
update and gateway smoke tests; see [AGENTS.md](../AGENTS.md).

Manual checklist (not performed by automated tests):

1. Sign in using your own account and explicitly enable egress above.
2. Start the observer, create a short local session containing a correction and
   an absolute-date fact, then inspect a dry run with prompts off and on.
3. Run one real dream. Review `dream show`, `dream log`, source agents and
   timestamps, file permissions, and the 200-line index limit.
4. Create another revision, roll back to the first, and confirm the original
   materialized items and a new rollback revision.
5. Disable the policy and verify a run refuses; show/log should still work.
6. Review plan usage. Never include private transcripts or credentials in reports.
7. For digests, pair two machines with `[gateway.dream].mode = "review"` on both,
   run a dream in a git checkout whose `origin` uses HTTPS on one machine and SSH
   on the other, approve a subset with `--entries`, and confirm `dream digest
   list --peers` on the other machine shows only those entries under the same
   project key. Mark a source session private and confirm the entry disappears
   after the next pull; revoke the peer and confirm the received file is deleted.
