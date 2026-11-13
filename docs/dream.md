# Experimental local dreaming (P0)

`rhizome dream` is hidden and explicit-only. It consolidates this device's local
coding-agent history into user-scope memory. It never runs automatically, creates
an agent session, calls an agent daemon, or writes an agent's native memory files.
There is no peer, gateway, team, or MCP memory API in P0.

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
revision, including their timestamps and provenance. It does not rewind the last
successful dream's history cutoff. Revisions start at 1.

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

Sessions and events must have recorded timestamps inside the lookback and newer
than the last successful run; future or unknown timestamps are excluded. Sessions
are selected newest-first, ties ordered by agent and exact session ID; each
session's events remain in source order. Work is bounded to 64 sessions, 128
history pages, a 30-second read deadline, and the configured input-byte cap
(including instructions and existing memory). The first event that would exceed
the budget ends selection. Omitted older events are not backfilled after a
successful run advances the cutoff. An empty input makes no model call and no
revision. A nonempty input makes exactly one call (180-second OAI deadline).

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

## Verification

Automated tests use synthetic transcripts, fake OAI endpoints and disposable
state. They cover policy defaults/bounds, independent content opt-ins, lookback
and since selection, request/reply redaction, strict validation, fabricated
provenance injection, rollback, lock contention, dry runs, and OAI request/SSE
behavior. Run the repository's required build/check and stdio, LAN, service and
update smoke tests; see [AGENTS.md](../AGENTS.md).

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
