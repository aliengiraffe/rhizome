# Dream benchmark

`scripts/bench_dream.py` measures whether `rhizome dream` memory helps coding
agents (Claude Code, Codex) reach correct outcomes with fewer tokens and less
time, and is the loop used to tune dreaming. It is a developer tool, not part of
the product.

## Privacy

Benchmarks run against the developer's own history. Everything derived from it
stays in a private directory, `RHIZOME_BENCH_DIR` (default `~/.rhizome-bench`,
mode 0700): the frozen catalog, isolated runtimes, dream memory, generated
questions, agent answers, tool inputs and reports. Nothing from it belongs in this
repository, issues or pull requests except aggregate numbers. Dreams spend
ChatGPT plan usage; agent runs spend Claude/Codex usage.

## Isolation

- `freeze` copies the observer catalog once. Each dream variant gets its own
  runtime directory under the bench root, with the catalog's scope re-bound to
  that path, so variants never touch the real memory store or each other and
  can be rerun from scratch. Two dreams cannot run at once (the OAI client's
  state lock is per user).
- `--instructions FILE` builds an experimental binary from HEAD plus the
  working tree's `internal/dream` and `internal/channel`, with the dream
  instructions replaced. The checkout is not modified.
- Agents run headless with no session persistence, no user MCP servers or
  hooks: `claude -p --no-session-persistence --strict-mcp-config
  --setting-sources ''` and `codex exec --ephemeral --ignore-user-config`, so
  benchmark runs never feed later dreams. Codex uses `BENCH_CODEX_HOME`
  (default `~/.codex-personal`). Tool inputs matching agent-log or bench-store
  paths mark the run `leak` for review; returned text mentioning those paths
  does not. Inputs are retained privately for auditing flags. This is a
  heuristic, not a filesystem access sandbox: inspect flagged requests before
  interpreting scores. A closed-book Codex run that uses tools is counted as a
  violation.

## Suites and arms

**Recall (closed-book).** Questions are generated (`gen`) from sessions sampled
independently of dreaming, stratified into `recent` (the 64 newest sessions in
the window, which a single P0 dream can read) and `older`. Each question has
1–4 atomic reference facts, a category and a repo-discoverability flag.
Near-duplicates (repeated scripted sessions) are dropped. The agent answers with
no tools from the prompt alone.

**Open-book.** The same questions with the agent in a disposable worktree of
this repository (read-only tools, git and `gh`), measuring time and tokens to
an answer.

Arms prepend the same untrusted-notes preamble so only the notes differ:

| Arm | Notes |
| --- | --- |
| `none` | none (baseline) |
| `<variant>` | `rhizome dream show` output of that dream variant |
| `raw` | unconsolidated recent assistant excerpts with the same byte budget as dream memory (does consolidation beat retrieval of raw text?) |
| `oracle` | the question's own source session (ceiling; validates the questions) |
| `tools` | no notes; Rhizome MCP `search_sessions`/`read_session_history` on the frozen catalog (on-demand retrieval) |

## Grading and statistics

An LLM judge marks each reference fact as stated or not, and flags
contradictions and abstentions (`grade`). Scores: fact recall (fraction of facts
stated), correct (all facts), contradiction and abstention rates. `--judge-repeats
2` re-grades every answer to report judge test-retest agreement (Cohen's κ on
per-fact labels).

Claude remains the default judge. `grade --judge-agent codex` uses the configured
`BENCH_CODEX_MODEL` and `BENCH_CODEX_EFFORT`, with structured JSON output and no
tool use. It writes a separate grades file keyed by judge model and effort;
`report --judge-agent codex` reads only that file and labels the judge in its
output. Claude grades are never overwritten or pooled with Codex grades.
Codex grades independent cases in batches (`--batch-size`, default 8, maximum
32); repeated judgments of an answer always occur in separate calls. Every
case ID, fact count and boolean label is checked before a batch is accepted.
Batch sizes are recorded with each grade. Judge availability failures stop the
stage while preserving completed grades. Compare judges as separate analyses:
model choice and batching can affect scoring.

`report` follows [Adding Error Bars to Evals](https://arxiv.org/abs/2411.00640):
repeats are averaged per question (the question is the clustering unit), arms
are compared by **paired** per-question differences against the baseline with
bootstrap 95% confidence intervals, and `pass_all_k` is the share of questions
answered correctly on every repeat (pass^k consistency, as in τ-bench).
Efficiency metrics are total and uncached input tokens, output tokens, wall
time and tool calls, reported per arm and as paired differences.

## Running

```sh
python3 scripts/bench_dream.py freeze
python3 scripts/bench_dream.py dream --variant dream --set include_prompts=false
python3 scripts/bench_dream.py dream --variant cv1 --set include_prompts=true \
  --instructions ~/.rhizome-bench/instructions/v1.txt --passes 60
python3 scripts/bench_dream.py gen --exclude <current-session-id>
python3 scripts/bench_dream.py run --suite recall --arms none,dream,raw,oracle --repeats 2
python3 scripts/bench_dream.py grade --judge-repeats 2
python3 scripts/bench_dream.py report --suite recall
# Optional independent Codex judge, preserving Claude results:
python3 scripts/bench_dream.py grade --judge-agent codex --judge-repeats 2
python3 scripts/bench_dream.py report --suite recall --judge-agent codex
```

Every stage is resumable; rerunning skips completed rows. Grading stops on a
failed judge response (including account usage limits) and exits nonzero;
completed grades remain available for the next run. Reports state the number
of successful runs still missing grades, so a partial report is identifiable.
Resumed dreams retain prior pass metadata and save each completed pass; failed
dream calls exit nonzero. Agent error responses are
excluded from scoring, even when the CLI labels their subtype as success. Environment overrides:
`BENCH_CLAUDE_MODEL`, `BENCH_CODEX_MODEL`, `BENCH_CODEX_EFFORT`,
`BENCH_JUDGE_MODEL`, `BENCH_GEN_MODEL`, `RHIZOME_BIN`.

Offline harness regression checks (no account, model calls or real agent state):
`python3 scripts/test_bench_dream.py`.

## Limits

Questions and the judge are model-generated; spot-check samples before trusting
a change. Recall questions favour specific facts, so they measure memory
coverage more than general helpfulness. The oracle arm bounds what any memory
can reach. A single developer's history is one distribution; results guide
tuning for that user, not universal claims.
