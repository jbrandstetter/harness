---
status: draft
date: 2026-09-29
implements: [ADR-0043]
extends: [SPEC-0005, SPEC-0006, SPEC-0007, SPEC-0013, SPEC-0014, SPEC-0017, SPEC-0022, SPEC-0026, SPEC-0031]
---

# SPEC-0032: Task Loadouts

## Overview

This spec lets a triggered one-shot start each event-carrying run with a
smaller kit than its harness's full reach. The kit is chosen per task, and it
is never wider than what the operator declared.

* A **loadout**: a global-only `[loadout.<name>]` table with a mode (`model` or
  `retrieval`), a router model, limits per axis, task families, a brief mode,
  a fallback and named prompt templates. A harness names one with `loadout`.
* The **ceiling**: per axis, what the harness can already reach. Skills are its
  SPEC-0006 merged set, MCP tools its effective `mcp_policy` set, templates the
  loadout's plus the harness's own prompt, lanes a list of harnesses. Every
  choice is a subset of it.
* **Retrieval** ranks each axis's ceiling against the event's task text and
  keeps a short candidate list. In `retrieval` mode the top candidates are the
  kit, and no chat model is called.
* A **router call**: one SPEC-0031 gate call that picks a template, a family,
  skills, tools and a short brief from the candidates. Validation drops every
  name that was not offered. Any failure falls back to the ceiling or to a
  minimal kit.
* **Delivery**: a per-run skill directory, a per-run overlay on the run's
  gateway session, the chosen operator template, and the brief as untrusted
  data in a `0600` file.
* A **`narrow` screening level** that runs a suspicious event with a read-only
  ceiling, with or without a loadout.
* **Lanes**, where a router picks which harness takes unbound work, plus
  recording, misses, eval arms, metrics, and a CLI that explains a decision
  without spawning.

See ADR-0043 for the decision and the options it rejected. This spec amends
SPEC-0005, SPEC-0006, SPEC-0007, SPEC-0013, SPEC-0014, SPEC-0017, SPEC-0022,
SPEC-0026 and SPEC-0031, and lists each amendment in REQ-21. The MCP gateway
(ADR-0035), the model API and skill index (ADR-0036) and central adapter
configuration (ADR-0039) have no spec yet, so requirements on them are stated
against the ADRs' own sections.

Requirements are numbered. Cite them as `SPEC-0032 REQ-n`.

## Requirements

### Requirement: REQ-1 — Loadout Table

The global `harness.toml` SHALL accept any number of `[loadout.<name>]` tables,
`<name>` matching `^[a-z][a-z0-9-]{0,62}$`, with these keys:

| Key | Type | Default | Rule |
|---|---|---|---|
| `mode` | string | required | `model` or `retrieval` |
| `model` | string | none | Required when `mode = "model"`. An exact model id served through `[model_api]` (ADR-0036), compared with the served model on every call (REQ-8) |
| `timeout` | duration | `8s` | `1s` to `60s`. Bounds the whole decision (REQ-3) |
| `candidates` | integer | `12` | `1` to `50`, and at least `max_skills` and at least `max_tools` |
| `max_skills` | integer | `3` | `0` to `16` |
| `max_tools` | integer | `8` | `0` to `16` |
| `families` | list of strings | none | When set, 1 to 16 unique labels, each matching `^[a-z][a-z0-9_-]{0,31}$` |
| `brief` | string | `file` | `off`, `file` or `inline` (REQ-13) |
| `brief_max_chars` | integer | `1200` | `1` to `4096` |
| `fallback` | string | `ceiling` | `ceiling` or `minimal` (REQ-10) |
| `lanes` | list of strings | none | REQ-15 |
| `template.<name>` | table | none | REQ-2 |

Any other key SHALL fail the load, naming it. So SHALL each of these, naming
the key:

* in `retrieval` mode: `model`, `families`, a `template.*` table, `lanes`, or a
  `brief` other than `off`. `brief` defaults to `off` in that mode, because
  retrieval writes no brief (REQ-7);
* on a loadout with `lanes`: a `mode` other than `model`, and any of
  `candidates`, `max_skills`, `max_tools`, `families`, `brief`,
  `brief_max_chars`, `fallback` or a `template.*` table (REQ-15).

Any declared loadout SHALL require a `[model_api]` table (ADR-0036 "The model
API"). A loadout with `max_skills` above 0 SHALL also require its
`embedding_model`, as ADR-0036 requires one for the skill index.

`[loadout.*]` SHALL be accepted only in the global `harness.toml`. It SHALL be
rejected, naming the table, in a `harness_d` drop-in, in a project
`harness.toml`, and on the project-up and scratchpad wire. It joins ADR-0009's
global-only list: a cloned repository must not choose which model reads the
operator's event text, or which prompts the operator's harnesses run.

#### Scenario: A project file cannot declare a loadout

- **WHEN** a project `harness.toml` declares `[loadout.small]` with
  `mode = "retrieval"`
- **THEN** `harness up` fails, naming `[loadout.*]` as global-only

#### Scenario: Candidates must cover the limits

- **WHEN** a loadout declares `candidates = 6` and `max_tools = 8`
- **THEN** the load fails, naming `candidates` and `max_tools`

#### Scenario: Retrieval mode takes no model

- **WHEN** a loadout declares `mode = "retrieval"` and
  `model = "qwen3-30b-a3b"`
- **THEN** the load fails, naming `model` as unused in retrieval mode

#### Scenario: Skills need embeddings

- **GIVEN** a `[model_api]` table with no `embedding_model`
- **WHEN** a loadout declares `max_skills = 3`
- **THEN** the load fails, naming the loadout and `embedding_model`

### Requirement: REQ-2 — Loadout Templates

A loadout MAY declare up to 16 named prompt templates as
`[loadout.<l>.template.<name>]` tables, `<name>` matching
`^[a-z][a-z0-9-]{0,31}$`. The name `default` SHALL be rejected, because it
names the harness's own prompt source. Each table SHALL have exactly these
keys:

| Key | Required | Value |
|---|---|---|
| `file` | yes | A path to a SPEC-0017 prompt template |
| `description` | yes | One line of operator prose, 1 to 200 characters, shown to the router (REQ-8) |

Any other key SHALL fail the load, naming it.

`file` SHALL follow SPEC-0017 REQ-5's rules for `prompt_template_file`: it is
resolved by the config path rules; config load checks that it is an existing,
readable, non-empty file and parses it, failing with a located error on a
grammar error; and every spawn that uses it reads and parses it again. A read or
parse failure at spawn SHALL fail the start as SPEC-0017 REQ-5 specifies,
naming the loadout, the template and the file. It SHALL NOT fall back to
another template.

Each template SHALL be validated against every harness that names the
loadout, by the location rules that SPEC-0017 REQ-6, REQ-7, REQ-10 and REQ-12
apply to that harness's `prompt_template_file`, with two differences:

* SPEC-0017 REQ-7's rule against a required `event.*` path on a scheduled
  harness SHALL NOT apply, because a named template renders only for a run that
  carries an event (REQ-3);
* a `loadout.*` path SHALL follow REQ-13.

A failure SHALL name the loadout, the template and the harness.

The harness's own `prompt`, `prompt_file`, `prompt_template` or
`prompt_template_file` SHALL be the **default template**, named `default`. It
renders whenever the router picks no template, in `retrieval` mode, and on
every fallback. `prompt` and `prompt_file` stay verbatim (SPEC-0017 REQ-5). A
harness that names a loadout with named templates SHALL have a prompt source,
or the load SHALL fail naming the harness, because a chosen template would
render a prompt that nothing delivers.

#### Scenario: The default name is reserved

- **WHEN** a loadout declares `[loadout.small.template.default]`
- **THEN** the load fails, naming the table and stating that `default` is the
  harness's own prompt

#### Scenario: A fenced field needs the opt-in on every harness

- **GIVEN** template `triage` of loadout `small` references
  `{{untrusted event.body}}`
- **WHEN** harness `qwen-worker` names `small` and does not set
  `untrusted_inline = true`
- **THEN** the load fails, naming `small`, `triage`, `qwen-worker` and
  `untrusted_inline`

#### Scenario: A template broken after load fails the start

- **GIVEN** the router chooses `fix`, whose file parsed at load
- **WHEN** the file holds a malformed placeholder at spawn
- **THEN** the start fails naming `small`, `fix` and the file, the record reads
  `failed`, and the `default` template is not used instead

### Requirement: REQ-3 — Where A Loadout Applies

A `[harness.*]` table MAY carry `loadout`, naming a declared loadout. It SHALL
be accepted where `triggers` is accepted (SPEC-0014 REQ "Triggers Key"): the
global file and `harness_d` drop-ins. It SHALL be rejected, naming the key, in
a project `harness.toml`, on the project-up and scratchpad wire, and in a
package manifest's `[harness]` table (amending SPEC-0026 REQ-3). The load SHALL
also fail, naming the harness and the key, when:

* the loadout is not declared;
* the harness is resident, because a loadout applies when a one-shot spawns;
* the harness has no `triggers`, because a loadout needs an event and only a
  bound source brings one: SPEC-0014 REQ "Manual Trigger With Event" refuses an
  envelope whose source the harness does not bind;
* the loadout has `lanes` (REQ-15), because a triggered harness is already
  bound to its work.

A **decision** SHALL be made for a run of a harness that names a loadout
exactly when the run carries an event: a channel or webhook firing,
`harness trigger --event`, or `harness screen release` (SPEC-0031). A run with
no event (a schedule or catch-up firing, or `harness trigger` without
`--event`), or whose event yields empty task text (REQ-5), SHALL run with the
ceiling, make no retrieval and no model request, and record
`fallback = "no_event"`.

Timing:

* A decision SHALL start after the run is admitted and its `opened` line is
  written (SPEC-0021 REQ-4), and SHALL finish before the per-run kit is built
  and the process spawns. It SHALL NOT run under the admission lock.
* Screening (SPEC-0031 REQ-11) comes first. A run that screening holds or
  blocks is skipped before admission and makes no decision.
* `timeout` SHALL bound the whole decision: task text, retrieval, the router
  call and validation. Crossing it SHALL fall back with reason `timeout`
  (REQ-10).
* While it waits, the run holds the concurrency slot admission gave it
  (SPEC-0021 REQ-6). Supervision, attach, recording, a resident harness's
  restart, and the admission of any other run SHALL NOT wait on a decision.
* A stop or a daemon shutdown during a decision SHALL cancel it, spawn nothing,
  and close the record as SPEC-0022 REQ-5 specifies for that stop.

A decision SHALL select subsets on the four axes of REQ-4 and nothing else. It
SHALL NOT change the harness's model, workdir, environment, budgets,
`mcp_policy`, `mcp_allow` or any other key.

#### Scenario: A resident harness cannot name a loadout

- **WHEN** a resident `crush` harness declares `loadout = "small"`
- **THEN** the load fails, naming the harness and `loadout`

#### Scenario: A package cannot ship a loadout

- **WHEN** a package manifest's `[harness]` table declares `loadout = "small"`
- **THEN** the package fails to load, naming `loadout`

#### Scenario: A scheduled firing makes no call

- **GIVEN** `qwen-worker` has `schedule`, `triggers` and `loadout = "small"`
- **WHEN** its schedule fires
- **THEN** the run spawns with its ceiling, its record carries
  `fallback = "no_event"`, and a fake model server receives no request

#### Scenario: A slow router delays nobody else

- **GIVEN** a router that answers only after `timeout`, and an unrouted harness
  bound to the same source as `qwen-worker`
- **WHEN** one delivery fires both
- **THEN** the unrouted harness is admitted and spawns without waiting, and
  `qwen-worker` spawns once `timeout` passes, recording `fallback = "timeout"`

### Requirement: REQ-4 — The Ceiling

Each axis has a ceiling, and a decision SHALL choose within it:

| Axis | Ceiling | The loadout chooses | Delivered by |
|---|---|---|---|
| Skills | The harness's merged skill set: one winning copy per name, per SPEC-0006 REQ "Ordered Merge and Shadowing" as amended by SPEC-0026 REQ-10, resolved at decision time from the roots projection would read | At most `max_skills` | REQ-11 |
| MCP tools | The upstream tools the harness's effective `mcp_policy` allows (ADR-0035 "Exposure policy": `servers`, `tools`, `deny`), less every tool its `mcp_allow` would refuse (ADR-0035 "Tool tiers") | At most `max_tools` | REQ-12 |
| Template | The loadout's named templates plus `default` | One | REQ-2, REQ-13 |
| Lane | The loadout's `lanes` | One harness | REQ-15 |

* **Narrow only.** Every item a run receives on an axis SHALL be a member of
  that axis's ceiling. The daemon SHALL check the per-run kit against the
  ceiling before spawn. An item outside it SHALL fail the start, naming the
  item, because it can only mean a defect.
* **Skills are relevance; tools are privilege.** A ceiling skill the loadout
  leaves out stays reachable through search for that run (REQ-11). A ceiling
  tool the loadout leaves out SHALL be unreachable for that run (REQ-12).
* Skills from SPEC-0007 skill repos are not on the skills axis. SPEC-0006
  excludes them from projection, and `search_skills` keeps serving them as
  SPEC-0007 specifies.
* Reserved-namespace tools (the facade tools, `search_skills`, `get_skill`,
  `search_tools` and `call_tool`) are not on the tools axis. `mcp_allow` scopes
  them as before, except under `minimal` (REQ-10) and `narrow` (REQ-14).
* The agent CLI's built-in tools are on no axis. Only REQ-14 touches them.
* Under the `narrow` level, the MCP tools ceiling is its read-tier subset,
  taken before retrieval (REQ-14).

#### Scenario: A tool the harness cannot call is never offered

- **GIVEN** `qwen-worker`'s policy exposes the write-tier tool
  `gitea__merge_pull_request`, and its `mcp_allow` is `["read"]`
- **WHEN** a decision runs
- **THEN** `gitea__merge_pull_request` is neither a candidate nor in the
  router's catalog

#### Scenario: A skill repo's skill is not on the axis

- **GIVEN** a skill repo serves `go-race-flakes` to `qwen-worker`, and no
  skill root supplies it
- **WHEN** a decision runs
- **THEN** `go-race-flakes` is not a candidate, and `search_skills` still
  returns it to the run as SPEC-0007 specifies

### Requirement: REQ-5 — Task Text

The **task text** of a run's event SHALL be drawn only from the text SPEC-0031
REQ-11 derives to screen that event, by the same code whether or not screening
is on for the source or the harness. It is a list of named fields:

* for a `github` or `gitea` webhook with a JSON body, each of SPEC-0017
  REQ-10's untrusted fields that is present (`title`, `body`, `comment`,
  `ref`), named by that field; when none is present, the whole screened text as
  one field named `event`;
* for any other webhook, the whole screened text as one field named `event`;
* for a channel notification, `content` and every `meta` value, named by key.

The task text is a subset of what screening sees, never more. Screening covers
every string the sender wrote. The router needs only what the task is about,
and never reads text screening did not see. Each field SHALL be:

* redacted, as SPEC-0031 REQ-8 redacts gate call input;
* stripped of NUL and every C0 and C1 control character except newline and
  tab, and capped at 4096 bytes on a UTF-8 boundary with a trailing
  `[truncated N bytes]` marker, as SPEC-0017 REQ-10 treats a fenced value.

The same fields SHALL feed retrieval (REQ-6), the router call (REQ-8) and the
lane call (REQ-15). A task text whose fields are all empty after these steps
SHALL count as no event (REQ-3).

The task text SHALL NOT be written to the ledger, a log line, a metric label, a
notification, or any file other than the event file SPEC-0014 already writes.
For work a dispatcher claims, the task text of a todo's coalesced events is
defined by the ADR-0021 amendment for #541, not here.

#### Scenario: The same text with screening off

- **GIVEN** `[webhook.gitea]` sets `screen = { mode = "off" }`
- **WHEN** a Gitea issue delivery fires `qwen-worker`
- **THEN** the fields fenced in the router request are the issue's `title` and
  `body`, byte-identical to the same values in what SPEC-0031 REQ-11 derives
  from that delivery, and identical to the fields a screened source would give

#### Scenario: Empty text is no event

- **WHEN** a channel notification with empty `content` and no `meta` fires
  `qwen-worker`
- **THEN** no model request is made, and the record carries
  `fallback = "no_event"`

### Requirement: REQ-6 — Candidate Retrieval

For each axis the run can receive narrowed (REQ-11 and REQ-12 say when it
cannot), retrieval SHALL rank that axis's ceiling against the task text, its
fields joined with newlines, and SHALL keep the top `candidates` items as that
axis's **candidates**:

* **Skills.** ADR-0036's hybrid ranking ("The skill index"): BM25 over the
  FTS5 fields, cosine similarity over `embedding_model` vectors, fused by
  reciprocal rank fusion. The index SHALL hold an entry for every ceiling skill
  of every harness that names a loadout with `max_skills` above 0. The entry is
  built from the winning copy's `SKILL.md` over the fields ADR-0036 indexes,
  keyed by content hash, with its vector cached by content hash and model id.
  An entry missing at decision time SHALL be built then, inside `timeout`.
* **Tools.** ADR-0035's FTS5 tool index over each tool's name, description and
  parameter names (ADR-0035 "Context: lazy discovery and trimming").
* **Templates, families and lanes** are not retrieved. Every declared one is
  offered.

Rules:

* Ranking SHALL be restricted to the ceiling before the top `candidates` are
  taken. An index entry outside the ceiling SHALL never be a candidate,
  whatever its rank. Retrieval never widens.
* An item the ranking does not match SHALL NOT be a candidate, so an axis may
  have fewer than `candidates`, or none. Equal ranks SHALL be ordered by name.
* When the query or a skill cannot be embedded (the endpoint fails, is slow, or
  lacks the model), the skills axis SHALL rank by FTS5 alone for that decision,
  as ADR-0036 degrades `search_skills`. The record SHALL carry
  `degraded = true`, and `harness_skill_search_degraded_total` SHALL increment
  with reason `embeddings_unavailable`. Degraded retrieval is not a failure.
* A store error that prevents ranking an axis SHALL fall back with reason
  `index` (REQ-10).

#### Scenario: Another harness's skill never leaks

- **GIVEN** skill `release-notes` is in another harness's ceiling only, and
  ranks first for the task text
- **WHEN** `qwen-worker` decides
- **THEN** `release-notes` is not a candidate

#### Scenario: Degraded, not failed

- **GIVEN** the embeddings endpoint refuses connections
- **WHEN** `qwen-worker` decides
- **THEN** its skills are ranked by FTS5 alone, its record carries
  `degraded = true`, and the run spawns with the decision applied

#### Scenario: A small ceiling

- **GIVEN** a tool ceiling of five tools and `candidates = 12`
- **WHEN** retrieval runs
- **THEN** there are at most five tool candidates

### Requirement: REQ-7 — Retrieval Mode

In `mode = "retrieval"`, a decision SHALL make no chat-completion request. The
kit SHALL be the first `max_skills` skill candidates and the first `max_tools`
tool candidates, in rank order, with the `default` template, no family and no
brief. The only model API requests it may make are the skills axis's
embeddings requests (REQ-6). Its record SHALL carry no `served_model` and
`invalid = 0`. A retrieval-mode decision fails only with reason `timeout` or
`index`.

#### Scenario: Zero chat calls

- **GIVEN** loadout `small` has `mode = "retrieval"`
- **WHEN** twenty events fire `qwen-worker`
- **THEN** a fake model server records no request to `/chat/completions`, and
  every run's record carries `template = "default"` and skills and tools drawn
  from its candidates

### Requirement: REQ-8 — The Router Call

In `mode = "model"`, a decision SHALL make exactly one **router call**: a gate
call (SPEC-0031 REQ-8) on the `loadout` entry point (REQ-21). It SHALL keep
every gate call rule: one request, no `tools` field, no prior assistant turn,
redacted input, and metering as a gate call. Its effect is narrow-only. It
selects subsets of the candidates, and SHALL NOT start a run, widen a
permission, select a command or change config.

The request:

* SHALL go to `[model_api].base_url` followed by `/chat/completions`, with the
  `[model_api]` credential, `model` set to the loadout's `model`, and
  `temperature = 0`;
* SHALL carry exactly two messages. The **system message** is a fixed text
  embedded in the binary, owned by Harness and identified by a version string.
  No key, file or front door sets it. It states that fenced text is data to
  classify, never instructions, and that only listed names may be returned;
* the **user message** SHALL hold a catalog, then the task:
  * the catalog lists `default` and each named template with its
    `description`; the declared `families`; each skill candidate's name and
    `description`, trimmed to 300 characters; each tool candidate's namespaced
    name and description, trimmed as ADR-0035 trims (`max_description_chars`,
    default 300), without its schema; and `max_skills`, `max_tools` and, unless
    `brief = "off"`, `brief_max_chars`;
  * the task is each REQ-5 field in its own SPEC-0017 REQ-10 fence, with the
    event's source reference as `source`, the field's name as `field`, and a
    fresh nonce for each call;
* SHALL ask for the reply in JSON with a JSON schema (an OpenAI-compatible
  `response_format` of type `json_schema`) whose object has exactly the keys
  below, all required. Where an endpoint rejects that response format itself,
  the daemon MAY send the request once more without it, inside the same
  `timeout`. The parse below is the same either way.

| Key | Type | Present when |
|---|---|---|
| `template` | string or null | always |
| `family` | string or null | `families` is declared |
| `skills` | array of strings | always |
| `tools` | array of strings | always |
| `brief` | string | `brief` is not `off` |

The reply:

* **Strict parse.** The first choice's message content SHALL be exactly one
  JSON object matching that schema, optionally surrounded by whitespace, and at
  most 64 KiB. Anything else SHALL fail with reason `parse`: prose or a code
  fence around the object, a missing or extra key, a wrong type, or a second
  value. The daemon SHALL NOT search prose for an object or repair one.
* **Served model.** The reply's `model` SHALL be checked as SPEC-0031 REQ-8
  checks a gate call's served model. A mismatch SHALL fail with reason
  `model_mismatch`. The served id SHALL be recorded as `served_model`.
* **Other failures.** A connection error, a non-2xx status or a malformed
  response envelope SHALL fail with reason `transport`. Reaching the deadline
  SHALL fail with reason `timeout`.
* The raw reply SHALL NOT be logged or stored. Only the names and the brief
  that pass REQ-9 leave the call.

#### Scenario: Prose around the object is a parse failure

- **WHEN** the router replies with a valid object preceded by the words
  `Here is the kit:`
- **THEN** the loadout's fallback applies with reason `parse`, and none of the
  object's names are used

#### Scenario: A substituted model

- **GIVEN** loadout `small` names `model = "qwen3-30b-a3b"`
- **WHEN** the reply reports `model` `qwen3-8b`
- **THEN** the fallback applies with reason `model_mismatch`, and the record
  carries `served_model = "qwen3-8b"`

#### Scenario: The task stays fenced

- **GIVEN** an event body containing the canary `CANARY-7f3a`
- **WHEN** the router request reaches a fake model server
- **THEN** the canary appears only inside an `untrusted-data` fence in the user
  message, never in the system message, and the request has no `tools` field

### Requirement: REQ-9 — Validation

Validation SHALL turn a parsed reply into the kit, keeping only what was
offered:

* `template`: a declared template name or `default` selects that template;
  null or an empty string selects `default`; any other value selects `default`
  and counts one `invalid`.
* `family`: a declared family is kept; null or an empty string is omitted; any
  other value is omitted and counts one `invalid`.
* `skills` and `tools`: an entry SHALL be kept only if it equals a candidate of
  its axis, exactly and case-sensitively. Every other entry SHALL be dropped and
  counted `invalid`, including a ceiling item that was not a candidate. A
  repeated entry SHALL be kept once and not counted. The kept entries, in reply
  order, SHALL then be cut to `max_skills` or `max_tools`.
* `brief`: stripped of NUL and every C0 and C1 control character except
  newline and tab, and capped at `brief_max_chars` characters or 4096 bytes,
  whichever is shorter, on a UTF-8 boundary, with a trailing
  `[truncated N bytes]` marker, exactly as SPEC-0017 REQ-10 treats a fenced
  value. An empty result means no brief.

Validation SHALL NOT add a name to any axis, whatever the reply. `invalid` is
the total across axes.

#### Scenario: A tool outside the ceiling is dropped

- **GIVEN** `gitea__delete_repo` is outside `qwen-worker`'s tool ceiling
- **WHEN** the reply's `tools` is `["gitea__list_issues", "gitea__delete_repo"]`
- **THEN** the run's `tools/list` carries `gitea__list_issues` and not
  `gitea__delete_repo`, and the record carries `invalid = 1`

#### Scenario: Naming the whole ceiling yields at most the cap

- **GIVEN** a 30-tool ceiling, 12 tool candidates and `max_tools = 8`
- **WHEN** the reply's `tools` lists all 30 ceiling tools
- **THEN** the run receives 8 tools, each a candidate, and the record carries
  `invalid = 18`

#### Scenario: An undeclared template means the default

- **WHEN** the reply's `template` is `"release"`, which `small` does not declare
- **THEN** the run renders the `default` template, and `invalid` counts one

### Requirement: REQ-10 — Fallback

A decision that does not yield the chosen kit SHALL record one reason from this
closed set:

| Reason | Means | The run gets |
|---|---|---|
| `timeout` | `timeout` passed before the decision finished | the loadout's `fallback` |
| `transport` | the router call failed below the reply content (REQ-8) | the loadout's `fallback` |
| `parse` | the reply content failed the strict parse (REQ-8) | the loadout's `fallback` |
| `model_mismatch` | the served model is not `model` (REQ-8) | the loadout's `fallback` |
| `index` | a store error prevented retrieval (REQ-6) | the loadout's `fallback` |
| `no_event` | no event, or empty task text (REQ-3, REQ-5) | the ceiling, whatever `fallback` says |
| `unsupported` | the decision applied, but an axis could not be delivered narrowed (REQ-11, REQ-12) | the decision, with that axis at its ceiling |

* `fallback = "ceiling"` SHALL spawn the run exactly as it would spawn with no
  `loadout`: SPEC-0006 projection as configured, the gateway session under its
  full policy with no overlay, the `default` template, no brief and no
  `loadout.*` context value. This is the status quo.
* `fallback = "minimal"` SHALL spawn the run with the `default` template, no
  brief, no `loadout.*` context value, an empty per-run skill directory
  (REQ-11), and an overlay with no upstream tool (REQ-12) whose session also
  refuses reserved-namespace write tools, so that the run reaches only
  reserved-namespace read tools. An axis the run cannot receive narrowed stays
  at its ceiling, and its record field is absent (REQ-16).
* The `narrow` level (REQ-14) applies on top of either, because it comes from
  the event's verdict, not from the decision.
* A fallback SHALL NOT retry the decision, hold the run or skip it. When
  `unsupported` and another reason both apply, the other reason SHALL be
  recorded. Every reason is recorded (REQ-16) and counted (REQ-19).

#### Scenario: A timeout under the ceiling fallback

- **GIVEN** `fallback = "ceiling"` and a router that answers after `timeout`
- **WHEN** an event fires `qwen-worker`
- **THEN** the run spawns with SPEC-0006's full projection and no overlay, and
  its record carries `fallback = "timeout"` and neither `skills` nor `tools`

#### Scenario: A minimal fallback

- **GIVEN** `fallback = "minimal"` and a router that answers with status 500
- **WHEN** an event fires `qwen-worker`
- **THEN** the run's skill directory is empty, its `tools/list` holds only
  reserved-namespace read tools, and its record carries
  `fallback = "transport"`, `skills = []` and `tools = []`

#### Scenario: One axis unsupported

- **GIVEN** `qwen-worker`'s adapter declares no `skills.per_run`
- **WHEN** a decision chooses three tools
- **THEN** the run's overlay holds those three tools, its skills are projected
  as SPEC-0006 specifies, and its record carries `tools`, no `skills`, and
  `fallback = "unsupported"`

### Requirement: REQ-11 — Per-Run Skill Projection

Each effective adapter (ADR-0039) SHALL declare `skills.per_run`: either none,
or how one process of its family is made to read skills only from a directory
the daemon names, in place of the adapter's `skills.target`.
`harness adapters show` SHALL print it. The declarations for the built-in
`claude-code`, `crush` and `codex` adapters are open (see design.md). Until a
family's declaration is specified and covered by a test that drives the real
client and observes which skills it loads, it SHALL be none.

When a run's skills axis is narrowed (a decision applied, or `minimal`) and its
adapter declares `skills.per_run`, the daemon SHALL, before exec:

* create `<jobs dir>/<harness>/<run_id>.skills/` with mode `0700`;
* copy into it the winning copy of each chosen skill and nothing else, by copy
  and never by link (SPEC-0006 REQ "Spawn-Time Projection");
* apply `skills.per_run` so that the process reads skills from that directory,
  and not project into the adapter's `target` for that run.

Each run of a harness SHALL get its own directory and SHALL NOT see another
run's skills. This holds for runs that overlap in time too, where a later spec
allows them: ADR-0021 defers concurrent runs of one harness, and #541's
dispatcher may lift that. The directory SHALL be pruned with the
run's record, log and event file (SPEC-0014 REQ "Event Delivery To The Run").
A failure to create or fill it SHALL fail the start with an error naming the
adapter, the path and the cause, and the record SHALL read `failed` with reason
`spawn`.

When the adapter declares none, the skills axis SHALL be neither retrieved nor
offered to the router, the run SHALL be projected as SPEC-0006 specifies, and
its record SHALL carry no `skills` and, unless another reason applies,
`fallback = "unsupported"`.

**Left-out skills stay searchable.** For a run whose skills axis is narrowed,
`search_skills` and `get_skill` (SPEC-0007 REQ "Search And Retrieval Tools")
SHALL also serve that run's skill ceiling, each skill identified as
`projected/<name>` and ranked through REQ-6's index entries. They SHALL be
registered for that run even when no skill repo is declared. `projected` SHALL
be a reserved skill repo name. No caller SHALL see another run's ceiling
through them.

#### Scenario: Each run sees only its own skills

- **GIVEN** run 7 of `qwen-worker` chose `go-testing`, and its
  `7.skills/` directory is still on disk
- **WHEN** run 8 chooses `forge-triage` and lists the skills its client loaded
- **THEN** it sees only `forge-triage`: not `go-testing`, and not the rest of
  the ceiling in the adapter's `target`

#### Scenario: A run cannot change a source

- **WHEN** a run edits a file inside its `<run_id>.skills/` directory
- **THEN** the same file under the contributing skill root is unchanged

#### Scenario: A left-out skill is recovered by search

- **GIVEN** `forge-triage` is in the ceiling but not in the run's `skills`
- **WHEN** the run calls `search_skills` with a matching query, then
  `get_skill` on `projected/forge-triage`
- **THEN** the search returns it, `get_skill` serves it, and a skill miss is
  recorded (REQ-17)

### Requirement: REQ-12 — Per-Run Gateway Overlay

This requirement is stated against ADR-0035, which has no spec yet.

* **Session by run.** When the daemon mints a spawn's `HARNESS_MCP_TOKEN`
  (SPEC-0005 REQ "Caller Identity"), it SHALL bind the token to the run id that
  spawn serves. The gateway SHALL resolve a session's run from its token alone.
  A run id an agent claims, `HARNESS_RUN_ID` included, SHALL NOT be trusted.
* **The overlay.** When a run's tools axis is narrowed, the daemon SHALL
  register an **overlay**, a set of namespaced upstream tool names, under the
  run id before the process spawns, and SHALL release it when the process
  exits. It SHALL NOT change during the run.
* **Enforcement.** For a session whose run has an overlay:
  * `tools/list` SHALL return the overlay's tools that the policy still allows,
    plus the reserved-namespace tools the run may call;
  * `search_tools` SHALL return only overlay tools;
  * `call_tool`, or a direct call, naming an upstream tool outside the overlay
    SHALL be refused with `not_permitted` before it reaches an upstream;
  * policy, tier and timeout checks SHALL still apply to overlay tools. The
    effective set is the overlay intersected with the policy at call time, so a
    reload can remove a tool from a run and never add one.
* **Exposure.** The overlay SHALL be exposed `full`, whatever the policy's
  `exposure`: each tool is listed with its schema and a description trimmed as
  ADR-0035 trims it. `search_tools` and `call_tool` SHALL NOT be listed. A call
  to either by name SHALL still see only the overlay.
* **No churn.** The gateway SHALL send no `notifications/tools/list_changed`
  because of an overlay. An upstream's own list change may withdraw an overlay
  tool (ADR-0035 "Session semantics"), and never adds one.
* **Call log.** A refused call SHALL be logged with outcome `denied`. When the
  refused tool is in the run's tool ceiling, the row SHALL also carry
  `loadout_miss = "tool"` (REQ-17).

A run with no gateway session (its adapter cannot wire the bridge, or it sets
`mcp_bridge = false`, per SPEC-0005 REQ "Endpoint Wiring") SHALL have its tools
axis neither retrieved nor offered, SHALL carry no `tools` in its record, and,
unless another reason applies, SHALL record `fallback = "unsupported"`. The
overlay narrows only gateway tools. MCP servers an agent loads from its own
configuration, which additive wiring leaves in place, are outside it
(REQ-20 lists the doctor warning).

#### Scenario: The run lists only its overlay

- **GIVEN** `qwen-worker`'s policy is `lazy` and allows 40 tools, and a
  decision chooses `gitea__list_issues` and `gitea__add_issue_labels`
- **WHEN** the run calls `tools/list`
- **THEN** it receives those two tools with their schemas plus the
  reserved-namespace tools it may call, and neither `search_tools` nor
  `call_tool`

#### Scenario: A ceiling tool outside the overlay is refused

- **WHEN** that run calls `gitea__create_pull_request`, which its policy allows
- **THEN** the call fails with `not_permitted` without reaching `gitea-mcp`,
  and the call-log row carries outcome `denied` and `loadout_miss = "tool"`

#### Scenario: The overlay belongs to the run, not the policy

- **GIVEN** `claude-worker` has the same `mcp_policy` as `qwen-worker` and no
  loadout, and both have a run in flight
- **WHEN** both call `gitea__create_pull_request`
- **THEN** `claude-worker`'s call proceeds, and `qwen-worker`'s is refused

#### Scenario: A reload cannot add a tool

- **WHEN** a reload adds `gitea__merge_pull_request` to `qwen-worker`'s policy
  while a run is in flight
- **THEN** that run's `tools/list` does not change, and its call of the tool is
  refused

### Requirement: REQ-13 — Template Context And The Brief

The SPEC-0017 REQ-7 template context SHALL gain these paths:

| Path | Tier | Present when |
|---|---|---|
| `loadout.template` | daemon | A decision applied: the chosen template's name, or `default` |
| `loadout.family` | daemon | The decision kept a declared family |
| `loadout.skills` | daemon | A decision applied and narrowed the skills axis: the chosen names joined by `, `, empty when none |
| `loadout.tools` | daemon | A decision applied and narrowed the tools axis: the chosen names joined by `, `, empty when none |
| `loadout.lane` | daemon | A lane call routed the run: the lanes loadout's name (REQ-15) |
| `loadout.brief_file` | daemon | A brief file was written: its absolute path |
| `loadout.brief` | untrusted | Only as `{{untrusted loadout.brief}}`, with `brief = "inline"` |

Every value except `loadout.brief` is an operator-declared identifier or a
daemon path, never free text, and SHALL be accepted wherever SPEC-0017 accepts
a daemon-tier path. `loadout.template` MAY be referenced in the required form
in a named template, which renders only when a decision applied. Every other
reference to a `loadout.*` path, and every reference in a harness's default
template, SHALL use the optional form, because the default template renders on
every fallback. A required form elsewhere SHALL fail the load, naming the file
and the path.

The brief, per the loadout's `brief`:

* **`file`.** When validation yields a brief, the daemon SHALL write it before
  exec to `<jobs dir>/<harness>/<run_id>.brief.md`, mode `0600`, and export its
  absolute path as `HARNESS_BRIEF_FILE`. The variable SHALL override a
  same-named variable from the daemon's environment and from `env_file`, as
  `HARNESS_PROMPT_FILE` does (SPEC-0017 REQ-12). The file SHALL be pruned with
  the run's record, log and event file. A write failure SHALL fail the start.
  With no brief, no file is written and the variable is unset.
* **`inline`.** No file and no variable. The brief SHALL be available only as
  `{{untrusted loadout.brief}}` in a prompt template, rendered in the SPEC-0017
  REQ-10 fence with the event's source reference as `source` and
  `loadout.brief` as `field`, and logged and counted as REQ-10 requires for
  every fenced rendering. A harness that names an `inline` loadout without
  `untrusted_inline = true` SHALL fail the load, naming the harness, the
  loadout and the key. No new key opens the fence.
* **`off`.** The router is asked for no brief.

The brief is written from untrusted text, and SHALL travel as that text does:

* it SHALL NOT appear in an argv element, except inside the rendered prompt of
  an opted-in harness under `brief = "inline"` whose adapter delivers the prompt
  by argv, where SPEC-0017 REQ-10 already places fenced event text;
* it SHALL NOT appear in an environment variable's value, the ledger, a log
  line, a notification, a metric label, `state.json`, or a protocol frame other
  than the reply to `harness loadout explain`. Only its hash is recorded
  (REQ-16);
* it carries the event's SPEC-0031 verdict, and SHALL NOT be screened again.
  No gate call SHALL be made on it.

#### Scenario: A canary stays in the brief file

- **GIVEN** a canary string planted in an event body, and a fake router that
  copies it into its brief
- **WHEN** `qwen-worker` (`brief = "file"`, no `untrusted_inline`) runs
- **THEN** the canary is in the file `HARNESS_BRIEF_FILE` names, and in no argv
  element, no rendered prompt and no ledger line, each checked by reading the
  process and the files

#### Scenario: The default template cannot require a loadout value

- **WHEN** `qwen-worker`'s `prompt_template_file` references
  `{{loadout.family}}`
- **THEN** the load fails, naming the file and `loadout.family`, and suggesting
  `{{loadout.family?}}`

#### Scenario: Inline needs the opt-in

- **WHEN** loadout `small` sets `brief = "inline"` and `qwen-worker` names it
  without `untrusted_inline = true`
- **THEN** the load fails, naming `qwen-worker`, `small` and
  `untrusted_inline`

#### Scenario: The brief is pruned with the run

- **WHEN** `keep_runs` prunes run 7 of `qwen-worker`
- **THEN** `7.brief.md` and `7.skills/` are removed with its record, log and
  event file

### Requirement: REQ-14 — The Narrow Level

This requirement amends SPEC-0031 REQ-12. The levels SHALL be, in order and
cumulative: `annotate`, `notify`, `narrow`, `hold`, `block`. `narrow` SHALL be
accepted wherever SPEC-0031 accepts a level (`on_flag`, `on_warn` and
`on_error`, in `[screen]`, on a source and on a harness), and SHALL act as
`notify` plus a **read-only ceiling** for that run:

* **MCP tools.** The tools ceiling SHALL be reduced to its read-tier tools
  before retrieval (ADR-0035 "Tool tiers": the upstream's `readOnlyHint`,
  overridden by `read_tools` and `write_tools` globs, where a tool with neither
  is write), and reserved-namespace write tools SHALL be refused. With a
  loadout, the overlay is chosen from the read-only ceiling. With no loadout, or
  on `fallback = "ceiling"`, the run SHALL get an overlay of the whole read-only
  ceiling, under REQ-12's identification, enforcement and no-churn rules and
  with the policy's own `exposure`. A run with a gateway session records
  `mcp_tools = "read_only"`. A run without one records
  `mcp_tools = "unnarrowed"`.
* **Built-in tools.** Where the effective adapter declares a read-only
  built-in set, `tools.read_only` (ADR-0039), the run SHALL be spawned so that
  the agent cannot use a built-in tool outside that set, intersected with the
  harness's own `allowed_tools` where it sets them, and records
  `builtin_tools = "read_only"`. A rendering the client treats as a permission
  hint, which a bypass such as `auto_accept` overrides, does not meet this.
  Where the adapter declares no set, the run records
  `builtin_tools = "unnarrowed"`.
* Skills, the template and the brief are unchanged.

`narrow` needs no router model. It applies to any event-carrying run, with or
without a loadout. In shadow mode (SPEC-0031) it acts on nothing, and the
record shows no narrowing. A refusal that `narrow` causes is not a miss
(REQ-17). SPEC-0031's defaults are unchanged. The documentation SHALL
recommend `on_warn = "narrow"` and `on_flag = "hold"` for a public source.

#### Scenario: A warned event runs read-only

- **GIVEN** `pr-review` sets `screen = { on_warn = "narrow" }`
- **WHEN** a delivery scores `warn` and fires it
- **THEN** its gateway session lists no write-tier tool, a call to one fails
  with `not_permitted`, one `screen_flagged` notification is sent, and the
  record carries `narrowed = true` and `mcp_tools = "read_only"`

#### Scenario: A declared built-in set is enforced

- **GIVEN** an adapter that declares `tools.read_only`, and a `warn` verdict
  with `on_warn = "narrow"`
- **WHEN** the agent tries a built-in tool outside the set, `auto_accept` on
- **THEN** the client does not run it, checked by driving the real client
  rather than reading its argv, and the record carries
  `builtin_tools = "read_only"`

#### Scenario: A family with no read-only set says so

- **GIVEN** a harness whose adapter declares no `tools.read_only`
- **WHEN** a `warn` verdict narrows its run
- **THEN** its gateway session is read-only, and its record carries
  `builtin_tools = "unnarrowed"`

#### Scenario: Shadow does not narrow

- **GIVEN** `mode = "shadow"` and `on_warn = "narrow"`
- **WHEN** a delivery scores `warn`
- **THEN** the run's gateway session is its full policy, no notification is
  sent, and the record carries the would-have level `narrow` and no `narrowed`

### Requirement: REQ-15 — Lanes

A **lanes loadout** declares `lanes`: 2 to 16 unique names, each naming a
harness declared in the global file or a `harness_d` drop-in that is a
one-shot and has a non-empty `description`. An unknown or duplicate name, a
resident harness, or a missing description SHALL fail the load, naming the
loadout and the lane. REQ-1 limits a lanes loadout's keys, and REQ-3 forbids a
harness to name one.

A **lane call** SHALL be one gate call on the `loadout` entry point (REQ-21):

* a fixed, Harness-owned system message, distinct from the router's;
* a user message whose catalog lists each lane's name and `description` in
  declared order, followed by the task text fenced as REQ-8 fences it;
* a reply that is one JSON object with the single key `lane`, a string, parsed
  strictly and model-checked as REQ-8 parses and checks the router's reply,
  inside the lanes loadout's `timeout`.

Then:

* A reply naming a lane SHALL choose it.
* Any other outcome SHALL choose the first lane: a null lane, a name outside
  `lanes` (counted `invalid`), or a failure (`timeout`, `transport`, `parse`,
  `model_mismatch`). No outcome SHALL start a harness outside `lanes`.
  Operators put the cheapest safe default first.
* The chosen harness's own screening level, admission, ceiling and loadout
  SHALL then apply, as if its own source had fired it. The lane call and that
  harness's kit decision are separate calls, and neither widens the other. The
  lane call SHALL NOT change the chosen harness's configuration.
* The chosen run's record SHALL carry `lane`, the lanes loadout's name
  (REQ-16).

**Deferred.** No dispatcher exists yet (#541). How a dispatcher queue names a
lanes loadout, which work counts as unbound, and how a claimed todo's
coalesced events become task text belong to the ADR-0021 amendment for #541.
Until it lands, no runtime path makes a lane call, and
`harness loadout explain --lanes` (REQ-20) exercises lanes end to end without
spawning.

#### Scenario: A lane outside the list is never started

- **GIVEN** loadout `queue` with `lanes = ["qwen-worker", "claude-worker"]`
- **WHEN** `harness loadout explain --lanes queue --event FILE` gets a reply
  naming `pr-review`
- **THEN** the explanation chooses `qwen-worker`, reports the reply as
  `invalid`, and never names `pr-review` as the lane

#### Scenario: A harness cannot name a lanes loadout

- **WHEN** a triggered harness declares `loadout = "queue"`, and `queue` has
  `lanes`
- **THEN** the load fails, naming the harness, `queue` and `lanes`

#### Scenario: A resident lane

- **WHEN** `lanes` names a resident harness
- **THEN** the load fails, naming the loadout and the lane

### Requirement: REQ-16 — Recording

This requirement amends SPEC-0022 REQ-4. A run record SHALL gain `loadout`, an
object with these fields, each omitted when it does not apply:

| Field | Type | Meaning |
|---|---|---|
| `name` | string | The harness's loadout |
| `mode` | `model` or `retrieval` | |
| `template` | string | The chosen template, or `default` |
| `family` | string | The kept family |
| `lane` | string | The lanes loadout that routed the run (REQ-15) |
| `skills` | list of strings | Present exactly when a decision or `minimal` narrowed the skills axis: the skills the run received |
| `tools` | list of strings | Present exactly when a decision or `minimal` narrowed the tools axis: the overlay. A `narrow`-only overlay is not listed |
| `narrowed` | boolean | `true` when the `narrow` level applied (REQ-14) |
| `builtin_tools` | `read_only` or `unnarrowed` | Present when narrowed |
| `mcp_tools` | `read_only` or `unnarrowed` | Present when narrowed |
| `fallback` | string | A REQ-10 reason |
| `invalid` | integer | REQ-9 |
| `served_model` | string | The router's served model (REQ-8) |
| `degraded` | boolean | Skills were ranked by FTS5 alone (REQ-6) |
| `duration_ms` | integer | The whole decision |
| `brief_hash` | string | SHA-256, in hex, of the validated brief |

* A run narrowed with no loadout carries only `narrowed`, `builtin_tools` and
  `mcp_tools`. A `no_event` run carries `name`, `mode` and `fallback`.
* Lists SHALL be capped at 16 entries and strings at 256 bytes, as SPEC-0022
  REQ-4 caps its other fields.
* The object SHALL be written in an `updated` line after the decision and
  before the process spawns, synced to stable storage as SPEC-0022 REQ-6 syncs
  an `opened` line. A run that fails or is skipped after its decision keeps it.
* Privacy (SPEC-0022 REQ-17): no ledger line SHALL carry task text, the brief,
  the router's prompt or reply, a description, or a tool argument. The ledger
  holds names, counts, reason codes, the served model and one hash.

#### Scenario: A routed run's record

- **WHEN** a model-mode decision for `qwen-worker` chooses template `triage`,
  family `bug`, two skills and five tools, with no invalid name and a brief
- **THEN** the record's `loadout` carries `name = "small"`, `mode = "model"`,
  `template = "triage"`, `family = "bug"`, both lists, `invalid = 0`,
  `served_model`, `duration_ms` and `brief_hash`, and the `updated` line was
  on disk before the process spawned

#### Scenario: No text reaches the ledger

- **GIVEN** a canary string in an event's title and body
- **WHEN** a decision runs and the run completes
- **THEN** no ledger line contains any byte of the canary

### Requirement: REQ-17 — Misses

* A **skill miss** is a `get_skill` call, or a read of the skill's MCP
  resource, by a run whose skills axis was narrowed, for a ceiling skill
  outside the run's `skills`. The skill SHALL be served, and the call-log row
  SHALL carry `loadout_miss = "skill"`. A search result alone is not a miss.
* A **tool miss** is REQ-12's refusal of a ceiling tool outside the overlay,
  logged with `loadout_miss = "tool"`. A refused tool outside the ceiling, or a
  write tool refused under `narrow`, is not a miss.
* Misses SHALL be read from the gateway call log (ADR-0035 "The call log"),
  joined to run records by harness and run id, and kept for the call log's
  `call_log_days`. A miss SHALL change nothing else for the run.
* `harness loadout misses` lists them (REQ-20), and
  `harness_loadout_misses_total` counts them (REQ-19).

#### Scenario: A recovered skill is counted once per load

- **WHEN** a run calls `get_skill` on the left-out ceiling skill
  `projected/forge-triage` twice
- **THEN** the skill is served both times, two call-log rows carry
  `loadout_miss = "skill"`, and
  `harness_loadout_misses_total{harness="qwen-worker",axis="skill"}` increases
  by two

#### Scenario: A narrowed write is not a miss

- **GIVEN** a run narrowed by `on_warn = "narrow"`
- **WHEN** it calls a write-tier tool its policy allows
- **THEN** the call is refused with `not_permitted`, and its call-log row
  carries no `loadout_miss`

### Requirement: REQ-18 — Eval Arms

This requirement is stated against ADR-0036 "Suites live with the skill",
"Trials are supervised runs" and "What is recorded". A `case.toml` MAY
declare:

* `loadout`, the name of a loadout without `lanes`;
* `event`, a path inside the case directory to a SPEC-0014 event envelope. It
  is required when `loadout` is set.

Such a case SHALL run two arms on each target, `loadout` and `ceiling`, in
place of `with` and `without`. For each trial:

* Both arms SHALL get the same scaffold, fixtures, target, pins, budgets,
  ceiling and event file (`HARNESS_EVENT_FILE`), with the case's `prompt` as the
  `default` template.
* Only the `loadout` arm SHALL make a decision (REQ-3 to REQ-13), from the
  envelope's task text. The `ceiling` arm SHALL run as `fallback = "ceiling"`
  would.
* Each trial row SHALL carry its arm, and the `loadout` arm's run record SHALL
  carry its `loadout` object.
* The aggregate SHALL report, per arm and side by side, the trial count, pass
  rate, mean score, turns, tokens and cost. It SHALL also report the `loadout`
  arm's mean score minus the `ceiling` arm's, with the standard error ADR-0036
  computes for its delta.
* Router calls SHALL be metered as gate calls and counted toward the suite's
  `max_cost_usd`.
* A loadout comparison SHALL NOT gate promotion, merge or regression (ADR-0036
  "Gates"), and SHALL NOT turn a loadout on or off. It is evidence for the
  operator.

`harness skills lint` SHALL fail a case that sets `loadout` without `event`,
names an undeclared loadout, or names a lanes loadout.

#### Scenario: Two arms, side by side

- **GIVEN** a case with `loadout = "small"`, an `event` file, `trials = 5` and
  one target
- **WHEN** `harness eval run` runs it
- **THEN** ten trials run, five per arm; each `loadout`-arm run record carries
  a `loadout` object and no `ceiling`-arm record does; and the report shows
  pass rate, tokens and cost for both arms with the delta

#### Scenario: A comparison needs an event

- **WHEN** a case sets `loadout = "small"` and no `event`
- **THEN** `harness skills lint` exits non-zero, naming the case and `event`

### Requirement: REQ-19 — Metrics

On the existing `/metrics` listener (SPEC-0013), the daemon SHALL export:

```text
harness_loadout_decisions_total{loadout,outcome}   counter
harness_loadout_duration_seconds{loadout}          histogram
harness_loadout_items{loadout,axis}                histogram
harness_loadout_misses_total{harness,axis}         counter
harness_loadout_invalid_total{loadout}             counter
```

* `outcome` is `applied` or a REQ-10 reason. Every declared loadout SHALL
  report every outcome it can produce, including zeros. A lane call counts
  under its lanes loadout's name.
* `harness_loadout_duration_seconds` observes every decision that started,
  `no_event` excluded.
* `harness_loadout_items` observes, for each applied decision, the number of
  items the run received on each narrowed axis. `axis` is `skill` or `tool`,
  here and on misses.
* `harness_loadout_invalid_total` increases by each decision's `invalid`.
* `loadout` and `harness` values SHALL be capped, with overflow collapsed into
  `__other__`, as SPEC-0013 REQ-5 caps `harness`. No label SHALL carry a skill,
  tool, template, family, model, brief or any task text.
* Decisions made by `harness loadout explain` SHALL NOT increment these
  series. They are metered as gate calls.

#### Scenario: A timeout is counted

- **WHEN** a decision for loadout `small` times out
- **THEN** `harness_loadout_decisions_total{loadout="small",outcome="timeout"}`
  increases by one, and `harness_loadout_duration_seconds{loadout="small"}`
  observes about `timeout`

### Requirement: REQ-20 — CLI And Visibility

`harness loadout explain <harness> --event FILE [--narrow] [--json]` SHALL:

* validate the envelope as `harness trigger --event` does (SPEC-0014 REQ
  "Manual Trigger With Event"), and answer `invalid_event` otherwise;
* run REQ-5 to REQ-9 for that harness as a firing would, without screening the
  event, as `harness trigger --event` is not screened. `--narrow` applies
  REQ-14's read-only ceiling;
* print the loadout and its mode; per axis, the ceiling size, the candidates in
  rank order, whether the axis is unsupported, and whether retrieval was
  degraded; the names the router returned, each one found `invalid`, and the
  resulting kit; the brief, inside a fence marked untrusted; the fallback the
  run would get and why; and one line stating the calls it made, either one
  router call to `model` with the served model and duration, or, in `retrieval`
  mode, that no chat model was called;
* spawn nothing, write no run record, ledger line, brief file or skill
  directory, and register no overlay.

`harness loadout explain --lanes <loadout> --event FILE` SHALL make the lane
call; print the lane catalog, the reply, the choice, and whether the first lane
was taken by default and why; then explain the chosen harness's kit as above;
and state how many calls it made.

Both are daemon operations in the `control` tier (ADR-0034). The CLI holds no
model credential, and every call they make is a real, metered gate call.

`harness loadout misses [harness]` SHALL list each miss (time, harness, run
id, loadout, axis and item) and a count per harness, axis and item. It takes
`--since DURATION` (default `7d`), `--axis skill` or `--axis tool`, and
`--json`. It is a `read`-tier operation.

`harness describe` SHALL show a harness's `loadout` and, per axis, whether the
harness can receive it narrowed. `harness doctor` SHALL add a warn row for
each harness that:

* names a loadout with `max_skills` above 0 while its adapter declares no
  `skills.per_run`;
* names a loadout, or has `narrow` among its effective levels, while it has no
  gateway session or does not set `mcp_exclusive = true`;
* has `narrow` among its effective levels while its adapter declares no
  `tools.read_only`.

#### Scenario: Explain spawns nothing and says it called

- **WHEN** an operator runs
  `harness loadout explain qwen-worker --event 41.event.json` against a
  model-mode loadout
- **THEN** a fake model server receives exactly one request, the output says
  one router call was made and names the served model, and no process, ledger
  line, brief file or skill directory appears

#### Scenario: Explain in retrieval mode

- **WHEN** the same command runs against a retrieval-mode loadout
- **THEN** the fake model server receives no request to `/chat/completions`,
  and the output says no chat model was called

#### Scenario: Misses by harness

- **GIVEN** three skill misses and one tool miss for `qwen-worker` in the last
  day
- **WHEN** the operator runs `harness loadout misses qwen-worker`
- **THEN** four rows are listed with their run ids and items, and the summary
  counts three `skill` and one `tool`

### Requirement: REQ-21 — Amendments To Existing Specs

This spec SHALL be read as amending the following, and where they conflict
this spec governs:

* **SPEC-0005 REQ "Caller Identity".** Each spawn's token is bound to its run
  id when minted, and the gateway resolves a session's run from the token
  (REQ-12).
* **SPEC-0006 REQ "Spawn-Time Projection".** A run whose skills axis is
  narrowed is projected into its own `<run_id>.skills/` directory, and the
  adapter's `target` is not written for that run (REQ-11). Every other start
  projects as before.
* **SPEC-0007 REQ "Search And Retrieval Tools".** For a run whose skills axis
  is narrowed, `search_skills` and `get_skill` also serve its skill ceiling as
  `projected/<name>`, and are registered for it even with no skill repo
  declared (REQ-11). A `get_skill` of a left-out ceiling skill is a skill miss
  (REQ-17).
* **SPEC-0007 REQ "Skill Repos".** `projected` is a reserved skill repo name.
* **SPEC-0007 REQ "Embedded Retrieval Index".** The index also holds the
  ceiling skills of each harness that names a loadout, rebuildable from their
  roots, and serves them only as REQ-11 allows (REQ-6).
* **SPEC-0013.** The `harness_loadout_*` series (REQ-19).
* **SPEC-0014 REQ "Event Delivery To The Run".** `HARNESS_BRIEF_FILE` joins the
  run variables (REQ-13). The brief file and the per-run skill directory are
  pruned with the event file. A brief's path, never its text, enters a
  variable.
* **SPEC-0017 REQ-7.** The context gains the `loadout.*` paths (REQ-13).
* **SPEC-0017 REQ-10.** `loadout.brief` joins the untrusted paths, usable only
  as `{{untrusted loadout.brief}}` under `untrusted_inline = true`.
* **SPEC-0017 REQ-11.** The brief file joins the event and prompt files as a
  persisted copy of run text, pruned with the run.
* **SPEC-0017 REQ-12.** `HARNESS_BRIEF_FILE` follows `HARNESS_PROMPT_FILE`'s
  override rule.
* **SPEC-0022 REQ-4.** Records gain `loadout` (REQ-16), written in an
  `updated` line synced before spawn.
* **SPEC-0026 REQ-3.** `loadout` joins the keys a manifest's `[harness]` table
  SHALL NOT carry.
* **SPEC-0031 REQ-8.** `loadout` is the third entry point on the closed list.
  It covers the router call (REQ-8), the lane call (REQ-15), and the calls
  `harness loadout explain` makes at the operator's request (REQ-20).
* **SPEC-0031 REQ-12.** The levels gain `narrow` between `notify` and `hold`
  (REQ-14).
* **SPEC-0031 REQ-16.** A run record's `screen.level` may be `narrow`.

Requirements stated against unspecified ADRs, which their specs SHALL carry
when written:

* ADR-0035: run-scoped sessions, overlays, `full` exposure over an overlay, and
  the call log's `loadout_miss` field (REQ-12, REQ-17);
* ADR-0036: index entries for ceiling skills, and eval arms (REQ-6, REQ-18);
* ADR-0039: the `skills.per_run` and `tools.read_only` declarations (REQ-11,
  REQ-14).

`[loadout.*]` joins ADR-0009's global-only list (REQ-1).

#### Scenario: A harness without a loadout is unchanged

- **GIVEN** a triggered harness with no `loadout` and no `narrow` level
- **WHEN** an event fires it
- **THEN** its argv, projection target, gateway tool list and record are those
  it had before this spec, and no retrieval or model request is made

### Requirement: Error Handling Standards

All operations in this spec SHALL follow structured error handling:

- Errors SHALL be wrapped with context at each layer boundary, naming the
  harness, the loadout and the run id, and the file and line for config errors.
- Sentinel errors SHALL be defined for: unknown loadout, invalid loadout table,
  loadout on an ineligible harness, template failure at spawn, router timeout,
  router transport failure, router parse failure, served-model mismatch, index
  failure, per-run projection failure, brief write failure, a kit item outside
  the ceiling, and an overlay refusal (`not_permitted`).
- A decision failure SHALL always be a recorded fallback, never a silent
  default and never a dropped run. A failure while building the kit (template,
  projection, brief) SHALL always be a failed start with a record.
- Logs SHALL be structured key-value, and SHALL NOT include task text, the
  brief, the router's reply or a description. They name the harness, loadout,
  run id, reason and counts.

#### Scenario: A failure is never silent

- **WHEN** any step of a decision fails
- **THEN** the run's record names the fallback reason,
  `harness_loadout_decisions_total` counts it, and one log line names the
  harness, loadout, run id and reason with no text from the event
