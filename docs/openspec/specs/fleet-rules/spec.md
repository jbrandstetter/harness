---
status: draft
date: 2026-09-27
implements: [ADR-0041]
extends: [SPEC-0003, SPEC-0021, SPEC-0022]
---

# SPEC-0029: Fleet Rules

## Overview

This spec adds rules: small, reviewed, deterministic checks over what the fleet
does, which can annotate the fleet graph, tell the operator, hold a harness or
stop a run.

* A **policy repo**: a git remote declared in a global-only
  `[policy_repo.<name>]` table, served from the default branch of a managed
  clone that only `harness policy sync` updates. Rules live under `rules/`;
  `weights/` belongs to SPEC-0028. A **starter pack** ships in the binary.
* A **rule file**: TOML naming one event kind, a CEL boolean `when`, a level
  (`annotate` < `notify` < `hold` < `stop`), a cooldown, a target and a
  fixed-field message.
* A **CEL environment** with typed variables per event kind, `now` supplied
  as an input, and three windowed aggregation primitives implemented in Go:
  `count`, `distinct` and `exists`. Rules are type-checked at load, cost-limited
  per evaluation, and cannot loop or perform I/O.
* A **rule journal**: every input a rule sees is one row in a single ordered
  table, so `harness rules backtest` replays exactly what live evaluation saw.
* **Shadow first**, and a hand-written `[memory.rules] enforce` list: a rule
  acts at `hold` or `stop` only when the operator named it there.
* A **rule miner** one-shot whose only write is the `propose_rule` gateway tool,
  and `harness rules propose`, a client command that opens a pull request
  carrying the rule and its would-have-fired list.

See ADR-0041 ("Rules", "Where the human is") for the decision and the options
it rejected. The policy repo reuses the shapes of SPEC-0007 REQ "Skill Repos",
REQ "Default-Branch Gate", REQ "Pull Request Proposals" and REQ "Review
Feedback And Suppression". This spec amends SPEC-0003 REQ "Operator
Notification", SPEC-0021 REQ-4 and SPEC-0022 REQ-5. The loop guard is not
changed.

Requirements are numbered. Cite them as `SPEC-0029 REQ-n`.

## Requirements

### Requirement: REQ-1 — Policy Repo Declaration And Sync

The global configuration SHALL accept any number of `[policy_repo.<name>]`
tables, `<name>` matching `^[a-z][a-z0-9-]*$`, with `remote` (required, a git
URL) and `branch` (optional; default the remote's default branch as recorded
when the clone was created). The keys that `harness rules propose` uses
(REQ-16) are `credential_file`, `reviewers`, `labels`, `public` (unset is
treated as `true`, following SPEC-0007), `branch_prefix` (default
`harness-rules/`) and `max_proposals_per_week` (default 3); SPEC-0028's
`harness memory weights refresh` reads the same `credential_file`. Any other
key SHALL fail the load, naming it.

`[policy_repo.*]` SHALL be rejected in a project `harness.toml`, in a
`harness_d` drop-in and on the project-up wire, joining ADR-0009's global-only
list, because a cloned repository must not choose the rules that watch it.

`harness policy sync [name]` SHALL be the only operation that touches a policy
repo's remote. It SHALL clone into `$XDG_STATE_HOME/harness/policy/<name>/`
when the clone is absent, and otherwise fetch and fast-forward `branch`. It
SHALL fail without modifying the clone on a non-fast-forward state. After a
fast-forward it SHALL call `RuleService.PolicySynced` (REQ-17); with no daemon
running it SHALL succeed and the daemon SHALL load the clone at its next start.
The daemon MUST NOT fetch, pull, commit or push in a policy clone.

#### Scenario: A project file cannot declare a policy repo

- **WHEN** a project `harness.toml` declares `[policy_repo.evil] remote =
  "https://attacker.example/rules.git"`
- **THEN** `harness up` fails naming `[policy_repo.*]` as global-only, and
  nothing is cloned

#### Scenario: Sync is the only fetch

- **WHEN** the daemon starts, reloads, or evaluates rules for a week with no
  `harness policy sync`
- **THEN** no git network operation runs in `$XDG_STATE_HOME/harness/policy/`

### Requirement: REQ-2 — Rule Sources, Layout And Override

The daemon SHALL read rule files from each policy clone's `rules/<name>.toml`
(top level of `rules/` only), at the checked-out tip of the configured
`branch`. If a clone's `HEAD` is not that branch, or its working tree is
dirty, the daemon SHALL keep that repo's previously loaded rules and
`harness doctor` SHALL warn, as SPEC-0007 REQ "Default-Branch Gate" does for
skill repos. Directories other than `rules/` SHALL be ignored by this spec;
`weights/` is read by SPEC-0028 (via `[memory.promotion] weights_repo`), and no
policy repo file sets a promotion threshold.

A starter pack of rule files SHALL be embedded in the binary (REQ-14). A
policy-repo rule whose `name` equals a starter rule's SHALL replace it
entirely. Two policy repos defining the same `name` SHALL disable both copies
with reason `duplicate_name`. A rule named in the global `[memory.rules]
disable` list SHALL NOT be evaluated, whatever its source.

The rule set SHALL be (re)loaded at daemon start, on `harness reload`, and on
`RuleService.PolicySynced`. A reload SHALL swap the rule set atomically between
two journal rows (REQ-7).

#### Scenario: A policy rule overrides a starter rule

- **WHEN** the policy repo `ops` holds `rules/same-file-collision.toml`
- **THEN** `harness rules list` shows one `same-file-collision`, with source
  `policy:ops`, and the embedded copy is not evaluated

#### Scenario: A dirty clone keeps the last good rules

- **WHEN** an operator edits `rules/churn.toml` in the policy clone by hand
  without committing
- **THEN** the daemon keeps evaluating the rules it loaded before, and
  `harness doctor` warns that the `ops` clone is dirty

#### Scenario: An operator silences a starter rule without a policy repo

- **WHEN** no policy repo is declared and `[memory.rules] disable =
  ["cross-run-churn"]`
- **THEN** `cross-run-churn` is listed as `disabled: operator` and never fires

### Requirement: REQ-3 — Rule File Schema

A rule file SHALL be TOML with exactly these keys:

| Key | Required | Value |
|---|---|---|
| `name` | yes | `^[a-z][a-z0-9-]{0,62}$`, equal to the file's basename |
| `description` | yes | one line, at most 200 characters |
| `on` | yes | `run_event`, `graph_event`, `fact_event` or `tick` (REQ-4) |
| `when` | yes | a CEL expression of type `bool` (REQ-4 to REQ-6) |
| `level` | yes | `annotate`, `notify`, `hold` or `stop` (REQ-10) |
| `cooldown` | no | a duration from `1m` to `7d`; default `1h` (REQ-13) |
| `target` | yes | `run`, `harness`, `node` or `fleet` |
| `message` | yes | a template of at most 280 characters |

`target` names what the action applies to, derived from the event: `run` is
the event's `run`; `harness` is the event's `harness`; `node` is the graph node
the event is about (the file for a graph event with a `path`, the fact's
subject for a fact event, the run node otherwise); `fleet` is no particular
object. `node` and `fleet` SHALL be accepted only with `level` `annotate` or
`notify`, and `fleet` only with `notify`. A fire whose event lacks the field its
target needs SHALL be recorded with action `no_target` and SHALL take no action.

`message` SHALL accept only the placeholders `{{rule}}`, `{{level}}`,
`{{harness}}`, `{{run}}`, `{{repo}}`, `{{branch}}`, `{{path}}`, `{{type}}` and
`{{at}}`, each rendered from the event's identifier fields; an absent value
renders empty. No placeholder SHALL render free text from a trace, a fact
value, or any other agent-written field.

A file that violates this table, has an unknown key, or uses an unknown
placeholder SHALL disable that rule with reason `schema`, naming the file and
the key; the other rules SHALL load.

#### Scenario: An unknown key disables one rule

- **WHEN** `rules/churn.toml` declares `action = "restart"`
- **THEN** `churn` is listed `disabled: schema (unknown key action)`, and every
  other rule in the repo loads

#### Scenario: A message cannot quote agent text

- **WHEN** a rule's message is `"{{harness}} said {{summary}}"`
- **THEN** the rule is disabled with reason `schema`, naming `{{summary}}`

### Requirement: REQ-4 — Event Kinds And The CEL Environment

Each rule SHALL be compiled with `cel-go` against an environment for its `on`
kind that declares exactly two variables: `event`, an object type fixed per
kind, and `now`, a CEL `timestamp`. `now` SHALL be bound from the journal row
being evaluated (REQ-7). The CEL standard library has no clock
function, and the daemon SHALL register none, so a rule cannot read the time
any other way. The environment SHALL contain the CEL standard library, its
standard macros (`has`, `all`, `exists`, `exists_one`, `map`, `filter`), and
the REQ-5 primitives, and nothing else: no extension library, no I/O, no
map-typed variable. Every macro iterates a list carried in `event`, whose
length the journal bounds (REQ-7), so no rule can loop beyond its input.

Every `event` carries `type` (string) and `at` (timestamp, the source's own
time). Fields a given `type` does not set SHALL read as the zero value (`""`,
`0`, `false`, `[]`), never as an error. Per kind:

* `run_event` — `type` is `tool`, `mark`, `opened`, `closed` or
  `loop_stopped`. All carry `harness`, `run` (`<harness>#<run id>`),
  `adapter`, `repo`, `branch`, `session`. `tool` adds `tool`, `action`
  (agent-trace's `search`, `read`, `edit`, `exec`, `verify`, `other`),
  `is_error`, `digest` (the input digest) and `paths` (repo-relative, at most
  64). `mark` adds `mark` (the mark type). `opened` adds `trigger`. `closed`
  adds `outcome` and `reason` (SPEC-0022 REQ-5), `exit_code` (int) and
  `cause_digest` (SHA-256 of the redacted last output line the supervisor keeps
  for notifications, empty when none). `loop_stopped` adds `tool` and `digest`.
* `graph_event` — `type` is `edge_added`, `edge_ended`, `collision_started` or
  `collision_ended`, per SPEC-0027 REQ "Collisions And Graph Events".
  Fields: `edge` (the edge kind), `from`, `to` (node ids), `repo`, `branch`,
  `path`, `harness`, `run`, and for collisions `scope` (`file` or `branch`),
  `harnesses` and `runs` (at most 16 each), where `run` and `harness` are the
  ones whose event started or ended the collision.
* `fact_event` — `type` is `recorded`, `status_changed` (one SPEC-0028
  `fact_events` row) or `voted` (one `fact_votes` row), per SPEC-0028 REQ "The
  Facts Table" and REQ "Exposure, Votes And Independence". Fields: `fact`,
  `kind`, `predicate`, `scope`, `status`, `previous_status`, `reason`,
  `stance`, `subject` (a node id), `repo`, `untrusted` (bool), and `harness`
  and `run` (the writer or voter). Never the fact's value text.
* `tick` — `type` is `tick`. No other fields.

#### Scenario: A misspelled field fails at load, not at 3 a.m.

- **WHEN** a `run_event` rule's `when` is `event.outcom == "failed"`
- **THEN** the rule is disabled with reason `compile`, naming the undefined
  field, before any event is evaluated

#### Scenario: Time comes only from now

- **WHEN** a rule is written `timestamp("2026-09-27T00:00:00Z") < now`
- **THEN** it compiles, and its result for a given journal row is the same in
  live evaluation and in any later backtest

#### Scenario: An unset field is a zero, not an error

- **WHEN** `event.exit_code == 0` is evaluated for a `run_event` of type `tool`
- **THEN** it evaluates `true` and no evaluation error is counted

### Requirement: REQ-5 — Windowed Aggregation Primitives

The environment SHALL provide three global functions, implemented in Go over
the rule journal:

```text
count(sel, within)           -> int    rows matching sel in the window
distinct(field, sel, within) -> int    distinct values of field among them
exists(sel, within)          -> bool   count(sel, within) > 0
```

* `sel` SHALL be a CEL map literal whose keys are string literals naming
  indexed fields: `kind`, `type`, `harness`, `run`, `repo`, `branch`, `path`,
  `tool`, `action`, `is_error`, `digest`, `outcome`, `cause_digest`, `edge`,
  `predicate`, `status`, `scope`. Values MAY be any expression of the field's
  type (`is_error` is `bool`, the rest are `string`). `kind` defaults to the
  rule's `on`. A `path` key matches a row when any of its paths is equal. All
  keys SHALL match (conjunction, equality only).
* `field` SHALL be a string literal from the same set.
* `within` SHALL be a literal `duration("…")` from `1m` to `7d`.
* The window SHALL be the journal rows whose `at` is in `(now - within, now]`
  and whose `seq` is at most the evaluated row's `seq`, the evaluated row
  included.
* `count` SHALL saturate at 10,000 and `distinct` at 1,000.
* A rule SHALL call primitives at most 8 times. A non-literal key, field or
  window, an unknown key, or a ninth call SHALL fail compilation.

The global `exists(sel, within)` is a different overload from CEL's
receiver-style macro `list.exists(x, p)`; `cel-go` keys macros by name, argument
count and receiver style, so both are available.

#### Scenario: A computed window is rejected

- **WHEN** a rule calls `count({"type": "closed"}, duration(event.tool))`
- **THEN** it is disabled with reason `compile`, naming the window as
  non-literal

#### Scenario: The current row counts

- **WHEN** the third failing `closed` row for `nightly` in one hour is
  evaluated by `count({"type": "closed", "harness": event.harness, "outcome":
  "failed"}, duration("1h")) >= 3`
- **THEN** the rule fires on that row, not on the next one

### Requirement: REQ-6 — Compilation, Cost Limits And Isolation

At load the daemon SHALL, for each rule: parse and type-check `when` with
`Env.Compile` and require the output type `bool`; apply AST validators for the
REQ-5 literal rules and a comprehension nesting limit of 2; and compute a
static cost with `Env.EstimateCost`, in which each primitive call is charged a
fixed 100 units. A rule whose estimated maximum cost exceeds the per-evaluation
limit of 5,000 units SHALL be disabled with reason `cost_estimate`. Every
program SHALL be built with `cel.CostLimit(5000)`, and the primitives SHALL be
charged the same 100 units at run time, so estimate and runtime agree.

An evaluation that exceeds the cost limit SHALL abort, count as a non-fire, and
disable the rule with reason `cost_exceeded` until the next rule-set load. An
evaluation that returns a CEL error, or whose primitive fails to read the
store, SHALL count as a non-fire, SHALL be recorded as `eval_error` or
`store_error` against the row, and SHALL be logged at most once per rule per
hour. A failure of one rule SHALL NOT stop, delay past its own evaluation, or
change the result of any other rule.

#### Scenario: One bad rule does not stop the rest

- **WHEN** `rules/a.toml` fails to compile and `rules/b.toml` is valid
- **THEN** `b` evaluates normally, and `harness doctor` names `a` and the
  compile error

#### Scenario: A runtime cost overrun disables only that rule

- **WHEN** a rule's evaluation exceeds 5,000 units on one row
- **THEN** that row is a non-fire for it, the rule is listed `disabled:
  cost_exceeded`, and the other rules evaluate the same row

### Requirement: REQ-7 — The Rule Journal And Evaluation Order

Every input a rule can see SHALL first be written as one row of the
`rule_journal` table in the ADR-0037 store: an `INTEGER PRIMARY KEY` `seq`; the
event `kind` and `type`; `at`, the daemon's clock when the row was admitted,
made non-decreasing in `seq` order; the REQ-5 indexed fields (paths in a child
table); and the REQ-4 `event` fields as JSON. Rows SHALL hold identifiers,
enums, digests and repo-relative redacted paths only, never summaries, notes,
output lines or fact values. Sources:

* `tool` and `mark` rows from a journaling subscriber on the observer;
* `opened` and `closed` rows from the run ledger's commits (SPEC-0022);
* `loop_stopped` rows copied, in order, from SPEC-0027's source journal
  (SPEC-0027 REQ "Projection And Rebuild"), the one capture of loop-guard
  trips;
* `graph_event` rows from SPEC-0027 and `fact_event` rows from SPEC-0028;
* one `tick` row per wall-clock minute while the daemon runs.

Journaling SHALL NOT block the observer, the supervisor or any other
subscriber: a full queue drops the row and counts it (REQ-20). The evaluator
SHALL consume committed rows in `seq` order and, for each row, evaluate every
loaded rule whose `on` matches, in lexical order of rule name, with `now` set
to the row's `at`; it SHALL then apply actions (REQ-10) and hold
re-evaluations (REQ-11) for that row before the next. It SHALL persist its
cursor, and after a restart SHALL evaluate the rows committed after the cursor
before new ones. The fires one row produced, their holds and cooldowns, and
the cursor SHALL commit in one transaction, before any notification is queued
or stop issued. Primitive queries SHALL be parameterized and index-served.
`[memory.rules] journal_retention` (default `45d`, minimum `37d`) SHALL bound
the table; pruning follows ADR-0037's batched retention.

#### Scenario: A dropped row is dropped for everyone

- **WHEN** the journal queue is full and an observer `tool` event is dropped
- **THEN** no rule sees it live, no backtest sees it later, and
  `harness_rule_journal_dropped_total{source="observer"}` increments

#### Scenario: Order is by sequence, not source timestamp

- **WHEN** a trace `tool` row with an earlier source `at` is admitted after a
  ledger `closed` row
- **THEN** the evaluator evaluates the `closed` row first, and so does every
  backtest

#### Scenario: A crash loses no evaluation

- **WHEN** the daemon crashes after committing rows 900 to 910 and before
  evaluating them
- **THEN** after restart rows 900 to 910 are evaluated with their own `at`
  as `now`, before any newer row

### Requirement: REQ-8 — Backtest

`harness rules backtest <file> [--since <duration>] [--json]` SHALL compile the
file with the REQ-4 to REQ-6 checks and ask `RuleService.Backtest` to replay the
journal rows admitted in the last `--since` (default `30d`) through that one
rule, in `seq` order, starting with no cooldowns and no holds. Windows at the
start SHALL look back into older rows. `--since` longer than
`journal_retention` minus `7d` SHALL be refused. The result SHALL list each
fire: `at`, journal `seq`, kind and type, target, the effective level the
current enforce list would give it (REQ-10), and for a `hold` rule the
simulated hold span. It SHALL also report the number of rows evaluated,
`eval_error` and `store_error` counts, and zero fires plainly.

For a rule with content hash `H` (REQ-9) loaded continuously over an interval,
with no cooldown entry for `H` at its start, the backtest's fires over that
interval SHALL equal, row for row and target for target, the fires live
evaluation recorded for `H`, in shadow and active modes alike, except rows
recorded as `store_error`. A backtest SHALL take no action and write no fire,
hold or annotation. With the daemon unreachable, the CLI SHOULD run the same
replay over a read-only connection to the store.

#### Scenario: Backtest reproduces live fires

- **WHEN** `same-file-collision` ran live for 30 days, then its file is
  backtested with `--since 30d`
- **THEN** the backtest lists exactly the `(seq, target)` pairs in its live
  fire log for those 30 days

#### Scenario: A silent rule says so

- **WHEN** a backtest over 30 days produces no fire
- **THEN** the output states `0 fires in 30d over <n> rows` and exits 0

### Requirement: REQ-9 — Shadow Mode

A rule's content hash SHALL be SHA-256 over a canonical encoding of `on`, the
normalized `when` (the `cel-go` AST printed with `cel.AstToString`), `level`,
`target` and `cooldown`. `name`, `description` and `message` SHALL NOT be part
of it. The first time the daemon loads a hash for a name, it SHALL record
`shadow_since` as the `at` of the next journal row. Until `shadow_since +
[memory.rules] shadow_period` (default `7d`; `"0s"` turns shadowing off), fires
SHALL be recorded with mode `shadow` and SHALL take no action at any level. A
hash that has completed its shadow period once SHALL be active whenever it is
loaded again. Shadow fires SHALL respect the cooldown exactly as active fires
do. A starter rule embedded in the binary (REQ-14) SHALL NOT shadow, because it
is reviewed code in a release; a policy-repo rule that overrides one SHALL
shadow as usual.

#### Scenario: A merged rule waits a week

- **WHEN** a new `rules/verify-flake.toml` is synced on 2026-10-01 with
  `level = "notify"`
- **THEN** its fires until 2026-10-08 appear in the TUI feed as shadow fires,
  and no `rule_fired` notification is sent

#### Scenario: Rewording keeps its clock

- **WHEN** only a rule's `description` and `message` change
- **THEN** its hash, and so its shadow or active mode, is unchanged

#### Scenario: A changed expression starts over

- **WHEN** an active rule's `when` changes from `>= 3` to `>= 2`, or a policy
  repo overrides starter rule `loop-stop-recurs`
- **THEN** the new hash is in shadow for `shadow_period`

### Requirement: REQ-10 — Action Levels And The Enforce List

Levels SHALL be cumulative: `annotate` attaches an annotation (rule, message,
fire id, `at`) to the target's graph node, shown in the TUI graph browser;
`notify` also delivers `rule_fired` (REQ-13); `hold` also holds the target's
harness (REQ-11); `stop` also stops the target run (REQ-12). A `fleet` target
has no node and is not annotated.

A rule's **effective level** SHALL be its declared level, capped at `notify`
unless its `name` is listed in the global `[memory.rules] enforce`. In shadow
no level acts. `enforce` SHALL be a list of rule names, SHALL be rejected in a
project file, a `harness_d` drop-in and on the project-up wire, and SHALL be
applied on reload. No command, RPC or tool defined by any spec SHALL write
`enforce`. A name in `enforce` that matches no loaded rule SHALL be logged at
WARN on every load and reported by `harness doctor` as `fail`.

#### Scenario: An unlisted stop rule only notifies

- **WHEN** an active policy rule declares `level = "stop"` and is not in
  `enforce`
- **THEN** its fires notify and annotate, the fire log records declared
  `stop` and effective `notify`, and no run is stopped

#### Scenario: Enforce is hand-written and global

- **WHEN** a project `harness.toml` declares `[memory.rules] enforce =
  ["restart-into-same-failure"]`
- **THEN** `harness up` fails naming `[memory.rules]` as global-only

### Requirement: REQ-11 — Holds

When a rule fires at effective level `hold` with target `run` or `harness`,
the daemon SHALL hold that harness: it SHALL persist `(rule, hash, harness, fire
id, since)` in the store, where it survives daemon restarts. While any hold is
on a harness:

* every firing of a scheduled or triggered harness SHALL be recorded `skipped`
  with reason `rule_hold`, evaluated in SPEC-0021 REQ-4's admission order
  beside SPEC-0020's `model_hold`, between operating hours and quota parks;
* a resident harness's restart policy SHALL NOT respawn it after it exits;
* `harness start`, `restart` and `trigger` SHALL be refused with an error
  naming the rule and `harness rules clear <harness>`;
* a process already running SHALL NOT be stopped by the hold.

A hold SHALL be released when (a) on every `tick` row for a continuous span of
the rule's `cooldown`, re-evaluating the rule's `when` with the firing event's
variables and the tick's `now` returns `false`; (b) the operator runs `harness
rules clear <harness> [--rule <name>]`; or (c) a reload leaves the rule absent,
disabled, changed in hash, or out of `enforce`. Each release SHALL be logged at
WARN naming the cause and emit a `rule_hold_released` event.

#### Scenario: Firings under a hold are skipped

- **WHEN** `budget-exhaustion-recurring` is enforced and holds `nightly`, and
  its schedule fires twice
- **THEN** both firings are recorded `skipped` with reason `rule_hold`, and
  nothing spawns

#### Scenario: A hold clears when its evidence ages out

- **WHEN** a hold was placed because `count(…, duration("24h")) >= 2` and both
  counted rows become older than 24 hours, and the rule stays false for its
  `6h` cooldown on every tick
- **THEN** the hold is released with cause `condition_cleared`

#### Scenario: Start does not bypass a hold

- **WHEN** the operator runs `harness start nightly` while it is held
- **THEN** the command fails naming the rule and `harness rules clear
  nightly`, and `harness rules clear nightly` then releases it

### Requirement: REQ-12 — Stops, And The Loop Guard Unchanged

When a rule fires at effective level `stop`, and the target run is the
harness's run in flight, the daemon SHALL stop that harness through the
supervisor's `Stop`, the call the loop guard makes, which clears its enabled
intent. The run SHALL close `cancelled` with reason `rule_stop` (amending
SPEC-0022 REQ-5), and the daemon SHALL log an ERROR and a lifecycle line in the
harness's own log naming the rule, fire id and journal `seq`. If the target run
has already ended or been replaced, the fire SHALL be recorded with action
`target_gone` and nothing SHALL be stopped. Target `harness` SHALL stop the run
in flight, if any.

The loop guard SHALL keep its threshold, streak rules, stop and `loop_stopped`
notification unchanged. It SHALL NOT consult rules, and no rule SHALL configure
it. Its trips reach rules only as `loop_stopped` journal rows. A rule stop SHALL
NOT send `loop_stopped` or `recovered`.

#### Scenario: A late stop does not hit the next run

- **WHEN** an enforced stop rule fires for run `impl#41` after `impl#41`
  closed and `impl#42` started
- **THEN** `impl#42` keeps running, and the fire is recorded `target_gone`

#### Scenario: Both guards see a loop

- **WHEN** the loop guard stops `worker` and an enforced rule counting
  `loop_stopped` rows fires for the same harness
- **THEN** the harness is stopped once, `loop_stopped` is sent by the guard,
  and `rule_fired` is sent by the rule

### Requirement: REQ-13 — Cooldown And The rule_fired Notification

After a fire, the same rule hash SHALL NOT fire again for the same target key
(run id, harness, node id, or `fleet`) until `cooldown` has passed in journal
time; a match inside the cooldown is not a fire and is counted as
`suppressed`. Cooldown entries SHALL persist across restarts.

An active fire at effective level `notify` or above SHALL deliver the notify
event `rule_fired`, amending SPEC-0003 REQ "Operator Notification". Its JSON
SHALL carry the standard fields (`harness` is the target's harness, empty for a
`node` or `fleet` target without one; `message` is the rendered, redacted
rule message) plus `rule`, `level` (effective), `declared_level`, `fire_id`,
`action` and `hint` (`harness rules explain <fire_id>`). `rule_fired` SHALL join
the default `[notify] events`. The notify cooldown for `rule_fired` SHALL be
keyed by harness, event and rule, so two rules on one harness do not suppress
each other.

#### Scenario: One alert per cooldown

- **WHEN** `repeated-verify-failure` (cooldown `2h`) matches `impl` five times
  in 30 minutes
- **THEN** one `rule_fired` is delivered, and four matches are counted
  `suppressed`

#### Scenario: The hook can explain itself

- **WHEN** the notify hook receives `rule_fired` with `fire_id` `f-1832`
- **THEN** `harness rules explain f-1832` shows the rule, its hash, the
  journal row, `now`, each primitive's result and the action taken

### Requirement: REQ-14 — Starter Pack

The binary SHALL embed these rules, evaluated like any other rule except that
they skip shadow (REQ-9); they are capped by `enforce` and overridable by
name:

| Name | On | Level | Target | Cooldown |
|---|---|---|---|---|
| `same-file-collision` | `graph_event` | `annotate` | `run` | `30m` |
| `repeated-verify-failure` | `run_event` | `notify` | `harness` | `2h` |
| `restart-into-same-failure` | `run_event` | `hold` | `harness` | `1h` |
| `budget-exhaustion-recurring` | `run_event` | `hold` | `harness` | `6h` |
| `cross-run-churn` | `graph_event` | `annotate` | `node` | `24h` |
| `loop-stop-recurs` | `run_event` | `notify` | `harness` | `12h` |

Their `when` expressions SHALL be:

```text
same-file-collision:
  event.type == "collision_started" && event.scope == "file"

repeated-verify-failure:
  event.type == "tool" && event.action == "verify" && event.is_error
  && distinct("run", {"type": "tool", "action": "verify", "is_error": true,
       "repo": event.repo, "digest": event.digest}, duration("2h")) >= 2
  && !exists({"type": "tool", "action": "verify", "is_error": false,
       "repo": event.repo, "digest": event.digest}, duration("2h"))

restart-into-same-failure:
  event.type == "closed" && event.outcome == "failed"
  && event.cause_digest != ""
  && count({"type": "closed", "harness": event.harness, "outcome": "failed",
       "cause_digest": event.cause_digest}, duration("1h")) >= 3

budget-exhaustion-recurring:
  event.type == "closed" && event.outcome == "budget_exceeded"
  && count({"type": "closed", "harness": event.harness,
       "outcome": "budget_exceeded"}, duration("24h")) >= 2

cross-run-churn:
  event.type == "edge_added" && event.edge == "edited"
  && distinct("run", {"type": "edge_added", "edge": "edited",
       "repo": event.repo, "path": event.path}, duration("24h")) >= 4

loop-stop-recurs:
  event.type == "loop_stopped"
  && count({"type": "loop_stopped", "harness": event.harness},
       duration("24h")) >= 2
```

#### Scenario: An empty policy repo still sees collisions

- **WHEN** no policy repo is declared and two runs from different harnesses
  start editing `internal/store/store.go`
- **THEN** `same-file-collision` annotates the run whose edit started the
  collision, from its first load, and sends no `rule_fired`: paging on
  collisions is SPEC-0027's opt-in `collision` event

#### Scenario: A hold-level starter rule needs the operator

- **WHEN** `nightly` closes `failed` three times in an hour with one
  `cause_digest`, and `restart-into-same-failure` is active but not enforced
- **THEN** the operator is notified and `nightly`'s next firing still spawns

### Requirement: REQ-15 — Rule Miners And propose_rule

`[memory] rule_miners` SHALL list global harness names. Each SHALL be a
scheduled prompt one-shot (ADR-0013: `schedule` plus `prompt` or
`prompt_file`); anything else, or a name in a project file, SHALL fail the
load. Only a caller whose token belongs to a run of a listed harness (SPEC-0005
REQ "Caller Identity") SHALL see the gateway tool `propose_rule`; for every
other caller it SHALL NOT be registered. The miner reads through the read tools
its policy grants, including SPEC-0027's.

`propose_rule` SHALL accept the REQ-3 fields plus `rationale` (at most 2,000
characters), `evidence` (1 to 50 ids of runs, journal rows, fires or graph
nodes) and `dry_run`. At store time the daemon SHALL reject, with a named
error: a `level` other than `annotate` or `notify` (`invalid_level`); any REQ-3
schema, REQ-4 compile or REQ-6 cost failure; an evidence id that resolves to
nothing; more than 10 proposals from one run; and more than 50 stored,
unproposed proposals (`proposal_backlog_full`). A valid call SHALL store the
proposal with the caller's run id, flagged untrusted, and return its id, content
hash and a 30-day backtest summary (fire count and the first 10 fires) wrapped
per SPEC-0027 REQ "Memory Data Wrapper". `dry_run` SHALL return the same
without storing. A stored proposal SHALL never be evaluated live.

#### Scenario: A miner cannot propose a stop

- **WHEN** the miner calls `propose_rule` with `level = "stop"`
- **THEN** the call fails with `invalid_level`, and nothing is stored

#### Scenario: Only miners see the tool

- **WHEN** a harness not listed in `rule_miners` lists its gateway tools
- **THEN** `propose_rule` is absent

### Requirement: REQ-16 — harness rules propose

`harness rules propose [--policy-repo <name>]` SHALL be a client command, and
the only holder of the forge credential, read from the policy repo's
`credential_file` and never placed in any environment. `--policy-repo` SHALL be
required when more than one is declared. For each stored proposal, oldest
first, it SHALL: recompile it; backtest it over 30 days through
`RuleService.Backtest`; and open a pull request on a branch
`<branch_prefix><name>` cut from a freshly fetched default branch in a proposal
clone under its state directory, adding or replacing only `rules/<name>.toml`.
The body SHALL carry the marker `<!-- harness-rules name=<name>
hash=<expression hash> -->`, the rationale fenced as untrusted text, the
evidence ids, the backtest summary and the would-have-fired list (up to 200
rows, with the total), and a statement that the rule will shadow for
`shadow_period` and cannot hold or stop unless the operator lists it in
`enforce`. When `public` is true, fire rows SHALL omit `repo`, `branch` and
`path`.

The expression hash SHALL be SHA-256 over `on` and the normalized `when`. The
command SHALL: push a new commit to an open pull request with the same `name`
instead of opening another; skip a proposal whose expression hash equals a
rule already on the default branch (`duplicate`); count pull requests it opened
for that repo in the last 7 days against `max_proposals_per_week`, leaving the
rest `waiting`; request `reviewers` and apply `labels`, failing if a reviewer
equals the token's own login. It SHALL NOT force-push, merge, approve or arm
auto-merge. A pull request closed unmerged SHALL mark its expression hash
`rejected` for that repo, keeping the last review comment as the reason, and
no later proposal with that hash SHALL be proposed there. Proposal states SHALL
be recorded through `RuleService.RecordProposal`.

#### Scenario: A rejection is remembered

- **WHEN** a proposal's pull request is closed without merging, and the miner
  later stores a proposal with the same `on` and `when` under a new name
- **THEN** `harness rules propose` opens no pull request for it and reports
  `suppressed`, linking the closed pull request

#### Scenario: The cap holds

- **WHEN** three rule pull requests were opened against `ops` in the last 7
  days and two more proposals are stored
- **THEN** none is opened, and both are listed `waiting`

### Requirement: REQ-17 — RuleService

The daemon SHALL serve `RuleService` over the ConnectRPC control plane
(ADR-0034), and the CLI, TUI and gateway tool SHALL use it or its handlers,
never a private path:

| RPC | Tier | Does |
|---|---|---|
| `ListRules`, `GetRule` | `read` | rules: source, hash, mode, levels, errors |
| `ListFires`, `ExplainFire` | `read` | the fire log; one fire with its inputs |
| `Backtest` | `read` | REQ-8, streamed; at most 2 concurrent |
| `ListHolds`, `ListAnnotations` | `read` | holds; annotations by node id |
| `ListProposals` | `read` | proposals with state and last backtest |
| `ClearHold` | `control` | REQ-11 (b) |
| `PolicySynced` | `control` | reload after `harness policy sync` |
| `RecordProposal` | `control` | pull request number and state |

No RPC SHALL write `enforce`, a rule file or the journal, or store a proposal;
`propose_rule` stores proposals in process. The event stream SHALL gain
`rule_fired`, `rule_hold` and `rule_hold_released`, bumping the protocol minor
version.

#### Scenario: A read client cannot clear a hold

- **WHEN** a `read`-tier TCP client calls `ClearHold`
- **THEN** it receives `PERMISSION_DENIED` before the handler runs

#### Scenario: Backtests are bounded

- **WHEN** a third concurrent `Backtest` arrives
- **THEN** it fails with `RESOURCE_EXHAUSTED`

### Requirement: REQ-18 — CLI

The CLI SHALL provide `harness rules list`, `show <name>`, `backtest <file>`
(REQ-8), `propose` (REQ-16), `clear <harness> [--rule <name>]` and `explain
<fire-id>|--harness <name>`, and `harness policy sync [name]` (REQ-1). Every
command SHALL accept `--json`.

* `list` SHALL show each rule's name, source (`starter` or `policy:<name>`),
  short hash, mode (`shadow until <time>`, `active`, or `disabled: <reason>`),
  declared and effective level, and last fire.
* `show` SHALL add the file, cost estimate, `shadow_since`, enforce status and
  current holds, and SHALL warn when a `hold` rule's `when` uses no primitive,
  since such a hold can only clear by hand.
* `explain` SHALL show the rule and hash, the journal row and `now`, each
  primitive call with its arguments and result, the mode and effective-level
  derivation, and the action outcome; with `--harness`, it explains each
  current hold.

#### Scenario: Explaining a capped fire

- **WHEN** `harness rules explain f-77` names a fire of an unenforced `stop`
  rule
- **THEN** the output states declared `stop`, effective `notify`, and that the
  rule is absent from `[memory.rules] enforce`

### Requirement: REQ-19 — TUI, List And Doctor Visibility

The TUI feed SHALL show stored and proposed rule proposals with their backtest
summaries and pull request links, shadow fires grouped by rule with the time
each rule goes active, and current holds, from which the operator can call
`ClearHold`. The graph browser SHALL show a node's annotations with rule,
message and time, and SHALL hop from an annotation to its fire's explanation.
`harness list` SHALL show a held harness in its STATE cell without adding a
column. `harness doctor` SHALL show one rules row: `fail` when an `enforce`
name matches no loaded rule or an enforced rule is disabled; `warn` when any
rule is disabled, any harness is held, a policy clone is dirty or off its
branch, or journal rows were dropped since start; `pass` otherwise, with the
names in its detail.

#### Scenario: A disabled enforced rule fails doctor

- **WHEN** an enforced rule fails to compile after a policy sync
- **THEN** `harness doctor` shows the rules row `fail`, naming it, and exits
  non-zero

### Requirement: REQ-20 — Metrics

On the existing `/metrics` listener (SPEC-0013), the daemon SHALL export:

```text
harness_rule_evaluations_total{rule,result}   counter
harness_rule_fires_total{rule,mode,level}     counter
harness_rule_eval_duration_seconds            histogram
harness_rules_loaded{state}                   gauge
harness_rule_holds_active                     gauge
harness_rule_journal_rows_total{source}       counter
harness_rule_journal_dropped_total{source}    counter
harness_rule_journal_lag_seconds              gauge
harness_rule_proposals_total{result}          counter
```

`result` on evaluations is `false`, `fired`, `suppressed`, `eval_error`,
`store_error` or `cost_exceeded`; `mode` is `shadow` or `active`; `level` is
the effective level; `state` is `active`, `shadow` or `disabled`; `source` is
`observer`, `ledger`, `loopguard`, `graph`, `facts` or `tick`; `result` on
proposals is `stored`, `dry_run` or `rejected`. The lag gauge is the daemon
clock minus the `at` of the last evaluated row.

`rule` values SHALL be capped at 100 per daemon, with overflow collapsed into
`rule="__other__"`, following SPEC-0013 REQ-5. No label SHALL carry a path, run
id, digest, expression or message.

#### Scenario: A silent evaluator is visible

- **WHEN** the evaluator stalls while rows keep being journaled
- **THEN** `harness_rule_journal_lag_seconds` rises, rather than fires quietly
  stopping

### Requirement: Error Handling Standards

All operations in this spec SHALL follow structured error handling:

- Errors SHALL be wrapped with context at each layer boundary, naming the rule,
  its source and the journal `seq` where applicable.
- Sentinel errors SHALL be defined for: unknown rule, unknown policy repo,
  rule schema violation, compile failure, cost estimate exceeded, cost limit
  exceeded, diverged policy clone, invalid proposal level, proposal backlog
  full, suppressed proposal, proposal cap reached, branch diverged, and hold not
  found.
- A rule failure SHALL be contained to that rule (REQ-6); a proposal failure to
  that proposal; `harness rules propose` SHALL continue with the next.
- Logs SHALL be structured key-value and SHALL NOT include a proposal's
  rationale or any trace text; they name the rule, hash, fire id and `seq`.

#### Scenario: One bad proposal does not stop a pass

- **WHEN** one stored proposal no longer compiles against the current binary
- **THEN** `harness rules propose` records it `invalid` with the error and
  proceeds to the next
