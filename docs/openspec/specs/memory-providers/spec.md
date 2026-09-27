---
status: draft
date: 2026-09-27
implements: [ADR-0041]
extends: [SPEC-0005, SPEC-0006, SPEC-0017]
---

# SPEC-0030: Memory Providers and Briefings

## Overview

This spec adds episodic memory and briefings to fleet memory:

* A **memory provider**: an MCP server implementing a small, versioned
  contract. The built-in provider keeps episodes in the daemon's store and is
  the default; an operator may name an ADR-0035 upstream instead with
  `[memory] provider`. Nothing external is ever required.
* **Episodes**: one per supervised run with a repository, assembled by code
  from the run record at run end, redacted before it leaves the daemon, and
  carrying a trust class and a `project:<repo>` scope. Harness keeps its own
  copy and an index of every provider id, so it enforces scope even against a
  provider that misbehaves.
* The **`recall`** gateway tool: SPEC-0028's facts, the provider's episodes and
  recent runs from SPEC-0027's graph in one wrapped, capped,
  provenance-labelled answer, degrading to facts and the graph when the
  provider is unavailable.
* **Briefings**: a capped, sectioned summary computed at spawn. Every harness
  can pull one through the `briefing` tool and the `harness://memory/briefing`
  resource; a prompt one-shot with `memory_briefing = true` also gets it
  prepended to its prompt inside a SPEC-0017 fence.
* `MemoryService` (ADR-0034), `harness memory` commands, a TUI preview,
  `harness doctor` rows and `harness_memory_*` metrics.

See ADR-0041, Decision 6 and "Serving: hints with their source". Facts, votes
and promotion are SPEC-0028's; the graph, presence and the data wrapper are
SPEC-0027's. This spec amends SPEC-0005, SPEC-0006 REQ "Prompt Source" and
SPEC-0017 REQ-10, as REQ-20 states.

Requirements are numbered. Cite them as `SPEC-0030 REQ-n`.

## Requirements

### Requirement: REQ-1 — Provider Selection

The global-only `[memory]` table (SPEC-0028) SHALL accept `provider`: either
`"builtin"`, the default, or the key of an `[mcp.<name>]` upstream (ADR-0035)
declared in the global file or a `harness_d` drop-in. Any other value,
including an upstream declared only in a project file, SHALL fail config load
naming the key and the value. A configuration with no `[memory]` table SHALL
behave as `provider = "builtin"`.

This spec adds a global-only `[memory.episodes]` table:

| Key | Default | Meaning |
|---|---|---|
| `retention` | `"90d"` | age at which an episode is forgotten (REQ-10) |
| `summary` | `true` | request a utility summary per episode (REQ-7) |
| `provider_timeout` | `"2s"` | deadline for one `memory_recall` (REQ-12, REQ-13) |
| `excerpt_chars` | `600` | final-result excerpt cap, 0 to 2000 (REQ-5) |

#### Scenario: The default needs nothing

- **WHEN** `harness.toml` has no `[memory]` table and a supervised run in
  repository `forge.example/acme/api` ends
- **THEN** the built-in provider records its episode, and no process is
  started for it

#### Scenario: A provider must be a global upstream

- **WHEN** `[memory] provider = "mem0"` and `[mcp.mem0]` exists only in a
  project `harness.toml`
- **THEN** config load fails naming `[memory] provider` and `mem0`, and the
  daemon keeps its last-good configuration

### Requirement: REQ-2 — The Provider Upstream Is Reserved To Harness

Only the daemon's memory code SHALL call the provider upstream. Its tools SHALL
NOT appear in any harness's `tools/list` or `search_tools` result, whatever
`[mcp_policy.*]`, `[mcp] default_policy` or `exposure` say, and `call_tool`
naming one SHALL be refused `not_permitted`. Config load SHALL fail when any
`[mcp_policy.*] servers` list names the provider, or when the provider sets
`shared = false`, because episodes from every harness go to one store.

Agents SHALL have no tool that records, edits or forgets an episode. Episodes
are written only by REQ-5's assembler and forgotten only by REQ-10.

#### Scenario: A policy cannot expose the provider

- **WHEN** `[memory] provider = "zep"` and `[mcp_policy.dev] servers =
  ["gitea", "zep"]`
- **THEN** config load fails naming `[mcp_policy.dev]` and `zep`

#### Scenario: call_tool cannot reach the provider

- **WHEN** a harness in `lazy` exposure calls `call_tool` with name
  `zep__memory_forget`
- **THEN** the call is refused `not_permitted`, the upstream receives nothing,
  and the call log records outcome `denied`

### Requirement: REQ-3 — Provider Contract

A provider SHALL expose four MCP tools. Harness SHALL validate every response
against JSON schemas that ship with the binary and are printed by
`harness memory provider schema`.

| Tool | Input | Output |
|---|---|---|
| `memory_contract` | `{}` | `{contract: 1, provider: {name, version}}` |
| `memory_record_episode` | `{episode_id, scope, trust_class, episode, text}` | `{ids: [string]}`, 1 to 32 |
| `memory_recall` | `{query, scope: [string], limit, paths?}` | `{items: [{id, text, score?}]}` |
| `memory_forget` | `{id}` | `{forgotten: bool}` |

* `episode` is REQ-5's object and `text` its plain-text rendering, so a
  provider that indexes only text needs no schema knowledge.
* `ids` are the provider's identifiers for what it stored; a provider that
  derives several memories from one episode reports each. Recording the same
  `episode_id` twice SHOULD return the same ids.
* `query` is at most 512 characters, `scope` one or two scope strings, `limit`
  1 to 20, `paths` at most 20 repository-relative paths.
* An item whose `text` exceeds 2000 characters, whose `id` exceeds 128, or
  whose `score` is not finite is invalid and dropped (REQ-9). A response that
  fails its schema as a whole is a provider error.
* `memory_forget` on an unknown id SHALL return `{forgotten: false}`.

Harness SHALL call `memory_contract` when the upstream session opens and after
every reconnect. A missing tool or an unsupported `contract` SHALL mark the
provider `incompatible`: nothing is recorded to it, and recall treats it as
unavailable (REQ-13) until a later check succeeds.

#### Scenario: An unsupported contract version is not used

- **WHEN** the provider answers `memory_contract` with `{contract: 2}` and this
  binary supports only `1`
- **THEN** `harness memory status` shows the provider `incompatible`, no
  `memory_record_episode` call is made, and recall reports
  `degraded: "provider_unavailable"`

#### Scenario: An oversized item is dropped, not truncated

- **WHEN** `memory_recall` returns three items, one with 5,000 characters of
  `text`
- **THEN** the caller receives the other two, and
  `harness_memory_results_dropped_total{reason="invalid"}` increments

### Requirement: REQ-4 — Provider Conformance Check

`harness memory provider check [--provider NAME]` SHALL run a conformance
suite against the configured provider, or against a named global upstream
before the operator selects it, and SHALL exit non-zero on any `fail`:

1. `memory_contract` answers a supported version (else `fail`).
2. Each tool's `inputSchema` accepts Harness's canonical requests (`fail`).
3. Recording a synthetic episode in scope
   `project:conformance.invalid/harness/check-<nonce>` returns 1 to 32 ids
   (`fail`).
4. Recalling the nonce returns a recorded id (`fail`), within
   `provider_timeout` (`warn`).
5. Recalling it under another synthetic scope does not return the ids
   (`warn`: the provider does not filter by scope; Harness will).
6. After `memory_forget` on each id, recall returns none of them (`warn`: the
   provider does not forget; Harness tombstones).
7. Every response validates against its schema (`fail`).

Synthetic episodes SHALL NOT enter REQ-9's index, and the suite SHALL forget
every id it recorded before exiting, including on failure.

#### Scenario: A provider that ignores scope passes with a warning

- **WHEN** the provider returns the synthetic ids under the wrong scope
- **THEN** check 5 reports `warn`, the command exits 0, and REQ-9 still drops
  those ids for every real caller

#### Scenario: A provider without memory_contract fails

- **WHEN** the check targets an upstream with no `memory_contract` tool
- **THEN** check 1 reports `fail`, the command exits non-zero, and no episode
  was recorded

### Requirement: REQ-5 — Episode Assembly

When a run record closes (ADR-0028, ADR-0033), the daemon SHALL assemble an
episode from the stored run record by code, with no model and off the
supervision path. It SHALL contain exactly:

| Field | Source | Bound |
|---|---|---|
| `episode_id` | `ep_` + 20 hex of SHA-256 over harness, run id, `started_at` | fixed |
| `harness`, `adapter`, `run_id`, `trace_id` | run record | 256 B each |
| `model` | served model, else the pinned or configured one (ADR-0026) | 256 B |
| `repo`, `branch` | primary session's repo key and `git_branch`, per SPEC-0027 REQ "Repository And Path Resolution" | 256 B each |
| `outcome`, `reason` | run record (SPEC-0022 REQ-5) | fixed vocabulary |
| `started_at`, `duration_ms`, `cost_usd`, `cost_source` | run record | — |
| `files_touched` | repository-relative paths of `edit` events | 50, sorted |
| `pull_request` | a pull request SPEC-0027 links to the branch at assembly | 1 URL |
| `verify_failures` | error class and count of failed `verify` events | 10 classes |
| `result_excerpt` | first `excerpt_chars` characters of the result text | REQ-1 |
| `summary` | REQ-7, optional | 280 chars |
| `trust_class`, `scope` | REQ-6 | fixed |

The primary session is the run's first session that is not a subagent's;
files and verify failures come only from sessions in its repository. An
episode SHALL NOT contain the prompt, the event payload, tool arguments or
transcript text other than `result_excerpt`. Its `text` rendering SHALL list
the fields in the order above, one per line, identically for identical
records.

#### Scenario: A failed run yields a complete episode

- **WHEN** run 42 of `api-fixer` in `forge.example/acme/api` on `fix/flaky`
  edits three files, fails two `verify` calls with class `test_failure`, and
  ends `failed`
- **THEN** its episode lists the three paths, `verify_failures` of
  `test_failure: 2`, outcome `failed`, and no prompt text

#### Scenario: Assembly is reproducible

- **WHEN** the same stored run record is assembled twice
- **THEN** both episodes and their `text` renderings are byte-identical

### Requirement: REQ-6 — Redaction, Trust Class And Scope

Every string in an episode SHALL pass ADR-0033's shared redactor, with control
characters stripped, before the episode is stored, summarized or sent to any
provider.

`scope` SHALL be `project:<repo>` of the primary session. Episodes SHALL NOT
have `fleet` scope, and nothing SHALL change an episode's scope once
recorded. `trust_class` SHALL be `untrusted_input` when the run carried an
ADR-0021 event from a webhook or channel source or its harness sets
`untrusted_inline = true`, and `standard` otherwise. In either class,
`result_excerpt` and `summary` are agent-written text and SHALL be served with
the untrusted flag of SPEC-0027 REQ "Memory Data Wrapper".

#### Scenario: A token in the result never leaves the daemon

- **WHEN** a run's result text contains `ghp_` followed by 36 characters
- **THEN** the stored episode, the `memory_record_episode` input and the
  summary request all carry `[REDACTED]` in its place

#### Scenario: A webhook-triggered run is marked

- **WHEN** a run started by trigger `webhook.forge-pr` ends
- **THEN** its episode's `trust_class` is `untrusted_input`

### Requirement: REQ-7 — Episode Summary

When `[memory.episodes] summary = true` and `[model_api] utility_model` is set
(ADR-0036), the daemon SHALL request one summary per episode as the ADR-0036
utility use `episode_summary`: one chat completion, no tools, no loop, whose
only input is the redacted episode `text`, capped at 2,000 estimated input
tokens and 128 output tokens, under ADR-0036's shared rate limit and daily
ceiling. The result SHALL match `{summary: string}` of at most 280
characters and SHALL be redacted again. A failed, invalid, unconfigured or
slow (over 10 s) summary SHALL fall back to none, and recording SHALL NOT wait
past that deadline.

#### Scenario: No model API means no summary, not no episode

- **WHEN** `[model_api]` is absent and a run ends
- **THEN** the episode is recorded without `summary`, and
  `harness_memory_episode_summaries_total{outcome="fallback"}` increments

#### Scenario: An off-schema summary is discarded

- **WHEN** the utility model returns 900 characters
- **THEN** the episode is recorded without `summary`

### Requirement: REQ-8 — Recording Eligibility

An episode SHALL be recorded for a run when it spawned a process, recorded at
least one session whose `cwd` is inside a git repository, and its harness does
not set `memory_record = false`. `memory_record` is a `[harness.*]` boolean,
default `true`, accepted in project files too, because turning it off only
narrows. A harness with `record_trace = false` records no episode, and an
explicit `memory_record = true` on it SHALL fail config load as a key that
would do nothing.

#### Scenario: Opting a harness out

- **WHEN** `[harness.scratch] memory_record = false` and its run in
  `forge.example/acme/api` ends
- **THEN** no episode is assembled for it

#### Scenario: No repository, no episode

- **WHEN** a run's only session has `cwd = /tmp/work`, outside any repository
- **THEN** no episode is recorded, and
  `harness_memory_episodes_recorded_total{outcome="skipped"}` increments

### Requirement: REQ-9 — Episode Index And Scope Enforcement

The daemon SHALL keep in the ADR-0037 store every episode it assembled and one
index row per provider id: `(provider, provider_id, episode_id, scope,
recorded_at, forgotten_at)`. The indexed scope is the only scope Harness
trusts.

Harness SHALL pass the caller's visible scopes in `memory_recall.scope`, and
SHALL then drop every returned item whose `id` has no index row for the
configured provider (`unknown_id`), whose episode is forgotten
(`forgotten`), whose indexed scope the caller may not see (`scope`), or that
breaks REQ-3's bounds (`invalid`), counting each drop by reason. A kept item's
provenance (episode id, run, harness, adapter, model, repo, branch, outcome,
time) SHALL come from Harness's episode row; only `text` and `score` come from
the provider.

A harness caller SHALL see episodes in `project:<repo>` of its current run's
repository as SPEC-0027 records it, or of its workdir before the run's first
session. An unattributed caller (SPEC-0005 REQ "Caller Identity") SHALL see
no episodes.

#### Scenario: A provider cannot widen scope

- **WHEN** a harness in `forge.example/acme/api` recalls and the provider
  returns an id Harness indexed under `project:forge.example/acme/billing`
- **THEN** the item is dropped with reason `scope`, and no text from it
  reaches the caller

#### Scenario: Ids Harness never recorded are dropped

- **WHEN** a provider shared with another tool returns an id not in the index
- **THEN** the item is dropped with reason `unknown_id`

#### Scenario: Provenance cannot be spoofed

- **WHEN** a provider item's `text` claims it was recorded by harness
  `release-bot`
- **THEN** the served provenance names the harness from Harness's index, and
  the claim appears only inside the fenced text

### Requirement: REQ-10 — Forgetting And Retention

Forgetting an episode SHALL first set `forgotten_at` on its index rows, so
REQ-9 drops it at once, then call `memory_forget` for each provider id and
delete the stored body. A failed forget SHALL be retried with backoff for 24
hours, then marked `forget_failed`; the tombstone stays until every id is
forgotten or failed. The store's hourly pruning (ADR-0037) SHALL forget
episodes older than `[memory.episodes] retention`, for every provider. Only
retention and the operator (`harness memory forget`, `MemoryService.Forget`)
forget.

#### Scenario: Forget holds while the provider is down

- **WHEN** the operator forgets an episode while the provider times out
- **THEN** recall stops returning it immediately, and `harness memory status`
  shows one forget pending

#### Scenario: Retention applies to an external provider

- **WHEN** `retention = "30d"` and an episode recorded to `zep` is 31 days old
  at the hourly prune
- **THEN** `memory_forget` is called for each of its ids

### Requirement: REQ-11 — Built-In Provider

The built-in provider SHALL implement REQ-3 over an in-process MCP transport,
so REQ-4 and REQ-9 exercise it exactly as they do an upstream; its ids are
episode ids. It SHALL keep in the ADR-0037 store an FTS5 table
(`porter unicode61`) over each episode's `text` and `summary`, and, when
`[model_api] embedding_model` is set, a `vec1` table of one embedding per
episode cached by content hash plus model id.

It SHALL rank by BM25 alone with no embedding model, and otherwise by
reciprocal rank fusion (k = 60) of BM25 and cosine rankings as ADR-0036 does
for skills, breaking ties by recency then id, and boosting episodes whose
`files_touched` contains a requested path. A query that cannot be embedded
SHALL be answered from FTS5, with `degraded: "embeddings_unavailable"` carried
to the caller.

#### Scenario: Hybrid recall falls back to text

- **WHEN** `embedding_model` is set and `/v1/embeddings` returns 503 during a
  recall
- **THEN** the recall is answered from FTS5 with
  `degraded: "embeddings_unavailable"`, and no error

#### Scenario: The built-in provider passes its own check

- **WHEN** `harness memory provider check` runs with `provider = "builtin"`
- **THEN** every check reports `pass`

### Requirement: REQ-12 — The Recall Tool

The gateway SHALL serve `recall` in the reserved namespace as a read-tier
tool: `{query: string, paths?: [string], limit?: int}`, with `query` at most
512 characters, `paths` at most 20, `limit` 1 to 20 (default 8). It SHALL
return one SPEC-0027 REQ "Memory Data Wrapper" document with three sections in
this order, each item labelled with its provenance:

1. `facts`: up to `limit` live and suspect facts in the caller's scope from
   SPEC-0028's fact search, suspect ones marked; labelled `fact`.
2. `episodes`: up to `limit` items kept by REQ-9; labelled `episode` with
   `provider = "<name>"`.
3. `recent_runs`: with `paths`, up to 5 runs SPEC-0027's graph shows editing
   those paths in the caller's repository, newest first; labelled `run`.

Sections SHALL NOT be interleaved or re-scored against each other, since
provider scores and fact confidence are not comparable. A response SHALL be at
most 16 KiB: over that, items are dropped from the end of `episodes`, then
`recent_runs`, then `facts`, and `truncated` gives the count. No code path
SHALL write provider output as a fact, cast it as a vote, or count it as
corroboration.

#### Scenario: Facts and episodes come back separately labelled

- **WHEN** a harness in `forge.example/acme/api` calls `recall` with
  `"ledger flake"`
- **THEN** matching facts appear under `facts` labelled `fact`, episodes under
  `episodes` labelled `episode` with `provider = "builtin"`, and every item's
  text is fenced by the wrapper

#### Scenario: Provider text never becomes a fact

- **WHEN** a provider returns an item whose text reads like a fact
- **THEN** no row is written to the facts table and no vote is recorded

### Requirement: REQ-13 — Degraded Recall

When the provider times out (`provider_timeout`), errors, is `incompatible`,
or has an open ADR-0035 breaker, `recall` SHALL answer from facts and the
graph alone, with an empty `episodes` section and
`degraded: "provider_unavailable"`, which outranks `embeddings_unavailable`.
A provider failure SHALL NOT make `recall` or `briefing` return an error.

No spawn SHALL wait on the provider longer than `provider_timeout`, and no
provider failure SHALL fail, skip or further delay a run. Recording to an
unavailable provider SHALL be retried from Harness's stored copy for 24 hours,
then marked `record_failed`.

#### Scenario: A dead provider leaves spawns alone

- **WHEN** the provider upstream is killed and a `memory_briefing = true`
  one-shot starts
- **THEN** it spawns within `provider_timeout`, its briefing's episodes section
  says the provider was unavailable, and its run record is unaffected

#### Scenario: Recall says it is degraded

- **WHEN** a harness calls `recall` while the provider's breaker is open
- **THEN** it receives facts and recent runs with
  `degraded: "provider_unavailable"`, and
  `harness_memory_recalls_total{outcome="degraded"}` increments

### Requirement: REQ-14 — Briefing Composition

A briefing SHALL be one SPEC-0027 REQ "Memory Data Wrapper" document with
these sections, in this order, each capped:

| Section | Content | Items | Share |
|---|---|---|---|
| `collisions` | live presence and collisions on the harness's repository (SPEC-0027) | 10 | 25% |
| `recent_runs` | last runs on this repository and branch: harness, adapter, outcome, end, duration, pull request | 5 | 25% |
| `facts` | top live facts in scope by SPEC-0028 confidence, then recency, then id | 8 | 35% |
| `episodes` | provider items for a query built from repository, branch and harness name | 3 | 25% |

`[harness.*] briefing_max_chars` (default 2000, valid 500 to 4000) SHALL bound
the document, which SHALL also be at most 4096 bytes. Sections fill in order;
an item that exceeds its share or the remaining cap is dropped whole, never
cut, and the section ends with the count dropped and a pointer to `recall`.
Every section but `episodes` SHALL be deterministic: the same store and the
same `now` give byte-identical output. A harness outside any repository, or
with every section empty, SHALL get an empty briefing.

#### Scenario: Sections keep their order and bounds

- **WHEN** a briefing has 3 collisions, 5 recent runs, 20 live facts and a
  working provider, with `briefing_max_chars = 2000`
- **THEN** it is at most 2000 characters, its sections run collisions, recent
  runs, facts, episodes, and the facts section states how many were left out

#### Scenario: Deterministic without the provider

- **WHEN** a briefing is computed twice at the same instant over the same
  store with the provider unavailable
- **THEN** both documents are byte-identical

#### Scenario: Out-of-range cap

- **WHEN** a harness sets `briefing_max_chars = 10000`
- **THEN** config load fails naming the key and the range 500 to 4000

### Requirement: REQ-15 — Briefing By Tool And Resource

The gateway SHALL serve every attributed harness a read-tier `briefing` tool
(`{refresh?: bool}`) and an MCP resource `harness://memory/briefing`
(`text/plain`), each returning the caller's briefing. A run's briefing SHALL
be computed once, at spawn when `memory_briefing = true` and otherwise at its
first read, then served unchanged for the run so repeated reads do not churn
the agent's context; `refresh: true` recomputes it. An unattributed caller
SHALL get the error `not_attributed`. A resident harness SHALL receive
briefings only this way: nothing is typed into its terminal or written into
its configuration.

#### Scenario: A resident pulls its briefing

- **WHEN** a resident `claude-code` harness calls `briefing`
- **THEN** it receives its wrapped briefing, and nothing was prepended to or
  typed into its session

#### Scenario: The resource matches the tool

- **WHEN** a run reads `harness://memory/briefing` and then calls `briefing`
- **THEN** both return the same document

### Requirement: REQ-16 — Briefing Prepended To Prompt One-Shots

`[harness.*] memory_briefing` SHALL be a boolean, default `false`, accepted
only in the global file and `harness_d` drop-ins and rejected in a project
file and on the project-up wire, as SPEC-0017 REQ-14 treats
`untrusted_inline`. On a prompt one-shot (`prompt`, `prompt_file`,
`prompt_template` or `prompt_template_file`) the daemon SHALL compute the
briefing before spawn and prepend it:

```text
<untrusted-data source="harness-memory" field="briefing" nonce="NONCE">
BRIEFING
</untrusted-data nonce="NONCE">

PROMPT
```

The block SHALL be rendered by SPEC-0017 REQ-10's fence with a fresh nonce,
after `prompt_file` is read and any template rendered, and before the adapter
builds argv. `PROMPT` is the operator's prompt byte for byte. An empty
briefing prepends nothing; a respawn of the same run reuses its briefing. The
composed prompt SHALL NOT be persisted, logged or put on the wire (SPEC-0017
REQ-11); each prepend logs one INFO line with harness, run, bytes and item
counts. If the briefing cannot be computed, or prepending would exceed the
platform's argument limit, the run SHALL spawn with its prompt alone, log a
WARN naming harness and run, and count the skip. On a resident harness the
key only computes the briefing at spawn for REQ-15.

#### Scenario: A one-shot receives the fenced briefing

- **WHEN** `[harness.nightly-triage]` has `harness = "claude-code"`,
  `prompt_file = "~/prompts/triage.md"` and `memory_briefing = true`
- **THEN** the last argv element is the fenced briefing, a blank line, then the
  file's contents unchanged, and the nonce does not occur in the briefing

#### Scenario: A project file cannot turn it on

- **WHEN** a project `harness.toml` sets `memory_briefing = true`
- **THEN** `harness up` fails naming `memory_briefing` as global-only

#### Scenario: An oversized prompt still runs

- **WHEN** a one-shot's prompt is 127 KiB on Linux and its briefing 4 KiB
- **THEN** the run spawns with the prompt alone, a WARN names the harness, and
  `harness_memory_briefings_total{delivery="prompt",outcome="skipped"}`
  increments

### Requirement: REQ-17 — Exposure Records

These surfaces SHALL write exposure rows for every fact they serve to a run, in
the table and under the withhold-on-failure rule of SPEC-0028 REQ "Exposure,
Votes And Independence": a `recall` response and each `briefing` call or
`harness://memory/briefing` read (REQ-15), as each is served; and a briefing
prepended to a prompt (REQ-16), before spawn, with no call id. A prepended
briefing therefore counts as showing its facts, although the run made no call.
Operator previews (`harness memory briefing`, `MemoryService.GetBriefing`, the
TUI) SHALL NOT write exposure, because no run saw them.

#### Scenario: A prepended briefing blocks a vote

- **WHEN** a one-shot's prepended briefing contains fact `f_812` and the run
  then calls `vote_fact` supporting `f_812`
- **THEN** the vote is stored and not counted toward promotion

#### Scenario: A preview is not exposure

- **WHEN** the operator runs `harness memory briefing api-fixer`
- **THEN** no exposure record is written for any run of `api-fixer`

### Requirement: REQ-18 — Privacy And Telemetry

With the built-in provider, episodes SHALL NOT leave the host. An upstream
provider is the operator's choice: `harness doctor` SHALL show a `note` row
when it is an HTTP (`url`) upstream, naming its host and saying episodes leave
the host, and for a stdio provider SHALL say Harness cannot see whether that
process calls a network service.

Daemon calls to the provider SHALL appear in the ADR-0035 call log as the
daemon's own rows, with `args` hashed whatever `call_log_args` says, so the
call log is not a second copy of episodes. Episode text, summaries, briefings
and recall responses SHALL NOT be exported through ADR-0022 telemetry; memory
spans and log records MAY be exported only under its consent (`export_all` or
the harness's `export_telemetry`) and carry ids, provider, outcome and sizes
only.

#### Scenario: A network provider is visible

- **WHEN** the provider is `[mcp.zep] url = "https://zep.example.net/mcp"`
- **THEN** `harness doctor` shows a note naming `zep.example.net` and saying
  episodes leave this host

#### Scenario: Telemetry carries no episode text

- **WHEN** `[telemetry] export_all = true` and an episode is recorded
- **THEN** the exported span carries the episode id, provider and outcome, and
  no field of the episode body

### Requirement: REQ-19 — Operator Surfaces

The daemon SHALL serve `MemoryService` in `harness.v1` (ADR-0034); the gateway
tools, CLI and TUI SHALL use its handlers, and none has a private path. An
operator client sees every scope.

| RPC | Tier | CLI |
|---|---|---|
| `Status` | `read` | `harness memory status [--json]` |
| `ListEpisodes`, `GetEpisode` | `read` | `harness memory episodes [--harness H] [--repo R] [--since D]`, `harness memory episodes <id>` |
| `Recall` | `read` | `harness memory recall <query> (--repo R \| --as H) [--paths P...]` |
| `GetBriefing` | `read` | `harness memory briefing <harness>` |
| `Forget` | `control` | `harness memory forget <id>` |
| `CheckProvider` | `control` | `harness memory provider check [--provider NAME]` |

`Status` reports the provider, contract, state, counts, and pending or failed
records and forgets. `GetBriefing` returns exactly the document the harness
would receive now. The TUI SHALL open, with `B` on a selected harness, a
read-only overlay of `GetBriefing`: each section with item count, bytes used
against `briefing_max_chars`, dropped counts, provider state and whether
`memory_briefing` is set; `r` recomputes, `esc` closes.

`harness doctor` SHALL report the provider and contract version; `fail` when
it is `incompatible` or unreachable for over 5 minutes; `warn` for records or
forgets failed or pending over an hour, for degraded recalls in the last 24
hours, and for built-in episodes missing vectors while an embedding model is
set; REQ-18's note; and an `info` row listing harnesses with
`memory_briefing = true`.

#### Scenario: Forget needs the control tier

- **WHEN** a `read`-tier client calls `MemoryService.Forget`
- **THEN** it receives `PERMISSION_DENIED` before the handler runs

#### Scenario: The TUI preview shows degraded state

- **WHEN** the operator presses `B` on a harness while the provider is down
- **THEN** the overlay shows the deterministic sections and marks episodes
  `provider_unavailable`

#### Scenario: A down provider fails doctor, not harnesses

- **WHEN** the provider has been unreachable for 10 minutes
- **THEN** `harness doctor` shows a `fail` row naming it, and running and
  scheduled harnesses are unaffected

### Requirement: REQ-20 — Amendments To Existing Specs

* **SPEC-0005**: the gateway serves `recall`, `briefing` and
  `harness://memory/briefing` in the reserved namespace, and the upstream named
  by `[memory] provider` is excluded from every policy (REQ-2).
* **SPEC-0006 REQ "Prompt Source"**: `prompt` stays stored verbatim; the prompt
  given to prompt synthesis MAY be preceded by REQ-16's fenced briefing, and by
  nothing else.
* **SPEC-0017 REQ-10**: the fence format also carries REQ-16's briefing, opened
  only by `memory_briefing`. `untrusted_inline` alone still opens the fence for
  `event.*` fields; `memory_briefing` does not.

#### Scenario: memory_briefing does not open the event fence

- **WHEN** a harness sets `memory_briefing = true` and its template references
  `{{untrusted event.title}}` without `untrusted_inline`
- **THEN** config validation fails naming `untrusted_inline`

#### Scenario: The stored prompt is unchanged

- **WHEN** a `memory_briefing = true` harness with `prompt = "triage"` runs and
  `harness describe --json` is read
- **THEN** its `prompt` is `"triage"`, with no briefing text

### Requirement: REQ-21 — Metrics

The daemon SHALL export, per SPEC-0013 (`harness` capped by its REQ-5;
`provider` is one configured value):

```text
harness_memory_episodes_recorded_total{provider,outcome}  counter
harness_memory_episode_summaries_total{outcome}           counter
harness_memory_provider_up{provider}                      gauge
harness_memory_provider_duration_seconds{provider,op}     histogram
harness_memory_recalls_total{provider,outcome}            counter
harness_memory_results_dropped_total{provider,reason}     counter
harness_memory_briefings_total{harness,delivery,outcome}  counter
harness_memory_briefing_bytes{harness}                    gauge
harness_memory_pending{provider,kind}                     gauge
```

Episode `outcome` is `ok`, `error` or `skipped`; summary `outcome` is `ok`
or `fallback`; recall `outcome` is `ok`, `degraded` or `error`; `op` is
`record`, `recall`, `forget` or `contract`; `reason` is REQ-9's; `kind` is
`record` or `forget`; `delivery` is `prompt`, `tool` or `resource`, with
briefing `outcome` `ok`, `empty` or `skipped`. `harness_memory_provider_up`
SHALL be omitted before the first contract check (SPEC-0013 REQ-6). No label
SHALL carry a query, an id, a repository or text.

#### Scenario: Scope drops are countable

- **WHEN** a provider returns two out-of-scope items in one recall
- **THEN** `harness_memory_results_dropped_total{reason="scope"}` rises by two

#### Scenario: No provider reading yet

- **WHEN** the daemon has started and the provider's first contract check has
  not completed
- **THEN** `/metrics` has no `harness_memory_provider_up` series

### Requirement: Error Handling Standards

All error-producing operations in this spec SHALL follow structured error
handling:

- Errors SHALL be wrapped with context at each layer boundary, naming the
  provider, harness, run and episode id where they apply.
- Sentinel errors SHALL be defined for: provider unavailable, provider
  incompatible, provider reserved (REQ-2), unknown episode, not attributed,
  and no repository.
- A provider failure SHALL surface as `degraded` or a retried record, never be
  swallowed, and never reach an agent as an error from `recall` or `briefing`.
- Logs SHALL be structured key-value and SHALL NOT contain episode bodies,
  provider text, briefing text, queries or composed prompts: only ids, sizes,
  counts and outcomes.

#### Scenario: A provider error is attributable

- **WHEN** `memory_record_episode` fails inside upstream `zep`
- **THEN** the daemon log names `zep`, the episode id and the provider's
  redacted error message, and the episode is queued for retry
