---
status: draft
date: 2026-09-27
implements: [ADR-0041]
---

# SPEC-0028: Fleet Facts

## Overview

This spec adds the facts half of fleet memory:

* A **facts table** in the daemon's store (ADR-0037): one row per fact, with a
  `kind` (`observed`, `synced`, `asserted`, `operator`), a subject that is a
  SPEC-0027 graph node, a predicate from a closed vocabulary shipped with the
  binary, a bounded value, a scope (`project:<repo>` or `fleet`), a status, a
  validity window and its source run, harness, adapter and model.
* **SQLite triggers** that make the store refuse a vote on a non-asserted fact
  and any change to a fact's kind (or a vote's fact), with writer-by-kind
  enforced in the handlers.
* **Fail-safe trust**: a fact is trusted only when every evidence reference
  points at a repository file, a run record or a trusted fact.
* **Anchors** that make a fact suspect when the code under it moves, **churn**
  that lowers confidence commit by commit, and a time-to-live only for facts
  with nothing to anchor to.
* **Votes** with an independence rule (a run shown a fact cannot corroborate
  it), **weights** from a pinned public-ranking snapshot adjusted by each model
  family's local record, **promotion** to `fleet` scope behind distinct-source
  minimums and a **veto window** that starts only when the operator is told.
* **Retraction** by the operator only, sticky for a configured period.
* A **consolidation** one-shot that alone may re-verify suspect facts, a
  `harness memory weights refresh` client command, `FactService`, the gateway
  tools `record_fact`, `vote_fact` and `reverify_fact`, `harness facts`, and
  three TUI views.

See ADR-0041 for the decision and the options it rejected. The graph, graph sync
and the data wrapper are SPEC-0027's; `recall` and briefings are SPEC-0030's;
the policy repo is SPEC-0029's. This spec cites them and does not redefine them.

Requirements are numbered. Cite them as `SPEC-0028 REQ-n`.

## Requirements

### Requirement: REQ-1 — The Facts Table

The store SHALL hold a `facts` table with these columns:

| Column | Meaning |
|---|---|
| `fact_id` | ULID, immutable |
| `kind` | `observed`, `synced`, `asserted` or `operator` (CHECK constraint) |
| `subject_kind`, `subject_key` | a SPEC-0027 graph node, e.g. `repo` / `forge.example/acme/api` |
| `predicate` | one term of the closed vocabulary (REQ-2) |
| `value`, `value_hash` | at most 280 Unicode code points; SHA-256 of the normalized value (REQ-5) |
| `scope` | `project:<repo>` (the SPEC-0027 repo key, `<host>/<owner>/<name>`) or `fleet` |
| `status` | `live`, `suspect`, `pending_promotion`, `ended` or `retracted` |
| `status_reason` | the last transition's reason, from REQ-23's reason set |
| `valid_from`, `valid_to` | validity window; `valid_to` is null while not ended or retracted |
| `recorded_at`, `last_observed_at` | write time; last counted re-observation (REQ-5) |
| `confidence` | 0 to 1, REQ-8 |
| `superseded_by` | a `fact_id`, or null |
| `untrusted_source` | boolean, REQ-4 |
| `source_harness`, `source_run_id`, `source_adapter`, `source_model`, `source_model_family`, `weights_sha` | the writing run; family as derived under snapshot `weights_sha` (REQ-12) |
| `ends_on` | `observed` and `synced` only: the event that ends the fact (REQ-10) |
| `suspect_since`, `notice_at`, `notice_via`, `veto_elapsed_s`, `veto_resumed_at` | REQ-9 and REQ-14 bookkeeping |

A value longer than 280 code points SHALL be rejected, not truncated. The
subject SHALL name a node that exists in the graph when the fact is written.
A partial unique index SHALL allow at most one fact per dedup key (REQ-5) whose
status is `live`, `suspect` or `pending_promotion`. Every status transition
SHALL append a row to `fact_events` (`fact_id`, `at`, `from`, `to`, `reason`,
`actor`, `detail` JSON), which is never updated or deleted while its fact
exists.

#### Scenario: An oversized value is refused

- **WHEN** `record_fact` is called with a 281-character value
- **THEN** the call fails naming the 280-character limit, and no row is written

#### Scenario: A subject must exist

- **WHEN** `record_fact` names `file:forge.example/acme/api:cmd/nope.go` and the
  graph has no such node
- **THEN** the call fails with an unknown-subject error

### Requirement: REQ-2 — Closed Predicate Vocabulary

The predicate SHALL be one of this vocabulary, compiled into the binary with a
version number:

| Predicate | The value states |
|---|---|
| `fails_when` | the condition under which the subject fails |
| `flaky` | the symptom of a nondeterministic failure |
| `slow` | what is slow and by roughly how much |
| `requires` | a tool, version, service or setting the subject needs |
| `conflicts_with` | what breaks when used together with the subject |
| `workaround` | a known way around a failure of the subject |
| `convention` | a practice the subject's maintainers follow |
| `deprecated` | what replaces the subject, or why not to use it |
| `reproduces_on` | the platform or environment where a failure reproduces |
| `broken_by` | the change that introduced a failure |

There SHALL be no free-form or `other` predicate. An unknown predicate SHALL be
refused, and the error SHALL list the vocabulary. A release MAY add a term. A
term removed by a release SHALL stay readable on existing facts and SHALL be
refused for new writes and votes.

A fact about something several repositories share SHALL take that thing's
node as subject, so records from different repositories share a group key
(REQ-5): "golangci-lint is slow" is `slow` on `tool:golangci-lint`, and a
fact about a module is on its `dependency:` node (SPEC-0027 REQ "Node Kinds And
Identity").

#### Scenario: No escape predicate

- **WHEN** `record_fact` is called with `predicate = "note"`
- **THEN** it fails, listing the ten terms, and nothing is written

### Requirement: REQ-3 — Kinds, Writers And Store Triggers

The daemon is the only writer the store sees, so the handler SHALL set `kind`
from the caller, never from the request:

| Caller | Kind written |
|---|---|
| the observer, from traces or the ledger (SPEC-0027) | `observed` |
| a `harness graph sync` batch (SPEC-0027) | `synced` |
| `record_fact` from an attributed run | `asserted` |
| `AssertFact` from an operator caller (REQ-22) | `operator` |

The schema migration SHALL create these triggers, in the form ADR-0037 permits:

```sql
CREATE TRIGGER fact_votes_asserted_only BEFORE INSERT ON fact_votes
WHEN (SELECT kind FROM facts WHERE fact_id = NEW.fact_id) IS NOT 'asserted'
BEGIN SELECT RAISE(ABORT, 'fact_votes: fact kind is not asserted'); END;

CREATE TRIGGER facts_kind_immutable BEFORE UPDATE OF kind ON facts
BEGIN SELECT RAISE(ABORT, 'facts: kind is immutable'); END;

CREATE TRIGGER fact_votes_fact_immutable BEFORE UPDATE OF fact_id ON fact_votes
BEGIN SELECT RAISE(ABORT, 'fact_votes: fact_id is immutable'); END;
```

The handler SHALL check the kind before inserting a vote, so reaching a trigger
is a bug: it SHALL be logged at error level and counted (REQ-24).

#### Scenario: A vote on an observed fact fails in SQLite

- **WHEN** a test inserts a `fact_votes` row directly for an `observed`,
  `synced` or `operator` fact
- **THEN** the insert fails with `fact_votes: fact kind is not asserted`

#### Scenario: Kind cannot change

- **WHEN** a test runs `UPDATE facts SET kind = 'operator'` on an asserted fact,
  or sets `kind` to its current value
- **THEN** the statement fails with `facts: kind is immutable`

#### Scenario: An agent cannot choose its kind

- **WHEN** a `record_fact` request carries `kind = "operator"`
- **THEN** the field is rejected as unknown, and no fact is written

### Requirement: REQ-4 — Evidence And Untrusted Source

`record_fact` and `vote_fact` SHALL accept up to 16 `evidence` references:

| Ref | Form | Trusted when |
|---|---|---|
| repository file | `file:<repo>:<path>@<blob_sha>` | the path has that blob in a synced default-branch tree listing of `<repo>` |
| run record | `run:<harness>#<run_id>` | always (the SPEC-0022 record) |
| trace event | `event:<harness>#<run_id>/<session_id>/<seq>` | its action is `edit` or `verify`; any other action is tool output |
| fact | `fact:<fact_id>` | that fact is not `untrusted_source` |
| pull request, issue, review | `pr:<repo>#<n>`, `issue:<repo>#<n>`, `review:<repo>#<n>/<id>` | never |
| web | `url:<https URL>` | never |

A `run`, `event` or `fact` ref naming a record that does not exist SHALL fail
the call. Refs SHALL be stored in `fact_evidence` with their computed trust.
`untrusted_source` SHALL be true unless **every** ref is trusted, and SHALL be
true when there is no evidence, when the writing run was started by a trigger
carrying an event payload (SPEC-0014), and when the writing harness is a
consolidator (REQ-16). It SHALL be computed by the handler and SHALL NOT be
settable by the caller. It never changes after write.

#### Scenario: One issue link taints the fact

- **WHEN** a fact cites `file:…/ci.yml@3f2a…` (matching the synced tree) and
  `issue:forge.example/acme/api#41`
- **THEN** it is written with `untrusted_source = true`

#### Scenario: No evidence is untrusted

- **WHEN** `record_fact` is called with an empty `evidence` list
- **THEN** the fact is live in its project and `untrusted_source = true`

#### Scenario: A webhook-triggered run writes untrusted facts

- **WHEN** a run whose trigger carried an issue-comment event records a fact
  citing only a `run:` ref
- **THEN** the fact is untrusted

### Requirement: REQ-5 — Dedup And Re-Observation

The **dedup key** SHALL be `(scope repo, subject, predicate, value_hash)`, where
the scope repo is empty for `fleet`. The **group key** SHALL be `(subject,
predicate, value_hash)`. `value_hash` SHALL be the SHA-256 of the value after
Unicode NFC, lowercasing, trimming, collapsing internal whitespace to one space
and stripping trailing `.`, `;` and `!`.

`record_fact` SHALL first look for a `fleet` fact with the same group key, then
for a fact in the caller's project with the same dedup key, in status `live`,
`suspect` or `pending_promotion`. A match SHALL NOT create a fact. It SHALL
insert a `support` vote from the calling run (REQ-11), and a counted support
SHALL set `last_observed_at`. The response SHALL return the existing
`fact_id` with `result = "reobserved"`. A match on a suppressed key is REQ-15.

#### Scenario: The same observation twice is one fact

- **WHEN** two runs in `forge.example/acme/api` record `slow` on one subject as
  `"go test -race takes 9m"` and `"Go test -race takes 9m."`
- **THEN** one fact exists, with the second run's support vote on it

#### Scenario: A fleet fact absorbs a project record

- **WHEN** a `fleet` fact exists for a group key and a run in another
  repository records the same subject, predicate and value
- **THEN** the run's record becomes a support vote on the fleet fact

### Requirement: REQ-6 — Anchors

A fact MAY carry anchors in `fact_anchors`, one row each, with a `state` of
`ok`, `changed`, `reverted` or `unresolved`:

| Anchor | Caller supplies | Recorded |
|---|---|---|
| `span` | repo, path, line range | blob SHA, range, base commit |
| `path` | repo, path | blob SHA, base commit |
| `dependency` | repo, `dependency` node | module, synced version |
| `agent_version` | adapter (default: the writing run's) | adapter, version and fingerprint from the writing run's record |
| `grounding_pr` | repo, number | number, merge SHA |

The handler SHALL resolve blob SHAs and the base commit from the latest synced
default-branch tree of the repository, and a dependency's version from the
repo's synced `depends_on` edge (SPEC-0027 REQ "Graph Sync"), and an agent's
version from the writing run's ledger record (SPEC-0027 REQ "Agent Version
Recording"), and SHALL NOT accept a caller-supplied blob SHA or version. A path
absent from that tree, a pull request not recorded as merged, a dependency with
no `depends_on` edge, and an `agent_version` anchor whose writing run recorded
no version SHALL be stored `unresolved`. Other tools' versions are anchored
where they are pinned: a manifest entry as `dependency`, a runner image tag as a
`span` of the workflow file that names it. A fact whose anchors are all
`unresolved` SHALL be treated as unanchored (REQ-10) until one resolves. A write
SHALL carry at most 8 anchors, all in the writing run's repository; only
promotion (REQ-14) gives a fact anchors in other repositories.

#### Scenario: The agent's branch does not set the blob

- **WHEN** a run on a feature branch anchors a fact to `internal/store/db.go`
- **THEN** the anchor records the blob SHA from the last synced default-branch
  tree, not the working copy's

#### Scenario: A new file is unresolved until merged

- **WHEN** a fact anchors to a path that exists only on an unmerged branch
- **THEN** the anchor is `unresolved`, the fact's TTL runs, and the first sync
  that lists the path resolves it and stops the TTL

### Requirement: REQ-7 — Anchor Checks On Graph Sync

`FactService.AnchorTargets(repo)` SHALL return each anchored path with its
recorded blob and base commit. `harness graph sync` carries, for each target,
the per-path commit count, the changed line ranges and the dependency versions
that SPEC-0027 REQ "Graph Sync" defines; no file content reaches the daemon.
After committing the batch, the daemon SHALL apply, in one transaction per
repository:

* `path` anchor, blob changed or path deleted → `changed`; the fact becomes
  `suspect`.
* `span` anchor, path deleted or a hunk overlapping the span (SPEC-0007 REQ
  "Staleness Re-Verification") → `changed`; the fact becomes `suspect`.
  Otherwise the span is shifted by the line delta of the hunks above it and
  re-recorded at the new blob, and the fact stays as it was.
* `dependency` anchor, synced version differs or `depends_on` ended →
  `changed`; the fact becomes `suspect`.
* `grounding_pr` anchor, the pull request recorded `reverted_by` →
  `reverted`; the fact becomes `ended` with reason `reverted`, whatever its
  other anchors say.

An `agent_version` anchor SHALL be checked when a run's record gains its
`agent_version`, not on sync: when a run of the anchored adapter in the fact's
repository records a version different from the anchor's, the anchor becomes
`changed` and the fact `suspect` with reason `agent_version_changed`. A run
that recorded no version SHALL change nothing.

`observed`, `synced` and `operator` facts SHALL follow the same rules, except
that a suspect `operator` fact SHALL NOT join the re-verification queue and SHALL
appear in the operator feed instead.

#### Scenario: An agent upgrade makes a version-anchored fact suspect

- **WHEN** a fact recorded by a `crush` run at version `0.9.1` carries an
  `agent_version` anchor, and a later `crush` run in the same repository records
  `0.10.0`
- **THEN** the fact is `suspect` with reason `agent_version_changed`, and a
  `claude-code` run's version leaves it unchanged

#### Scenario: A changed span makes the fact suspect

- **WHEN** a fact anchors lines 40–52 of `ledger.go` and the next sync's hunks
  change line 47
- **THEN** after that sync the fact is `suspect` with reason `span_changed`

#### Scenario: Edits elsewhere move the span

- **WHEN** a commit inserts 6 lines at line 10 of the same file and touches
  nothing in 40–52
- **THEN** the anchor becomes lines 46–58 at the new blob and the fact stays
  `live`, with one commit of churn

#### Scenario: A reverted grounding pull request ends the fact

- **WHEN** a sync records that pull request 212, a fact's `grounding_pr`
  anchor, was reverted
- **THEN** the fact is `ended` with reason `reverted`

### Requirement: REQ-8 — Churn Lowers Confidence

For `asserted` facts, `confidence` SHALL be computed from stored rows as

```text
confidence = churn_decay ^ n
n = Σ over the fact's path and span anchors of the commits counted by REQ-7
    since the anchor was recorded or last renewed
```

with `churn_decay` from `[memory.expiry]` (default `0.9`). A fact with no path
or span anchor SHALL keep `confidence = 1.0`. When `confidence` falls below
`churn_floor` (default `0.3`) the fact SHALL become `suspect` with reason
`churn`. A renewal (REQ-16) SHALL reset `n` to 0. Elapsed time SHALL NOT lower
confidence. Decay SHALL NOT apply to `observed`, `synced` or `operator` facts.

#### Scenario: A still file keeps full strength

- **WHEN** a fact anchored to a file has had no commit touch that file for a
  year
- **THEN** its confidence is 1.0 and its status is unchanged

#### Scenario: A hot file wears a fact down

- **WHEN** 12 commits have touched the anchored file since recording, none
  inside its span
- **THEN** confidence is 0.9^12 ≈ 0.28, below 0.3, and the fact is `suspect`

### Requirement: REQ-9 — Suspect Facts And The Re-Verification Queue

A `suspect` fact SHALL be served marked `suspect` with
`effective_confidence = confidence × 0.5`, SHALL NOT be promoted, and SHALL
pause any veto window it is in (REQ-14). Every suspect `asserted` fact SHALL
join the re-verification queue ordered by `suspect_since`, oldest first. The
queue SHALL be served only to consolidators (REQ-16), at most
`[memory] reverify_per_run` (default `5`) facts per run.

#### Scenario: Re-verification is bounded

- **WHEN** eight facts are suspect and a consolidation run starts
- **THEN** it can lease the five oldest, and three wait for the next run

### Requirement: REQ-10 — Expiry Of Unanchored And Deterministic Facts

An `asserted` fact with no resolved anchor SHALL end with reason `ttl` when
`last_observed_at + ttl` has passed, where `ttl` is `[memory.expiry]
trusted_ttl` (default `"30d"`) or, for an untrusted fact, `untrusted_ttl`
(default `"7d"`). Only a counted support (REQ-11) renews it. An `observed` or
`synced` fact SHALL carry the event that ends it (`run_end`, `superseded`, or
`sync_absent`), and SHALL end on that event and on nothing else; neither kind
has a TTL. `operator` facts SHALL NOT expire. Expiry SHALL be evaluated by a
ticker at most one minute apart and at daemon start.

#### Scenario: Re-observation renews

- **WHEN** an unanchored trusted fact recorded on day 0 gets a counted support
  on day 25
- **THEN** it is still live on day 50 and ends on day 55

#### Scenario: Presence ends with the run

- **WHEN** an `observed` fact with `ends_on = run_end` belongs to run 14, and
  run 14 closes
- **THEN** the fact is `ended` with reason `run_ended`

### Requirement: REQ-11 — Exposure, Votes And Independence

The store SHALL hold `fact_votes` (`fact_id`, `harness`, `run_id`, `stance`
`support` or `contradict`, `reason` of at most 280 code points, `model`,
`model_family`, `weights_sha`, `via` `record_fact` or `vote_fact`,
`untrusted_source` per REQ-4, `counted`, `not_counted_reason`, `recorded_at`),
unique on `(fact_id, harness, run_id)`. Only an attributed caller (SPEC-0005 REQ
"Caller Identity") with a current run SHALL vote. The model SHALL be the call
log's model for the call (ADR-0035), else the run's latest served model before
the call, else empty, which maps to family `unknown`.

Every surface that serves a fact to an attributed run SHALL write a
`fact_exposures` row (`fact_id`, `harness`, `run_id`, `via`, `call_id`, `at`)
and commit it before the fact reaches the run; if the write fails, the fact
SHALL be left out. There are three writers: `graph_neighbors` (SPEC-0027 REQ
"Tool graph_neighbors"), the ask-only graph tools (SPEC-0027 REQ "Canned
Questions And Ask"), and `recall` and briefings by tool, resource or prompt
(SPEC-0030 REQ "Exposure Records"); `call_id` is null for a prepended briefing.

A vote SHALL be stored with `counted = false` and `not_counted_reason =
"exposed"` when its run has an exposure to any fact with the same group key
recorded before the vote, and with reason `retracted` when its target is
`retracted`. A run SHALL NOT exceed `[memory] max_votes_per_run` (default `50`)
votes or `max_records_per_run` (default `20`) new facts; further calls SHALL
fail.

#### Scenario: A run that recalled a fact cannot vote for it

- **WHEN** a run receives fact F through `recall` and then calls `vote_fact`
  support on F
- **THEN** the vote is stored with `counted = false`, reason `exposed`, and
  adds nothing to promotion or renewal

#### Scenario: One vote per run

- **WHEN** a run votes twice on the same fact
- **THEN** the second call fails as a duplicate vote, and the first stands

#### Scenario: An unattributed session cannot vote

- **WHEN** an interactive session started outside Harness calls `vote_fact`
- **THEN** the call fails as unattributed

### Requirement: REQ-12 — Model Weights

A `[policy_repo.<name>]` (SPEC-0029) named by `[memory.promotion] weights_repo`
SHALL supply `weights/models.toml`:

```toml
schema     = 1
fetched_at = "2026-09-20T04:00:00Z"
[[source]]
name = "swe-bench-verified"   # built-in fetcher id
url  = "https://www.swebench.com/"
fetched_at = "2026-09-20T04:00:00Z"
content_sha256 = "9c1e…"      # of the raw fetched document
[models."anthropic/claude-opus-5"]
family  = "anthropic"
aliases = ["claude-opus-5"]
scores  = { swe-bench-verified = 0.79, aider-polyglot = 0.83 }
[[family_rule]]               # families for models with no [models] entry
match  = "anthropic/*"
family = "anthropic"
```

When the daemon loads a new version of the file it SHALL store it in
`weight_snapshots` keyed by its SHA-256 (`weights_sha`). Weights SHALL be
computed only from stored rows and one snapshot:

```text
norm(m,b) = (s(m,b) − min_b) / (max_b − min_b)      # 1 when max_b = min_b
p(m)      = mean of norm(m,b) over the benchmarks listing m
w0(m)     = prior_min + (prior_max − prior_min) × p(m);  unknown_weight if m unlisted
u_f       = facts family f supported (counted) that reached fleet scope, not retracted
r_f       = facts family f supported (counted) that were retracted
q_f       = (α + u_f) / (α + β + u_f + r_f)
mult_f    = clamp(q_f / (α / (α + β)), mult_min, mult_max)
w(m)      = w0(m) × mult_{family(m)}
```

Defaults: `prior_min = 0.5`, `prior_max = 1.5`, `unknown_weight = 0.25`,
`beta_prior = [4.0, 1.0]` (α, β), `multiplier_range = [0.25, 1.25]`. A model
matched by neither `[models]`, `aliases` nor a `family_rule` SHALL have family
`unknown`. With no weights file, every model SHALL get `unknown_weight` and
family `unknown`.

#### Scenario: An unlisted model gets the low default

- **WHEN** a vote comes from `z-ai/glm-5.3-flash` and the snapshot neither lists
  it nor matches it with a family rule
- **THEN** its weight is 0.25 × the `unknown` family multiplier

#### Scenario: Retractions lower a family

- **WHEN** family `acme` has u = 0 and r = 6 under the defaults
- **THEN** q = 4/11, the multiplier is 0.4545…, and each `acme` weight is less
  than half its prior

#### Scenario: Same rows, same weights

- **WHEN** weights are computed twice from the same rows and `weights_sha`
- **THEN** both computations return identical values

### Requirement: REQ-13 — Promotion Thresholds

For each group key, a vote SHALL **count toward promotion** when it is counted
(REQ-11), is on a group member that is `live` or `pending_promotion`, and is
either a contradiction or trusted. Untrusted members SHALL contribute nothing.
With `W_f⁺` and `W_f⁻` the weight sums of counting supports and contradictions
from family f:

```text
S = Σ_f min(family_cap, W_f⁺) − Σ_f min(family_cap, W_f⁻)
```

The group's representative (its earliest-recorded, trusted, `live` member)
SHALL become `pending_promotion` when all hold: supports come from at least
`min_repos` distinct repositories, `min_families` distinct families and
`min_runs` distinct runs; and `S ≥ threshold`. `[memory.promotion]` defaults:
`min_repos = 2`, `min_families = 2`, `min_runs = 3`, `family_cap = 1.0`,
`threshold = 1.5`. `min_families` and `min_runs` below 2 SHALL fail the load.
A pending fact whose group falls below any threshold SHALL return to `live`
and its veto clock SHALL reset. Thresholds SHALL be re-evaluated on every vote,
status change or weights load touching the group.

#### Scenario: One family never promotes

- **WHEN** four runs of one family in four repositories support a fact and
  `min_families = 2`
- **THEN** the fact is not `pending_promotion`, whatever S is

#### Scenario: Two families promote

- **WHEN** supports come from an `anthropic` run in `forge.example/acme/api`
  (w 1.4) and two `openai` runs in `forge.example/acme/web` (w 0.9 each), none
  exposed, all trusted
- **THEN** S = 1.0 + 1.0 = 2.0 ≥ 1.5 and the representative is
  `pending_promotion`

#### Scenario: An untrusted fact never auto-promotes

- **WHEN** every member of a group is `untrusted_source`
- **THEN** no member becomes `pending_promotion`, whatever the votes

### Requirement: REQ-14 — Veto Window And Notice

A `pending_promotion` fact's window of `[memory.promotion] veto_window`
(default `"72h"`) SHALL start at its first **notice**, recorded once in
`notice_at` and `notice_via`:

* `api`: `FactService.ListPending` returned it to an operator caller (REQ-22);
  the TUI feed (REQ-21) and `harness facts pending` use this call;
* `memory_digest`: a `[notify]` delivery of a `memory_digest` event naming it
  finished with result `ok`.

With no notice the window SHALL NOT start and the fact SHALL NOT promote. The
daemon SHALL send at most one `memory_digest` per `[memory.promotion]
digest_every` (default `"1h"`), naming up to 20 pending facts without notice,
oldest first, in a new `facts` payload field (`fact_id`, `subject`,
`predicate`, redacted `value`, `untrusted_source`). `memory_digest` SHALL NOT
be in the default `[notify] events`. `internal/notify` SHALL report each
delivery's result to the caller that queued it. While the fact is `suspect` the
window SHALL be paused (`veto_elapsed_s` kept); renewal resumes it. When
`veto_elapsed_s` reaches `veto_window`, the representative SHALL take scope
`fleet` and status `live`, gain copies of every member's anchors, and each
other member SHALL end with `superseded_by` set to it.

#### Scenario: Silence without notice never promotes

- **WHEN** a fact became `pending_promotion` 80 hours ago, no digest was
  delivered, and no operator caller listed pending facts
- **THEN** it is still `pending_promotion` with a null `notice_at`

#### Scenario: A failed hook starts nothing

- **WHEN** a `memory_digest` naming the fact times out
- **THEN** `notice_at` stays null, and the next digest names it again

#### Scenario: Suspect pauses the clock

- **WHEN** a fact 50 hours into its window turns suspect for 10 hours and is
  then renewed
- **THEN** it promotes 22 hours after renewal, not 12

### Requirement: REQ-15 — Retraction And Sticky Suppression

Only an operator caller (REQ-22) SHALL retract, through the TUI, `harness facts
retract` or `FactService.RetractFact`. Retraction SHALL set status `retracted`,
`valid_to`, and a reason, and SHALL record a suppression of the fact's dedup
key, or of its group key when the fact was `fleet` or `pending_promotion`, for
`[memory] suppress_retracted` (default `"90d"`). A `record_fact` matching a
suppressed key SHALL NOT create or revive a fact: it SHALL be stored as an
uncounted vote on the retracted fact with reason `retracted`, and the response
SHALL say `result = "suppressed"`. After the suppression lapses, a new record
SHALL create a new fact. A retraction SHALL add one to `r_f` for every family
with a counted support on it (REQ-12). Agents SHALL only record and vote; only
the system (anchors, TTL, re-verification) ends facts, and only the operator
retracts.

#### Scenario: A retracted fact cannot be asserted back

- **WHEN** an operator retracts a fact and, 10 days later, a run records the
  same subject, predicate and value
- **THEN** no live fact results, and the run's record is an uncounted vote on
  the retracted fact

#### Scenario: Retracting a pending fact is the veto

- **WHEN** the operator presses `x` on a pending fact in the feed
- **THEN** it is `retracted`, never promotes, and its group key is suppressed

### Requirement: REQ-16 — Consolidators

`[memory] consolidators` SHALL list global harnesses, each a scheduled prompt
one-shot (ADR-0013: `schedule` plus `prompt` or `prompt_file`) with a
`model_pin` (ADR-0026) and `max_tokens` or `max_cost_usd` (ADR-0027); anything
else SHALL fail the load, naming the harness and the missing key. Only their
runs SHALL be offered `reverify_fact`:

* `action = "next"` leases the oldest unleased suspect fact to the run, served
  in the SPEC-0027 REQ "Memory Data Wrapper", up to `reverify_per_run`;
* `action = "renew"` with `fact_id`, `reason` and, for span anchors, a new line
  range, re-resolves every changed anchor against the latest synced tree,
  resets churn, sets `confidence = 1.0`, and returns the fact to its prior
  status;
* `action = "end"` with `fact_id` and `reason` ends it with reason
  `reverify_end`.

`renew` and `end` SHALL require a lease held by the calling run. Leases lapse
when the run ends. A consolidator records facts with `record_fact` like any
agent, and they are untrusted (REQ-4).

#### Scenario: Only consolidators re-verify

- **WHEN** a harness not listed in `consolidators` calls `reverify_fact`
- **THEN** the tool is not registered for it, and the call fails as unknown

#### Scenario: An unpinned consolidator is refused

- **WHEN** `consolidators = ["mem-nightly"]` and `mem-nightly` has no
  `model_pin`
- **THEN** the configuration fails to load, naming `mem-nightly` and
  `model_pin`

### Requirement: REQ-17 — Weights Refresh Command

`harness memory weights refresh [--policy-repo NAME]` SHALL run in the client,
with the policy repo's `credential_file` (SPEC-0029 REQ "Policy Repo
Declaration And Sync"); the daemon SHALL NOT fetch rankings or hold that
credential. It SHALL fetch each built-in
source (`swe-bench-verified`, `aider-polyglot`), parse only model ids matching
`^[A-Za-z0-9._/:-]{1,128}$` and numeric scores, write a new
`weights/models.toml` that keeps every existing `family`, `aliases` and
`[[family_rule]]`, and open a pull request against the policy repo whose body
lists added and removed models and each `w0` change. No fetched text other than
model ids and numbers SHALL reach the file or the body. Weights SHALL change
only when that pull request merges and `harness policy sync` updates the
clone.

#### Scenario: Refresh proposes, never applies

- **WHEN** `harness memory weights refresh` completes
- **THEN** a pull request exists and the daemon's current `weights_sha` is
  unchanged

### Requirement: REQ-18 — Facts Are Not Grounding Evidence

A fact SHALL NOT count as grounding evidence under SPEC-0007 REQ "Grounding
Evidence", for any kind, scope or status. `harness distill` MAY include facts
in a dossier only in a section marked as context, and a candidate whose only
support is facts SHALL NOT be emitted.

#### Scenario: A fleet fact grounds no skill

- **WHEN** a live `fleet` fact describes a fix and no qualifying pull request
  exists
- **THEN** `harness distill` emits no candidate for it

### Requirement: REQ-19 — FactService And Gateway Tools

The daemon SHALL serve `FactService` over ConnectRPC (ADR-0034):

| RPC | Tier |
|---|---|
| `ListFacts` (filters: scope, status, kind, subject, predicate, since), `GetFact`, `ExplainFact`, `WatchFacts` (stream) | `read` |
| `ListPending` (REQ-14 notice), `AnchorTargets` (REQ-7) | `read` |
| `AssertFact`, `RetractFact` | `control`, operator caller only |
| `RecordFact`, `VoteFact`, `ReverifyFact` | gateway only; refused on the control plane |

The gateway SHALL expose `record_fact` (subject, predicate, value, evidence,
anchors) and `vote_fact` (fact_id, stance, reason, evidence) to attributed
callers whose harness does not set SPEC-0030's `memory_record = false`, and
`reverify_fact` to consolidators. Any fact content a tool returns SHALL be
inside SPEC-0027 REQ "Memory Data Wrapper". `vote_fact` on a fact outside the
caller's project and `fleet` SHALL fail as not found.

#### Scenario: The control plane cannot record as an agent

- **WHEN** a CLI client calls `FactService.RecordFact` over the Unix socket
- **THEN** it gets `PERMISSION_DENIED`

#### Scenario: Scope hides other projects

- **WHEN** a run in `forge.example/acme/web` votes on a
  `project:forge.example/acme/api` fact id
- **THEN** the call fails as not found

### Requirement: REQ-20 — The Facts CLI

`harness facts` SHALL provide, each with `--json`:

* `list [--scope S] [--status S] [--kind K] [--subject N] [--predicate P]`;
* `show <id>`: the fact, anchors with states, evidence with trust, status
  history;
* `assert --subject N --predicate P --value V [--scope S] [--anchor …]
  [--supersedes <id>]`: an `operator` fact; with only `--supersedes`, it copies
  that fact's subject, predicate and value, and the superseded fact ends. This
  is the operator action that promotes an untrusted fact;
* `retract <id> [--reason R]`;
* `pending`: pending promotions with notice state and time left; this is a
  notice (REQ-14);
* `why <id>`: status history, anchors and churn, votes grouped by family with
  weights, counted flags and reasons, S and each threshold, `weights_sha`,
  and the runs and trace events behind each vote.

#### Scenario: why shows the arithmetic

- **WHEN** `harness facts why <id> --json` is run on a pending fact
- **THEN** the output carries per-family capped sums, S, each threshold with
  its value, and `weights_sha`

### Requirement: REQ-21 — TUI Fact Views

The TUI SHALL add, as clients of `FactService`:

* a **fact timeline**: facts as horizontal bars over their validity windows,
  suspect spans hatched, `ended` bars closed, `retracted` bars in the danger
  role and marked;
* a **fact inspector**: fact → votes grouped by family with weights and capped
  sums → runs → trace events, each level entered with `↵` and left with `esc`;
* a **feed**: pending promotions with veto clocks (`awaiting notice`, time
  left, or `paused`), suspect `operator` facts, and facts with counted
  contradictions.

`↵` SHALL open the inspector and `x` SHALL retract the selected fact in one
keystroke. The feed SHALL call `ListPending` only while it is visible.

#### Scenario: Opening the feed is notice

- **WHEN** the operator opens the feed showing a fact awaiting notice
- **THEN** the fact's `notice_via` is `api` and its clock starts

### Requirement: REQ-22 — Operator Callers

An **operator caller** SHALL be a Unix-socket peer whose process is not a
descendant of any process the supervisor started (checked from the peer's pid,
`SO_PEERCRED` on Linux and `LOCAL_PEERPID` on macOS), or an authenticated TCP
client (ADR-0034) of any role. Only operator callers SHALL retract, assert
`operator` facts, or produce a notice.

#### Scenario: An agent's shell is not the operator

- **WHEN** a supervised agent runs `harness facts pending` from its shell
- **THEN** the listing is returned but records no notice, and `harness facts
  retract` from the same shell fails as not an operator caller

### Requirement: REQ-23 — Configuration

This spec owns these keys (SPEC-0029 and SPEC-0030 add others to `[memory]`):

```toml
[memory]
consolidators       = ["mem-nightly"]   # REQ-16; default []
reverify_per_run    = 5
max_records_per_run = 20
max_votes_per_run   = 50
suppress_retracted  = "90d"

[memory.expiry]
trusted_ttl   = "30d"
untrusted_ttl = "7d"
churn_decay   = 0.9     # 0 < x < 1
churn_floor   = 0.3     # 0 < x < 1

[memory.promotion]
weights_repo     = "fleet-policy"   # a [policy_repo.<name>]; unset = all unknown
min_repos        = 2
min_families     = 2
min_runs         = 3
family_cap       = 1.0
threshold        = 1.5
veto_window      = "72h"
digest_every     = "1h"
prior_min        = 0.5
prior_max        = 1.5
unknown_weight   = 0.25
beta_prior       = [4.0, 1.0]
multiplier_range = [0.25, 1.25]
```

A `weights_repo` naming no declared policy repo, an out-of-range value, or
`prior_min > prior_max` SHALL fail the load, naming the key. These tables SHALL
be global-only (ADR-0009) and rejected in a project `harness.toml`; promotion
thresholds SHALL come only from `[memory.promotion]`, never from a policy repo.
Durations SHALL parse as Harness durations with a `d` suffix allowed.
Transition reasons SHALL come from this set: `anchor_changed`, `span_changed`,
`dependency_changed`, `agent_version_changed`, `reverted`, `churn`, `ttl`,
`run_ended`, `superseded`,
`sync_absent`, `renewed`, `reverify_end`, `pending`, `thresholds_lost`,
`promoted`, `retracted`.

#### Scenario: A project cannot lower thresholds

- **WHEN** a project `harness.toml` declares `[memory.promotion] min_repos = 1`
- **THEN** the load fails, naming the table as global-only

### Requirement: REQ-24 — Metrics

The daemon SHALL export, per SPEC-0013, with no fact id, subject or value as a
label:

```text
harness_facts{kind,status,scope}                gauge    scope: project|fleet
harness_fact_writes_total{kind,result}          counter  created|reobserved|suppressed|rejected
harness_fact_votes_total{stance,result}         counter  counted|exposed|retracted|rejected
harness_fact_transitions_total{to,reason}       counter  reason: REQ-23 set
harness_fact_reverify_queue                     gauge
harness_fact_pending{clock}                     gauge    awaiting_notice|running|paused
harness_fact_anchor_checks_total{result}        counter  unchanged|moved|changed|reverted|unresolved
harness_fact_trigger_aborts_total{trigger}      counter
```

#### Scenario: Cardinality stays bounded

- **WHEN** ten thousand facts exist
- **THEN** every series' labels come from the fixed sets above

### Requirement: Error Handling Standards

- Errors SHALL be wrapped with context naming the fact id, harness and run
  where known.
- Sentinel errors SHALL exist for: unknown predicate, value too long, unknown
  subject, no project scope, unattributed caller, fact not found (also for
  out-of-scope), duplicate vote, kind not votable, run limit reached, not a
  consolidator, lease not held, not an operator caller, and store unavailable.
- A store failure SHALL fail the tool call with a tool error; it SHALL NOT
  block, stop or delay the calling agent, a spawn, or supervision.
- Multi-row mutations (a record with anchors, evidence and its vote; a sync's
  anchor application; a promotion) SHALL each be one transaction on the store's
  single writer, with parameterized queries.
- Logs SHALL be structured and SHALL NOT include fact values or reasons.

#### Scenario: A store outage does not hold an agent

- **WHEN** the store's writer is failing and a run calls `record_fact`
- **THEN** the call returns a store-unavailable tool error within the
  writer's timeout, and the run continues
