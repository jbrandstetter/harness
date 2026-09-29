---
status: draft
date: 2026-09-29
implements: [ADR-0042]
extends: [SPEC-0002, SPEC-0003, SPEC-0004, SPEC-0008, SPEC-0013, SPEC-0014, SPEC-0021, SPEC-0022, SPEC-0026]
---

# SPEC-0031: Input Screening

## Overview

This spec adds input screening: a classifier model reads untrusted text before
an agent does, and its verdict may hold or refuse work, never add it.

* **Guards**: classifier endpoints declared in a global-only `[guard.<name>]`
  table, each with natural-language **policies** and thresholds. A closed set
  of **kinds** fixes each guard's request shape and parser: `yesno-logprob`
  (a calibrated yes/no probability read from logprobs) and `labels` (a safety
  label and categories).
* **Gate calls**: a second class of daemon model call. A gate call may
  withhold work, and it may be waited on only at a named entry point.
* **Two entry points**: every verified webhook delivery and channel
  notification, screened once at `source.Manager.Fire` before fan-out and
  before admission; and every file a stable install or upgrade scans, screened
  through the daemon's `screen` op so the CLI never holds the guard's key.
* **Layered trust**: `[screen]` defaults, overridden per source and per
  harness, nearest-wins per key, with an exemption for a source's trusted
  actors.
* **Narrow-only levels**: `annotate < notify < hold < block`, applied
  per receiving harness to that harness's own verdict. A hold is a skipped run
  record plus its kept event file, released later through the manual-trigger
  path. **Shadow mode** records what would have happened and acts on nothing.
* **Recording without text**: verdicts, scores and a content hash reach the
  run record, the ledger, notifications and metrics. No byte of screened text
  does.

See ADR-0042 for the decision and the options it rejected. This spec amends
SPEC-0002 REQ "Control Operations", SPEC-0003 REQ "Operator Notification",
SPEC-0004 REQ "Project File Schema" and REQ "Project Control Operations",
SPEC-0008 REQ "Per-Run Logs", SPEC-0013, SPEC-0014 (several requirements),
SPEC-0021 REQ-4, SPEC-0022 REQ-2, REQ-4, REQ-5 and REQ-17, and SPEC-0026 REQ-3
and REQ-5. REQ-21 lists every amendment in one place.

Requirements are numbered. Cite them as `SPEC-0031 REQ-n`.

## Requirements

### Requirement: REQ-1 — Guard Table

The global configuration SHALL accept any number of `[guard.<name>]` tables,
`<name>` matching `^[a-z][a-z0-9_-]{0,31}$`. A guard is a classifier endpoint
on the OpenAI-compatible chat-completions API. Harness calls it; Harness never
serves it.

| Key | Type | Required | Default | Meaning |
|---|---|---|---|---|
| `kind` | `yesno-logprob` or `labels` | yes | | Fixes the request and the parser (REQ-3, REQ-4) |
| `base_url` | `http` or `https` URL | no | `[model_api].base_url` | The endpoint's `/v1` root; the daemon posts to `<base_url>/chat/completions` |
| `model` | string, 1 to 256 bytes | yes | | The exact model id the server reports, never a gateway alias (REQ-6) |
| `env_file` | path | when `api_key` is set | | The file `api_key` resolves from (ADR-0038) |
| `api_key` | exactly one `${NAME}` reference | no | | Sent as `Authorization: Bearer <value>` |
| `timeout` | duration, `100ms` to `20s` | no | `5s` | Bounds one call, slot wait included (REQ-8) |
| `max_concurrency` | integer, 1 to 64 | no | 4 | Calls to this guard in flight at once, daemon-wide |
| `max_input_tokens` | integer, 256 to 1,000,000 | no | 8192 | The chunk budget for screened text (REQ-5) |
| `max_chunks` | integer, 1 to 64 | no | 8 | More chunks than this is an error (REQ-5) |
| `description` | string | no | | Operator prose |

A guard also carries its policies as sub-tables (REQ-2). Any other key SHALL
fail the load, naming it.

`api_key` is secret-typed under ADR-0038: a literal value, a value that is not
exactly one `${NAME}` reference, or a reference `env_file` does not define SHALL
fail the load, naming the key and the reference and never the value. The
reference SHALL resolve only from the guard's own `env_file`. A guard does not
inherit `[model_api]`'s `api_key` or `env_file`; a guard without `api_key` sends
no `Authorization` header. `base_url` unset with no `[model_api].base_url`
SHALL fail the load, naming the guard. An `http` `base_url` whose host is not a
loopback address, on a guard that sets `api_key`, SHALL load with a WARN naming
the guard, and `harness doctor` SHALL warn about it.

`[guard.*]` SHALL be accepted only in the main global `harness.toml`. It SHALL
be rejected in a `harness_d` drop-in, in a project `harness.toml` and on the
project-up wire, joining ADR-0009's global-only list, because a cloned
repository must not choose, weaken or re-point its own screening. A reload
SHALL apply guard changes to screens that start after it; a screen in progress
SHALL finish with the configuration it started with.

#### Scenario: A minimal guard

- **GIVEN** `[model_api] base_url = "http://127.0.0.1:4000/v1"`
- **WHEN** the global file declares `[guard.shield]` with `kind =
  "yesno-logprob"`, `model = "mistralai/Shieldstral-1.0-3B"` and one policy
- **THEN** the config loads, and the guard's effective `base_url` is
  `[model_api]`'s, its `timeout` `5s` and its `max_concurrency` 4

#### Scenario: A literal key is refused

- **WHEN** a guard sets `api_key = "sk-live-123"`
- **THEN** the load fails naming `api_key` and requiring a `${NAME}` reference
  in `env_file`, and the error does not contain `sk-live-123`

#### Scenario: A drop-in cannot re-point a guard

- **WHEN** a file in `harness_d/` declares `[guard.shield] base_url =
  "http://attacker.example/v1"`
- **THEN** the load fails naming `[guard.*]` as allowed only in the global
  `harness.toml`, and the daemon keeps its last good configuration

### Requirement: REQ-2 — Policies

A guard SHALL declare one or more policies as `[guard.<g>.policy.<p>]`, `<p>`
matching the guard-name pattern. A policy is referenced everywhere as the
string `"<g>.<p>"`. A reference to an undeclared guard or policy SHALL fail the
load, naming the reference and the key that holds it.

| Key | Kinds | Type | Default | Meaning |
|---|---|---|---|---|
| `instruct` | `yesno-logprob` | string, at most 16 KiB | | The natural-language policy |
| `instruct_file` | `yesno-logprob` | path | | The same, read from a file |
| `query` | `yesno-logprob` | string, at most 1 KiB | required | The yes/no question asked of the document |
| `flag_on` | `yesno-logprob` | `yes` or `no` | `yes` | Which answer means "flag" |
| `flag_at` | `yesno-logprob` | number | required | Score at or above which the verdict is `flag` |
| `warn_at` | `yesno-logprob` | number | `flag_at` | Score at or above which the verdict is `warn` |
| `unsafe_label` | `labels` | string, 1 to 64 bytes | `unsafe` | The first-line label that means unsafe |
| `safe_label` | `labels` | string, 1 to 64 bytes | `safe` | The first-line label that means safe |
| `flag_categories` | `labels` | list of strings | `[]` | Categories that flag; empty means any |
| `description` | both | string | | Operator prose |

A `yesno-logprob` policy SHALL set exactly one of `instruct` and
`instruct_file`. `instruct_file` SHALL resolve like `prompt_file` (relative to
the file that declares it, `~` expanded), SHALL be read at load and on reload,
and SHALL obey the 16 KiB cap. Thresholds SHALL satisfy
`0 < warn_at <= flag_at <= 1`. A key from the other kind's column, a missing
required key, a threshold out of range, or an unknown key SHALL fail the load,
naming the policy and the key. `unsafe_label` and `safe_label` SHALL differ.

#### Scenario: Both instruct keys

- **WHEN** a policy sets `instruct` and `instruct_file`
- **THEN** the load fails naming both keys as mutually exclusive

#### Scenario: Thresholds out of order

- **WHEN** a policy sets `warn_at = 0.9` and `flag_at = 0.8`
- **THEN** the load fails naming the policy and both keys

#### Scenario: A threshold on a labels guard

- **WHEN** a policy of a `labels` guard sets `flag_at = 0.5`
- **THEN** the load fails, stating that `labels` policies score only 0 or 1
  and take no thresholds

### Requirement: REQ-3 — The yesno-logprob Kind

For each chunk and policy, a `yesno-logprob` guard SHALL receive exactly one
`POST <base_url>/chat/completions` with:

* `model`: the guard's `model`;
* `messages`: a system message holding the kind's judge system prompt, then one
  user message carrying the policy's instruct text, its `query` and the chunk,
  in the sections and order the model card documents;
* `max_tokens = 1`, `temperature = 0`, `n = 1`, `stream = false`,
  `logprobs = true`, `top_logprobs = 20`;
* no `tools`, `tool_choice` or `response_format`.

The judge system prompt and the user-message layout SHALL be constants compiled
into the binary, fixed per kind, and pinned by a golden test that compares the
serialized request byte for byte. No configuration key SHALL change them.

The daemon SHALL read the score only from
`choices[0].logprobs.content[0].top_logprobs`, never from the message text:

1. Each entry's `token` is normalized by trimming leading and trailing Unicode
   white space and applying Unicode simple case folding.
2. `P(yes)` is the sum of `exp(logprob)` over entries that normalize to `yes`,
   and `P(no)` likewise for `no`.
3. When exactly one of the two sums is zero, that answer's probability SHALL be
   taken as `exp` of the smallest `logprob` in the list, an upper bound on any
   token the list omits.
4. `p_yes = P(yes) / (P(yes) + P(no))`, the two-way softmax.
5. The score is `p_yes` when `flag_on = "yes"`, and `1 - p_yes` when
   `flag_on = "no"`.

A response whose `choices[0].logprobs` is absent or null, whose `content` or
`top_logprobs` is empty, or whose list normalizes to neither `yes` nor `no`,
SHALL be the error `no_logprobs` (REQ-6), whatever its text says.

#### Scenario: Logprobs stripped by a proxy

- **WHEN** a response carries message content `no` and no `logprobs`
- **THEN** the result is `error` with class `no_logprobs`, not `allow`

#### Scenario: Case and spacing variants add up

- **WHEN** `top_logprobs` holds ` Yes` at -0.2, `yes` at -2.5 and `No` at -1.9,
  and `flag_on = "yes"`
- **THEN** the score is the two-way softmax of `exp(-0.2) + exp(-2.5)` against
  `exp(-1.9)`

#### Scenario: Only one answer listed

- **WHEN** `top_logprobs` lists `yes` at -0.01 and 19 other tokens down to
  -9.0, and no `no`
- **THEN** `P(no)` is taken as `exp(-9.0)`, and the score is below 1.0

### Requirement: REQ-4 — The labels Kind

For each chunk and policy, a `labels` guard SHALL receive exactly one
`POST <base_url>/chat/completions` whose `messages` hold one user message whose
content is the chunk, so the server applies the model's own chat template, with
`model` set, `max_tokens = 32`, `temperature = 0`, `n = 1`, `stream = false`,
and no system message, `tools` or `logprobs`.

The daemon SHALL parse `choices[0].message.content`: its first line, trimmed of
white space, SHALL equal `unsafe_label` or `safe_label`; anything else is the
error `parse` (REQ-6). The categories are the second line, if any, split on
commas and trimmed. The score SHALL be `1.0` when the first line is
`unsafe_label` and either `flag_categories` is empty or a category equals one
of its entries, and `0.0` otherwise. A score of `1.0` is `flag` and `0.0` is
`allow`; a `labels` policy never yields `warn`.

#### Scenario: A listed category flags

- **WHEN** `flag_categories = ["S14"]` and the response reads `unsafe` then
  `S2,S14`
- **THEN** the score is 1.0 and the result is `flag`

#### Scenario: An unlisted category does not

- **WHEN** the same policy receives `unsafe` then `S2`
- **THEN** the score is 0.0 and the result is `allow`

#### Scenario: Garbage is not safe

- **WHEN** the response's first line is `I cannot help with that`
- **THEN** the result is `error` with class `parse`

### Requirement: REQ-5 — Chunking

Screened text SHALL be chunked before any call. A guard's chunk size SHALL be
`3 × max_input_tokens` bytes, a fixed, conservative estimate of three bytes per
token. The overlap SHALL be 1,024 bytes, or a quarter of the chunk size when
that is smaller.

* Text of at most one chunk size is one chunk.
* Otherwise each chunk ends at the last UTF-8 character boundary at or before
  its start plus the chunk size, and the next chunk starts at the first
  character boundary at or after that end minus the overlap. No chunk splits a
  UTF-8 sequence.
* When the text needs more than `max_chunks` chunks, the daemon SHALL make no
  call for that guard, and every policy of that guard SHALL yield the error
  `too_large` (REQ-6). Padding a payload therefore cannot push an instruction
  past the screen; it can only produce an error, which `on_error` governs.
* Empty text SHALL make no call and yield `allow` with zero chunks.

Each chunk SHALL be scored against each policy independently. The number of
chunks is recorded (REQ-16).

#### Scenario: Padding does not hide an instruction

- **GIVEN** a guard with `max_input_tokens = 1000` and `max_chunks = 4`
- **WHEN** a body of 20 KB of filler ends in an instruction
- **THEN** no call is made to that guard, and the result is `error` with class
  `too_large`

#### Scenario: An instruction across a boundary

- **GIVEN** the default `max_input_tokens`, so the overlap is 1,024 bytes
- **WHEN** a 600-byte instruction starts 300 bytes before a chunk boundary
- **THEN** one chunk contains the whole instruction, because the overlap is
  larger than it

#### Scenario: Multi-byte text

- **WHEN** a body of CJK text is chunked
- **THEN** every chunk is valid UTF-8, and concatenating the chunks without
  their overlaps reproduces the text

### Requirement: REQ-6 — Attestation And Error Classes

A call that cannot produce a score SHALL yield `error` with exactly one class
from this closed set:

| Class | When |
|---|---|
| `timeout` | No parsed response within the guard's `timeout`, slot wait included (REQ-8) |
| `transport` | A connection, TLS or read failure |
| `http_status` | Any status other than 200, including a redirect, which is never followed |
| `parse` | A body that is not the expected JSON, a body over 1 MiB, no `choices`, or a `labels` first line that is neither label |
| `no_logprobs` | REQ-3's missing or unusable logprobs |
| `model_mismatch` | The response's `model` differs from the configured `model` |
| `too_large` | REQ-5's chunk limit, or REQ-19's request cap |

The response's `model` field SHALL equal the guard's `model` exactly, compared
byte for byte. A response with a different or missing `model` SHALL be
`model_mismatch`, even when it carries a usable score, because a threshold tuned
on one model means nothing on another (ADR-0026). The model the response named
SHALL be recorded as `served_model`, capped at 256 bytes with control
characters removed.

A failed call SHALL NOT be retried within the same screen. A new class is a
spec amendment.

#### Scenario: The gateway answered with another model

- **WHEN** a guard's `model` is `mistralai/Shieldstral-1.0-3B` and the response
  names `qwen3-30b-a3b` with valid logprobs
- **THEN** the result is `error` with class `model_mismatch`, and
  `served_model` reads `qwen3-30b-a3b`

#### Scenario: A redirect is not followed

- **WHEN** the guard answers `307` with a `Location` on another host
- **THEN** no request reaches that host, and the result is `error` with class
  `http_status`

### Requirement: REQ-7 — Verdict Computation

A score against a policy SHALL become a result: `allow` when the score is below
`warn_at`, `warn` when it is at least `warn_at` and below `flag_at`, and `flag`
when it is at least `flag_at`. A call with no score is an `error` result.

A **verdict** aggregates the results of a set of policies over every chunk.
Its precedence SHALL be `flag > error > warn > allow`: `flag` when any
result flags; otherwise `error` when any result errored; otherwise `warn` when
any result warns; otherwise `allow`. An error is reported beside any score that
was produced, and decides only when nothing flagged.

The **deciding result** SHALL be, among the results at the verdict's severity,
the one with the highest score, ties going to the earlier policy in the
harness's effective `policies` list and then the lower chunk index. For an
`error` verdict it is the highest-scoring result produced, if any. The
verdict's `error` field is the class of the first errored result in policy
order, then chunk order.

#### Scenario: A flag outranks an error

- **WHEN** one policy flags chunk 2 and another times out on chunk 1
- **THEN** the verdict is `flag`, and the record also carries `error` `timeout`

#### Scenario: An error outranks a warning

- **WHEN** one policy warns and another returns `no_logprobs`
- **THEN** the verdict is `error`, with the warning's score as the deciding
  score

#### Scenario: Equal thresholds

- **WHEN** `warn_at = flag_at = 0.7` and the score is 0.7
- **THEN** the result is `flag`

### Requirement: REQ-8 — Gate Calls

A **gate call** is a daemon model call whose result may withhold work. Every
call REQ-3 and REQ-4 define is a gate call. Gate calls amend ADR-0036's utility
calls in exactly two rules, what their result may do and when they may be
waited on, and keep the rest:

* **One request, no tools, no loop.** A gate call is one request per chunk and
  policy. Its output is never fed back to any model.
* **Narrow only.** A verdict SHALL only withhold or narrow work the
  configuration would otherwise admit. No verdict SHALL start a run, widen a
  permission, select a command or change configuration.
* **Waited on only at a named entry point.** A gate call SHALL be waited on
  only before an event is admitted (REQ-11) and before a stable is installed or
  upgraded (REQ-18). SPEC-0032 adds a third, before a routed one-shot spawns. No
  other path, including supervision, attach, recording, a resident harness's
  restart, a schedule or catch-up firing, `harness trigger` and
  `harness screen release`, SHALL wait on a gate call. A new entry point is a
  spec amendment that names it.
* **Redacted input.** The text of every chunk SHALL pass the daemon's
  credential redactor before it leaves the daemon. The content hash (REQ-16) is
  taken before redaction.
* **Bounded concurrency.** Each guard SHALL have its own pool of
  `max_concurrency` slots, shared by every screen that uses it and by no other
  model call, so utility calls cannot starve a guard. A call that waits for a
  slot SHALL count that wait against its `timeout`: a call that has not
  received a parsed response `timeout` after it asked for a slot SHALL be
  abandoned with class `timeout`. The calls of one screen SHALL be issued
  together, so one screen ends within the largest `timeout` among the guards it
  uses.
* **Never under the admission lock.** No gate call SHALL be made or awaited
  while SPEC-0021 REQ-4's admission lock is held.
* **Metered on its own.** Gate calls SHALL be counted in their own series
  (REQ-17) and SHALL NOT count against any harness's SPEC-0021 budget.
* **Cancelled at shutdown.** A screen cancelled by daemon shutdown SHALL NOT
  become an `error` verdict. Its firings are abandoned before the run entry
  point, as SPEC-0014 REQ "Concurrency Safety" provides.

#### Scenario: A credential in a payload never reaches the guard

- **WHEN** an issue body contains `Authorization: token ghp_abc123` and is
  screened
- **THEN** the request the fake guard receives carries the redacted form, and
  the recorded hash is of the unredacted text

#### Scenario: A slow guard bounds its own wait

- **GIVEN** a guard with `max_concurrency = 1`, `timeout = "2s"`, and a server
  that takes 1.5 s per call
- **WHEN** one screen needs three calls to it
- **THEN** the first succeeds, the other two end with class `timeout` about 2 s
  after the screen started, and the screen ends within about 2 s

#### Scenario: Admission is never behind a guard

- **GIVEN** a guard that never answers
- **WHEN** a screened firing waits on it and a manual `harness start` of another
  budgeted harness arrives
- **THEN** the start is admitted without waiting for the guard

### Requirement: REQ-9 — Screen Defaults

The main global `harness.toml` MAY carry one `[screen]` table:

| Key | Type | Default | Meaning |
|---|---|---|---|
| `policies` | list of `"<guard>.<policy>"` | `[]` | The policies an event is screened against, unless overridden (REQ-10) |
| `mode` | `enforce`, `shadow` or `off` | `enforce` | REQ-12, REQ-13 |
| `on_flag` | a level (REQ-12) | `hold` | Level for a `flag` verdict |
| `on_warn` | a level | `notify` | Level for a `warn` verdict |
| `on_error` | a level | `hold` | Level for an `error` verdict |
| `hold_ttl` | duration, `1h` to `30d` | `7d` | How long a held event file is kept (REQ-14) |
| `packages` | `enforce`, `shadow` or `off` | `enforce` | Stable install and upgrade screening (REQ-18) |
| `packages_on_error` | `high` or `low` | `high` | Finding severity when package screening errors (REQ-18) |

The defaults in the table are the **built-in layer**. They apply only to a
harness whose effective `policies` (REQ-10) is non-empty. With no policies
configured anywhere, nothing SHALL be screened, no guard SHALL be called, and
every behavior SHALL be exactly as it was before this spec. Declaring guards
without referencing a policy screens nothing.

`[screen]` SHALL be accepted only in the main global `harness.toml` and
rejected in a `harness_d` drop-in, a project `harness.toml` and on the
project-up wire. `hold_ttl` greater than `[ledger] retention` (SPEC-0022 REQ-12)
SHALL fail the load, naming both keys, so no held file outlives its record. An
unknown key, a value outside its type, or a policy reference REQ-2 cannot
resolve SHALL fail the load, naming the key.

#### Scenario: Nothing configured, nothing screened

- **GIVEN** two guards declared and no `policies` key anywhere
- **WHEN** 100 verified deliveries fan out to three harnesses
- **THEN** the fake guard servers receive no request, and no run record
  carries a `screen` object

#### Scenario: A drop-in cannot set the defaults

- **WHEN** a `harness_d` file declares `[screen] mode = "off"`
- **THEN** the load fails naming `[screen]` as allowed only in the global
  `harness.toml`

### Requirement: REQ-10 — Source And Harness Overrides

A `[webhook.*]` table, a `[channel.*]` table and a `[harness.*]` table MAY carry
a `screen` table (inline, `screen = { … }`, or as a sub-table) with these keys:

| Key | Allowed on | Meaning |
|---|---|---|
| `policies` | source, harness | As `[screen] policies`; `[]` turns screening off for that layer's events |
| `mode` | source, harness | As `[screen] mode` |
| `on_flag`, `on_warn`, `on_error` | source, harness | As in `[screen]` |
| `skip_trusted_actors` | a `github`, `gitea` or `gitlab` webhook source only | Boolean, default `false` |

Any other key, including `hold_ttl`, `packages` and `packages_on_error`, SHALL
fail the load, naming it.

**Resolution.** For one event and one receiving harness, each key SHALL resolve
nearest-wins: the harness's `screen`, then the event source's `screen`, then
`[screen]`, then the built-in layer, as ADR-0011's merge already works. Each key
resolves independently: a harness that sets only `on_flag` takes `mode` and
`policies` from the source or `[screen]`.

**Trusted actors.** `skip_trusted_actors = true` SHALL be accepted only on a
webhook source whose `verify` SPEC-0017 REQ-9 allows `trusted_actors` on, and
only when that source declares `trusted_actors`; otherwise the load SHALL fail,
naming the key. When it is set and a delivery's sender matches the source's
`trusted_actors` by SPEC-0017 REQ-9's rule (the condition under which
`typed.actor` is set), the effective mode SHALL be `off` for every harness that
event reaches, after nearest-wins resolution and whatever layer set `mode`.

**Where `screen` may be set.** `screen` on a source or harness SHALL be accepted
in the main global `harness.toml` and in `harness_d` drop-ins. It SHALL be
rejected, naming the key and the table:

* on a harness in a project `harness.toml`;
* in a harness definition on the project-up wire, as SPEC-0017 REQ-14 rejects
  `untrusted_inline` there, with `ErrInvalidProjectDef`;
* in a package manifest's `[harness]` table, joining SPEC-0026 REQ-3's
  forbidden keys.

A cloned repository or a third-party package SHALL NOT be able to loosen or set
screening.

#### Scenario: Nearest wins, per key

- **GIVEN** `[screen] on_flag = "hold"`, `[webhook.github] screen = { mode =
  "shadow" }`, and `[harness.pr-review] screen = { on_flag = "block" }` bound
  to `webhook.github`
- **WHEN** `harness screen explain pr-review webhook.github` runs
- **THEN** it shows `mode` `shadow` from `source:webhook.github`, `on_flag`
  `block` from `harness`, and `on_warn` `notify` from `builtin`

#### Scenario: A stricter policy on a public source

- **GIVEN** `[screen] policies = ["shield.injection"]` and `[webhook.github]
  screen = { policies = ["shield.injection", "shield.exfil"] }`
- **WHEN** one delivery arrives on `webhook.github` and one on `webhook.gitea`
- **THEN** the first is screened against both policies and the second against
  `shield.injection` only

#### Scenario: A trusted sender on the private forge

- **GIVEN** `[webhook.gitea]` with `trusted_actors = ["joestump"]` and `screen
  = { skip_trusted_actors = true }`, and a harness bound to it with `screen =
  { mode = "enforce" }`
- **WHEN** a delivery from `joestump` arrives, then one from `mallory`
- **THEN** the first makes no guard call, and the second is screened

#### Scenario: A project file cannot turn screening off

- **WHEN** a project `harness.toml` declares `[harness.agent] screen = { mode =
  "off" }`
- **THEN** `harness up` fails naming `screen` as not allowed in a project file

#### Scenario: A package cannot loosen screening

- **WHEN** a package manifest's `[harness]` declares `screen = { on_flag =
  "annotate" }`
- **THEN** the package fails to load, naming `screen`

#### Scenario: skip_trusted_actors on an opaque source

- **WHEN** a `verify = "bearer"` source sets `screen = { skip_trusted_actors =
  true }`
- **THEN** the load fails naming `skip_trusted_actors`

### Requirement: REQ-11 — Event Screening At Fire

Every event SHALL be screened, when it is screened at all, at the event funnel
`source.Manager.Fire`: after verification, filtering, de-duplication and the
rate limit (SPEC-0014 REQ "Webhook Filtering"), before fan-out, before
admission, and outside the admission lock. For one event, `Fire` SHALL:

1. Resolve each bound harness's effective settings (REQ-10) for this event.
2. Decide operating hours first: a harness whose firing SPEC-0014 REQ
   "Operating Hours On Triggered Harnesses" records `outside_hours` SHALL NOT
   be screened.
3. Call a harness **screened** when its effective `mode` is `enforce` or
   `shadow` and its effective `policies` is non-empty.
4. When no harness is screened, make no call and fire exactly as SPEC-0014 REQ
   "Firing" does.
5. Otherwise derive the screened text once (below) and classify it once,
   against the **union** of the screened harnesses' effective policies: one call
   per chunk and distinct policy, whatever the number of receiving harnesses.
6. Compute each screened harness's verdict (REQ-7) from the results of its own
   effective policies only, and act on it (REQ-12).

An event SHALL be classified at most once. A harness that is not screened SHALL
be fired without waiting for any call. A screened harness SHALL wait at most the
largest `timeout` among the guards of its own policies, and SHALL NOT wait on
calls only other harnesses' policies need. Decisions SHALL still be reported
in config order (SPEC-0014 REQ "Webhook Responses").

The **screened text** SHALL be derived from the event envelope (SPEC-0014 REQ
"Event Delivery To The Run") alone, so its hash can be recomputed from a kept
event file:

| Source | Screened text |
|---|---|
| any webhook whose envelope carries a JSON `body` | Every JSON string value in the body, except a value that is wholly a URL, a hexadecimal identifier, a UUID or an RFC 3339 timestamp |
| any webhook whose envelope has no JSON `body` | The whole body: `body_text` verbatim, or `body_base64` decoded |
| channel | `content`, then every `meta` value in lexical key order |

For a JSON body, each string value SHALL be addressed by its path: object keys
and decimal array indices joined with `.`, for example `commits.0.message`.
Values SHALL be taken in lexical byte order of their paths, each distinct value
once (its first occurrence in that order), skipping empty strings. A value is
excluded when it matches, in full, one of: `^[A-Za-z][A-Za-z0-9+.-]*://\S*$`
(a URL), `^[0-9A-Fa-f]{7,64}$` (a hash or id), the canonical 8-4-4-4-12 UUID
form, or an RFC 3339 date-time. Taken values and channel fields SHALL be joined
with one blank line (`\n\n`). Invalid UTF-8 SHALL be replaced with U+FFFD
before hashing and chunking.

Taking every remaining string, rather than SPEC-0017 REQ-10's four untrusted
fields, is deliberate. On a public forge the sender also writes review bodies,
commit messages, branch names, labels and a fork's repository description, and
all of them reach the agent in the event file. The exclusions drop only values
that carry no prose, to save guard capacity.

These SHALL NOT be screened: `harness trigger --event` (the operator chose that
file), `harness screen release` (REQ-14), schedule and catch-up firings (they
carry no event), and anything an agent reads after it starts.

#### Scenario: Off makes no call, unless a receiver screens

- **GIVEN** `[webhook.gitea] screen = { mode = "off" }`
- **WHEN** a delivery fans out to two harnesses that set no `screen`
- **THEN** the fake guard receives no request
- **AND WHEN** a third harness bound to the source sets `screen = { mode =
  "enforce" }` and the same delivery is sent again
- **THEN** the fake guard receives one request per chunk and policy

#### Scenario: Three receivers, one classification

- **WHEN** one delivery with a 1-chunk body fans out to three screened
  harnesses that share one policy
- **THEN** the fake guard server counts exactly one request

#### Scenario: Each harness reads only its own policies

- **GIVEN** `pr-review` screens with `["shield.injection"]` and `pr-audit` with
  `["shield.injection", "shield.exfil"]`
- **WHEN** a delivery scores `allow` on `injection` and `flag` on `exfil`
- **THEN** the fake guard counts two requests, `pr-review` runs with verdict
  `allow`, and `pr-audit` applies its `on_flag`

#### Scenario: A slow guard does not delay an unscreened harness

- **GIVEN** a guard that sleeps longer than its `timeout`, and one screened and
  one unscreened harness on the same source
- **WHEN** a delivery arrives
- **THEN** the unscreened harness is admitted without waiting for the guard,
  and the screened one applies its `on_error`

#### Scenario: A review body and a commit message are screened

- **WHEN** a `pull_request_review` delivery's `review.body`, and a `push`
  delivery's `commits.0.message`, each carry an instruction to the agent
- **THEN** each delivery's screened text contains that instruction, and
  neither contains the payload's `html_url` or `head.sha` values

#### Scenario: A form-encoded GitHub delivery

- **WHEN** a `github` source receives `application/x-www-form-urlencoded`, so
  the envelope holds `body_text`
- **THEN** the whole body is screened, not an empty field set

#### Scenario: Replay is the operator's choice

- **WHEN** the operator runs `harness trigger pr-review --event 41.event.json`
- **THEN** no guard call is made, and the run's record carries no `screen`

### Requirement: REQ-12 — Levels And Per-Harness Action

The levels SHALL form one ordered list, lowest first:

| Level | Effect |
|---|---|
| `annotate` | The verdict is recorded on the run record and counted in the trigger outcome (REQ-16); nothing else happens |
| `notify` | Also sends `screen_flagged` (REQ-15) |
| `hold` | Also skips the firing with reason `screen_hold` and keeps its event file (REQ-14) |
| `block` | Also skips the firing with reason `screen_block`; no event file is written; the hash is kept |

Levels are cumulative: each has the effects of every level below it. The list
MAY be extended only by a spec amendment that names the new level's position;
SPEC-0032 inserts `narrow` between `notify` and `hold`. Every message that lists
levels SHALL list them in this order.

Each screened harness SHALL map its own verdict to a level: `allow` runs with no
level, `warn` applies its effective `on_warn`, `flag` its `on_flag`, and
`error` its `on_error`. In `enforce` mode the level's effects apply; in `shadow`
mode none do (REQ-13). A firing that runs, whatever its level, SHALL then go
through the ordinary run entry point, with its overlap policy, admission and
operating-hours rules unchanged.

#### Scenario: One delivery, two outcomes

- **GIVEN** `pr-review` with `on_flag = "hold"` and `pr-labels` with `on_flag =
  "annotate"`, both screened with one policy
- **WHEN** a delivery is flagged
- **THEN** `pr-review` is recorded `skipped` with reason `screen_hold`, and
  `pr-labels` runs with verdict `flag` and level `annotate` on its record

#### Scenario: The guard is down

- **GIVEN** an unreachable guard and the default `on_error`
- **WHEN** a delivery reaches a screened harness
- **THEN** the firing is held and `screen_flagged` is sent with verdict `error`
- **AND WHEN** the harness sets `screen = { on_error = "annotate" }`
- **THEN** the next delivery runs, with verdict `error` and class `transport`
  on its record

### Requirement: REQ-13 — Shadow Mode

A screened harness whose effective mode is `shadow` SHALL be screened exactly as
in `enforce`, waiting as REQ-11 describes, and SHALL record its verdict with
`mode` `shadow` and the level that `enforce` would have applied. It SHALL NOT
skip a firing, write a held event file, or send `screen_flagged`. Its firing
SHALL proceed as though the verdict were `allow`.

`harness screen report` (REQ-20) SHALL show shadow verdicts and would-have
levels beside enforced ones, so thresholds can be tuned against real
deliveries before `mode = "enforce"`.

#### Scenario: Shadow never holds

- **GIVEN** a harness in `shadow` with `on_flag = "block"`
- **WHEN** a delivery is flagged
- **THEN** the run starts, its record carries verdict `flag`, mode `shadow`
  and level `block`, and no notification is sent

#### Scenario: Shadow never notifies on error

- **WHEN** the guard is unreachable for a `shadow` harness
- **THEN** the run starts with verdict `error`, and no `screen_flagged` is sent

### Requirement: REQ-14 — Holds, Release, Drop And Expiry

**Hold.** A firing at level `hold` SHALL be recorded `skipped` with reason
`screen_hold`, as a `decided` ledger record with its own run id. The daemon
SHALL then write the event envelope, mode `0600`, to
`<jobs dir>/<harness>/<run_id>.event.json`, the path SPEC-0014 REQ "Event
Delivery To The Run" gives a run's event file, although no process starts. When
the file cannot be written, the firing SHALL stay skipped, an ERROR SHALL be
logged naming the harness and run id, and a later release of it SHALL fail
naming the missing file.

**Block.** A firing at level `block` SHALL be recorded `skipped` with reason
`screen_block`, and no event file SHALL be written. Its `screen.hash` is kept.

A `screen_hold` or `screen_block` skip SHALL NOT coalesce with any other record
(SPEC-0014 REQ "Overlap Skip Coalescing"): each held or blocked event is its own
record.

**No budget.** Screening precedes admission, so a held or blocked firing SHALL
NOT reach SPEC-0021 REQ-4's admission: it takes no run-count unit, no
concurrency slot and no daily-cost headroom.

**Release.** `harness screen release <harness> <run_id>` SHALL start a run of
that harness carrying the held envelope, through the manual-trigger path
(SPEC-0008 REQ "Manual Trigger", SPEC-0014 REQ "Manual Trigger With Event").
That run SHALL be recorded with trigger `manual`, the held record's `source`
and `event_id`, and `released_from` set to the held run id. Its event file
SHALL be identical to the held file except for the added `replayed_at`. It
SHALL NOT be screened. It SHALL pass admission, overlap and operating hours as
`harness trigger` does, and SHALL accept `--wait` and `--over-budget` with
their SPEC-0008 and SPEC-0021 REQ-15 meanings. When the released firing starts
or is queued, the held record SHALL gain `released_at`; when it is skipped or
refused, the hold SHALL stay as it was and the command SHALL say why. Release
SHALL NOT delete the held file. A second release of the same hold SHALL proceed
and warn that it was already released, and when. Release SHALL fail, naming the
reason, for a record that is not a `screen_hold` skip of that harness, or whose
file is gone.

**Drop.** `harness screen drop <harness> <run_id>` SHALL delete the held event
file, confirm the path is absent, and then give the record `hold_dropped`.

**Expiry.** At daemon start and at least once an hour, the daemon SHALL delete
the event file of every `screen_hold` record whose `ended_at` (the decision
time) is older than `hold_ttl`, confirm the path is absent, and then give the
record `hold_expired`. `hold_expired` is set whether or not the hold was
released.

**Retention.** A `screen_hold` record's event file SHALL be deleted only by
drop, expiry, or the removal of its harness's jobs directory. `keep_runs` SHALL
NOT delete it (amending SPEC-0008 REQ "Per-Run Logs").

A hold is a ledger record plus a file. No queue exists, and no firing SHALL be
buffered in memory waiting for a release.

#### Scenario: Hold, then release

- **GIVEN** a harness with `on_flag = "hold"` and a fake guard scoring 0.95
  against `flag_at = 0.85`
- **WHEN** a delivery arrives
- **THEN** the history gains a `skipped` record with reason `screen_hold`, its
  event file exists with mode `0600`, and one `screen_flagged` is sent
- **AND WHEN** the operator runs `harness screen release <harness> <run_id>`
- **THEN** a run starts whose event file equals the held file plus
  `replayed_at`, whose record carries `released_from`, and the fake guard
  receives no request

#### Scenario: A block keeps only the hash

- **WHEN** a firing is blocked
- **THEN** no `<run_id>.event.json` exists for it, and its record carries
  `screen.hash`

#### Scenario: Expiry removes the file

- **GIVEN** `hold_ttl = "1h"` and a hold decided two hours ago
- **WHEN** the expiry pass runs
- **THEN** the held path does not exist (checked by `stat`, not by the delete
  call's result), and the record carries `hold_expired`

#### Scenario: A hold spends no budget

- **GIVEN** `max_runs_per_day = 1` and no runs today
- **WHEN** a held firing arrives, then an allowed one
- **THEN** the allowed firing is admitted and runs

#### Scenario: keep_runs spares the evidence

- **GIVEN** `keep_runs = 2` and a hold decided before three later runs
- **WHEN** the third later run finishes and pruning runs
- **THEN** the held event file still exists

### Requirement: REQ-15 — The screen_flagged Notification

A firing whose applied level is `notify` or above, in `enforce` mode, SHALL
deliver the `[notify]` event `screen_flagged`, amending SPEC-0003 REQ "Operator
Notification". `screen_flagged` SHALL join the default `[notify] events`. It
SHALL NOT be sent in `shadow` mode or for `allow`.

Its JSON SHALL carry the standard fields and these, and nothing else:
`run_id` (absent when the firing was queued and has none yet), `source`,
`event_id`, `verdict`, `level`, `policy`, `score` (absent when none), `guard`,
`served_model`, and `hint`: `harness screen release <harness> <run_id>` for a
hold, and `harness runs <harness>` otherwise. `message` SHALL be rendered only
from those identifiers. `cause` SHALL be absent. No field SHALL carry any byte
of the screened text or of the event payload.

The notify cooldown for `screen_flagged` SHALL be keyed by harness, event,
policy and verdict, so a `warn` does not suppress a later `flag`, and a flood of
flagged deliveries for one harness and policy notifies once per cooldown. A
suppressed notification SHALL still leave its hold in place and its record
written.

#### Scenario: The payload names, never quotes

- **WHEN** a delivery whose body carries the canary `CANARY-7f3a` is held
- **THEN** the hook's stdin and environment carry the harness, run id, source,
  event id, policy, score, verdict and guard model, and neither contains
  `CANARY-7f3a`

#### Scenario: Ten flags, one page

- **GIVEN** a 15-minute notify cooldown
- **WHEN** ten deliveries are flagged for one harness and policy in five minutes
- **THEN** one `screen_flagged` is delivered, nine are counted `suppressed`,
  and ten held records exist

### Requirement: REQ-16 — Recording

Every run record produced by a screened harness's firing, whether it started,
queued, or was skipped for any reason, SHALL carry a `screen` object
(amending SPEC-0022 REQ-4 and SPEC-0014 REQ "Run Record Fields"):

| Field | Type | Meaning |
|---|---|---|
| `verdict` | `allow`, `warn`, `flag` or `error` | REQ-7 |
| `score` | number, 0 to 1, four decimals | The deciding result's score; absent when none |
| `policy` | string | The deciding result's `"<guard>.<policy>"` |
| `guard` | string | Its guard |
| `served_model` | string | The model its response named (REQ-6) |
| `mode` | `enforce` or `shadow` | |
| `level` | a REQ-12 level | Applied, or in shadow would-have-applied; absent for `allow` |
| `chunks` | integer | Chunks screened |
| `error` | a REQ-6 class | Absent when no result errored |
| `hash` | 64 lowercase hex | SHA-256 of the screened text, before redaction |

Records SHALL also carry, where they apply: `released_from` (integer, on a
released run), and on a `screen_hold` record `released_at`, `hold_dropped` and
`hold_expired` (RFC 3339), written as `updated` ledger lines (amending SPEC-0022
REQ-2). The skip reasons `screen_hold` and `screen_block` join SPEC-0022 REQ-5
and SPEC-0014 REQ "Run Record Fields". `harness runs` SHALL show them as the
reason of a `skipped` record.

The `triggers` reply (SPEC-0014 REQ "Trigger Visibility") SHALL carry, per
source, counters since daemon start of events `screened` and of harness firings
`held` and `blocked`.

**Privacy.** No ledger line, run record, log line, protocol frame, notification,
metric label or `state.json` field SHALL contain any byte of screened text or
of a guard's response body (amending SPEC-0022 REQ-17).
The kept `0600` event file is the only copy of held text. Logs name the harness,
run id, source, event id, policy, guard, verdict, score and error class.

#### Scenario: A canary appears nowhere it should not

- **GIVEN** a payload whose title, body and a channel `meta` value carry the
  canary `CANARY-7f3a`, screened with `on_flag = "notify"` and a flag verdict
- **WHEN** the test searches every ledger file, the daemon log, the per-run
  log, the notify payload, the `/metrics` body and every protocol frame
- **THEN** none contains `CANARY-7f3a`, and the held or run event file does

#### Scenario: The hash can be checked against the evidence

- **WHEN** a held record's `screen.hash` is compared with the SHA-256 of REQ-11's
  screened text derived from its kept event file
- **THEN** they are equal

### Requirement: REQ-17 — Metrics

On the existing `/metrics` listener (SPEC-0013), the daemon SHALL export:

```text
harness_screen_verdicts_total{entry,source,verdict,mode}   counter
harness_screen_duration_seconds{guard}                     histogram
harness_screen_errors_total{guard,class}                   counter
harness_screen_holds{harness}                              gauge
harness_screen_calls_total{guard,result}                   counter
harness_screen_tokens_total{guard,direction}               counter
```

* `entry` is `event` or `package`. `source` is the source reference for
  `event`, and empty for `package`. `verdict` is a REQ-7 verdict. `mode` is
  `enforce` or `shadow`. The verdict counter increments once per screened
  harness per event, and once per screened file.
* The duration histogram observes one gate call, from asking for a slot to a
  parsed response or an error.
* `class` is a REQ-6 class. `result` is `ok` or `error`.
* `harness_screen_holds` is the number of `screen_hold` records whose file
  exists and that are neither dropped nor expired.
* `direction` is `input` or `output`, from the response's `usage`. A response
  without `usage` adds nothing (SPEC-0013 REQ-6).

Every configured guard SHALL report every `class` and `result`, including
zeros. `harness` values SHALL be capped per SPEC-0013 REQ-5. No label SHALL
carry a policy name, model name, score, event id, run id, path, hash or any
screened text.

#### Scenario: Shadow counts are visible

- **WHEN** a shadow harness on `webhook.github` screens a flagged delivery
- **THEN** `harness_screen_verdicts_total{entry="event",source="webhook.github",verdict="flag",mode="shadow"}`
  increments

### Requirement: REQ-18 — Stable Screening

This requirement amends SPEC-0026 REQ-5. Before every install and upgrade,
after the heuristic scan, the CLI SHALL send every document that scan reads to
the daemon's `screen` op (REQ-19) with entry `package`, one request per
document:

* the manifest's string values, in manifest order, joined with newlines, as
  path `package.toml`;
* each bundled `.md` and `.txt` file, as its package-relative path.

Package screening SHALL use `[screen] policies` and `[screen] packages` only;
per-source and per-harness overrides do not apply, because a package has
neither. The CLI SHALL decide whether screening is configured from the global
`harness.toml` it already reads. A daemon whose loaded `[screen] policies` is
empty SHALL answer `classify` with `not_configured`, which the CLI SHALL treat
as the daemon being unreachable (below), naming `harness reload`.

| Model result for a document | Finding |
|---|---|
| `flag` | `high`, naming the policy, score, file and chunk |
| `warn` | `low`, naming the same |
| `error` | `packages_on_error` severity, naming the file and class |

* The model pass SHALL only add findings. A clean verdict SHALL NOT remove,
  lower or mark as reviewed any heuristic finding.
* With `[screen] policies` empty, the CLI SHALL make no request and SHALL print
  `model screening: not configured`. With `packages = "off"`, it SHALL print
  `model screening: off`.
* When the daemon cannot be reached, the CLI SHALL add one finding at
  `packages_on_error` severity, `model screening unavailable`.
* With `packages = "shadow"`, model findings SHALL be shown and recorded marked
  `shadow`, and SHALL NOT block or count as `high`.
* A model finding SHALL be recorded in the install record exactly as a
  heuristic one is, including an override with `--force-unsafe`. On upgrade, a
  model finding SHALL be marked new, as REQ-5 marks heuristic ones, when the
  installed pin's record holds no model finding with the same file, policy and
  severity.

A `high` model finding SHALL block exactly as a heuristic one does: only
`--force-unsafe` with the re-typed `<stable>/<package>` passes it, and `--yes`
alone never does.

#### Scenario: A clean model verdict leaves a heuristic block in place

- **WHEN** a `SKILL.md` has a heuristic `high` finding and the model returns
  `allow`
- **THEN** the install is still refused without `--force-unsafe`

#### Scenario: A flagged file blocks

- **WHEN** the model flags `skills/review/SKILL.md` chunk 0 at 0.91
- **THEN** install refuses, and the output names the file, chunk 0, the
  policy and 0.91

#### Scenario: The daemon is down

- **GIVEN** a configured policy and default `packages_on_error`
- **WHEN** the daemon is not running and install runs with `--yes`
- **THEN** the install is refused with the finding `model screening
  unavailable`

### Requirement: REQ-19 — The screen Control-Plane Op

The daemon SHALL add a `screen` control operation (SPEC-0002 REQ "Control
Operations"; ADR-0034), with an `action` field:

| Action | Caller | Does |
|---|---|---|
| `classify` | local socket only | Classifies one document for entry `package` (REQ-18) |
| `probe` | local socket only | One probe call to a named guard (REQ-20) |
| `explain` | `read` tier | Effective settings and their layers for a harness and optional source |
| `report` | `read` tier | Verdict counts and open holds (REQ-20) |
| `release` | `attach` tier | REQ-14 release; it delivers an event to an agent |
| `drop` | `control` tier | REQ-14 drop |

`classify` and `probe` SHALL be refused with `PERMISSION_DENIED` to any caller
that did not connect over the local socket, whatever its tier, because each
spends guard capacity and a remote caller could use `classify` as an oracle to
tune an injection (ADR-0008).

A `classify` request carries `entry` (only `package` is accepted in this
revision), `path` (a relative path, at most 1,024 bytes, no NUL, no `..`
segment) and `text` (UTF-8, at most 4 MiB; larger is refused with `too_large`).
The reply SHALL carry the aggregate verdict, the chunk count, the hash, and one
entry per chunk and policy with `policy`, `guard`, `served_model`, `chunk`,
`verdict`, `score` and `error`. It SHALL NOT echo `text` or any part of it.
The daemon SHALL use the text only to call the guard. It SHALL NOT read a file,
fetch a URL, or write configuration for this op (ADR-0040).

The protocol minor version SHALL be bumped for the op and for REQ-16's record
fields.

#### Scenario: A remote client cannot use the oracle

- **WHEN** an `attach`-tier TCP client calls `screen` with action `classify`
- **THEN** it receives `PERMISSION_DENIED`, and no guard call is made

#### Scenario: The reply carries no text

- **WHEN** a `SKILL.md` containing `CANARY-7f3a` is classified
- **THEN** the reply holds verdicts, scores and the hash, and does not contain
  `CANARY-7f3a`

### Requirement: REQ-20 — CLI And Doctor

The CLI SHALL provide, each accepting `--json`:

* `harness screen explain <harness> [source]`: for each source the harness
  binds (or the named one), every REQ-10 key's effective value and the layer it
  came from (`harness`, `source:<ref>`, `[screen]` or `builtin`), the effective
  policies with their guards, whether `skip_trusted_actors` applies, and
  `not screened` with the reason when the effective mode is `off` or the
  policies are empty. It reads what the daemon has loaded, not the file.
* `harness screen report [--since <duration>] [--source <ref>]`: from ledger
  records (default `--since 7d`), counts per source, policy, verdict, mode and
  level, with shadow would-have levels shown beside enforced ones, then every
  open hold with harness, run id, source, event id, policy, score, age and
  expiry. A coalesced record counts once.
* `harness screen release <harness> <run_id> [--wait] [--over-budget]`: REQ-14.
  With `--wait`, exit codes are those of `harness trigger --wait` (SPEC-0008
  REQ "Manual Trigger").
* `harness screen drop <harness> <run_id>`: REQ-14.

`harness doctor` SHALL show one row per guard, from one `probe` call per guard
carrying a fixed benign document through the guard's first policy: `pass` when
the guard answered 200, its `model` matched, and (for `yesno-logprob`) usable
logprobs were present or (for `labels`) the first line parsed; `fail` naming
the REQ-6 class otherwise. With the daemon unreachable each guard row SHALL be
`warn`, not probed. A probe withholds nothing and is not a gate call; it uses
the guard's slots and is counted in `harness_screen_calls_total`. Doctor SHALL
also show one screening row: `warn` when any
harness's effective policies are non-empty and `screen_flagged` would reach no
hook (no `[notify]` command, or the event not in `events`), when an `api_key`
crosses non-loopback `http`, or when holds are open, naming the oldest; `pass`
otherwise.

#### Scenario: doctor catches a proxy that strips logprobs

- **WHEN** a guard's `base_url` points at a gateway that drops `logprobs`
- **THEN** `harness doctor` shows that guard's row `fail` with `no_logprobs`,
  and exits non-zero

#### Scenario: Explaining an unscreened source

- **WHEN** `harness screen explain pr-review webhook.gitea` runs and the source
  sets `mode = "off"`
- **THEN** the output reads `not screened: mode off (source:webhook.gitea)`

### Requirement: REQ-21 — Amendments To Existing Specs

This spec SHALL be read as amending the following, and where they conflict this
spec governs:

* **SPEC-0002 REQ "Control Operations".** The op list gains `screen` (REQ-19).
* **SPEC-0003 REQ "Operator Notification".** The events gain `screen_flagged`,
  in the default `events`, with REQ-15's payload and cooldown key.
* **SPEC-0004 REQ "Project File Schema" and REQ "Project Control Operations".**
  A project file rejects `[guard.*]`, `[screen]` and a harness `screen` key, and
  `project_up` rejects a harness definition carrying `screen`.
* **SPEC-0008 REQ "Per-Run Logs".** `keep_runs` does not delete a
  `screen_hold` record's event file (REQ-14).
* **SPEC-0013.** The series of REQ-17 join the endpoint, under REQ-5's caps.
* **SPEC-0014 REQ "Channel Source Table" and REQ "Webhook Source Table".** The
  tables accept `screen` (REQ-10).
* **SPEC-0014 REQ "Firing".** A screened event is classified once before
  fan-out. Unscreened harnesses fire without waiting for it; screened ones fire
  once their own policies' results are in. "In config order" holds within each
  group and for the reported decisions.
* **SPEC-0014 REQ "Overlap Skip Coalescing".** `screen_hold` and `screen_block`
  skips never coalesce.
* **SPEC-0014 REQ "Run Record Fields".** Reasons gain `screen_hold` and
  `screen_block`; records gain `screen`, `released_from`, `released_at`,
  `hold_dropped` and `hold_expired` (REQ-16).
* **SPEC-0014 REQ "Event Delivery To The Run".** A `screen_hold` skip writes the
  event file though no process spawns, and it is pruned by REQ-14 rather than
  with the run.
* **SPEC-0014 REQ "Webhook Responses".** The response is sent after screening.
  A held or blocked harness reports `skipped` with its `run_id`, as any skip
  does, and the response SHALL carry no verdict, score or policy, so a sender
  cannot use it to probe the classifier.
* **SPEC-0014 REQ "Manual Trigger With Event".** `harness screen release` uses
  this path; its run also carries `released_from`. Neither path is screened.
* **SPEC-0014 REQ "Trigger Visibility".** The `triggers` reply gains REQ-16's
  per-source screening counters.
* **SPEC-0021 REQ-4.** Screening precedes admission and never holds its lock. A
  `screen_hold` or `screen_block` firing never reaches admission. A release is
  admitted as `harness trigger` is.
* **SPEC-0022 REQ-2.** `updated` lines also carry `released_at`,
  `hold_dropped` and `hold_expired` on a `decided` record.
* **SPEC-0022 REQ-4 and REQ-5.** REQ-16's fields and skip reasons.
* **SPEC-0022 REQ-17.** Screened text joins what the ledger never contains; the
  hash, score and verdict are allowed.
* **SPEC-0026 REQ-3.** `screen` joins the keys a manifest's `[harness]` table
  may not carry.
* **SPEC-0026 REQ-5.** The content scan gains REQ-18's model pass.

#### Scenario: An unscreened fleet is unchanged

- **WHEN** a daemon built with this spec runs a config with no `policies`
  anywhere
- **THEN** every webhook response, run record, event file and notification is
  identical to one from a daemon without this spec, apart from the protocol
  minor version

### Requirement: Error Handling Standards

All operations in this spec SHALL follow structured error handling:

- Errors SHALL be wrapped with context at each layer boundary, naming the
  guard, policy, harness, source, event id or run id where applicable, and the
  file and line for config errors.
- Sentinel errors SHALL be defined for: unknown guard, unknown policy, invalid
  policy reference, guard or policy schema violation, a key placed where it is
  not allowed, each REQ-6 class, hold not found, not a held record, held file
  missing, and screening not configured.
- An error MUST NOT be swallowed. A failed call is always an `error` result
  with a class; a failed screen is always a verdict that `on_error` governs, or
  an abandoned firing at shutdown; a failed hold write is always an ERROR log
  line.
- Logs SHALL be structured key-value and SHALL NOT include screened text,
  instruct text, a guard's response body, an `api_key` or any header value.
  They name the guard, policy, verdict, score, class, harness, source, event id
  and run id.

#### Scenario: A guard error is named, not quoted

- **WHEN** a guard answers `500` with a body echoing the request
- **THEN** the log line names the guard, policy, class `http_status` and status
  500, and contains no part of the body
