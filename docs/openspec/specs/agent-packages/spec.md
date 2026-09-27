---
status: draft
date: 2026-09-27
implements: [ADR-0040]
extends: [SPEC-0006]
---

# SPEC-0026: Agent Package Taps

## Overview

This spec adds a package manager for harness definitions:

* A **tap**: a git remote the operator explicitly trusts, declared in a new
  global-only `[tap.*]` table, holding any number of installable
  **packages** under a fixed repository layout.
* A **package manifest**: a narrowed, declarative-only subset of the harness
  schema (ADR-0006/SPEC-0006), plus package metadata and an itemized
  `[requests]` table. It can select an existing adapter and supply values; it
  can never define an adapter, a secret reference, or any table other than
  `[package]`, `[harness]` and `[requests]`.
* A **content scan and confirmation gate** that runs on every install and
  every upgrade — never only the first — showing the human the manifest, the
  itemized requests, and (on upgrade) a full diff before anything a harness
  will run can change.
* A **`source` field** on a harness table, resolved at config load against an
  immutable, content-addressed local store. The daemon never fetches,
  clones, scans, or confirms anything; all of that lives in the
  `harness agent` CLI tree.

See ADR-0040 for the decision and the options it rejected. This spec extends
SPEC-0006's skill-path merge order (REQ-10) and otherwise adds new
requirements rather than amending existing ones.

Requirements are numbered. Cite them as `SPEC-0026 REQ-n`.

## Requirements

### Requirement: REQ-1 — Tap Registration And Global-Only Trust

The global configuration SHALL accept any number of `[tap.<name>]` tables,
each with a required `remote` (a git URL) and an optional `public` boolean
(unset is treated as `true`, following SPEC-0007's `skill_repo` convention).
`<name>` SHALL match `^[a-z][a-z0-9-]*$`.

`[tap.*]` SHALL be rejected in a project `harness.toml` and on the
project-up wire, joining `[adapter.*]` and `[skill_repo.*]` on the
global-only list, because a cloned repository must not be able to expand
what is trusted on the machine that clones it.

A `[tap.*]` table SHALL be created, modified, or removed only by
`harness agent tap add|remove <name> ...`, which writes the global
`harness.toml` directly; no other command SHALL add or change a tap
declaration. `tap add` SHALL clone the remote into
`$XDG_STATE_HOME/harness/agents/taps/<name>/` before the table is written,
and SHALL fail without writing the table if the clone fails. `tap remove`
SHALL delete the table and print every currently installed harness whose
`source` names that tap, without uninstalling them.

`harness agent tap update [name]` SHALL be the only operation that fetches
an already-added tap's remote. It SHALL fast-forward the local clone and
SHALL fail, without modifying the clone, on a non-fast-forward state. No
other command, and no daemon activity of any kind, SHALL fetch a tap's
remote.

#### Scenario: A project file cannot add a tap

- **WHEN** a project `harness.toml` declares `[tap.evil] remote =
  "https://attacker.example/tap.git"`
- **THEN** `harness up` fails, naming `[tap.*]` as global-only, and no clone
  is attempted

#### Scenario: Adding a tap clones before trusting

- **WHEN** `harness agent tap add stump-wtf
  https://gitea.stump.rocks/stump.wtf/harness-tap.git` is run and the clone
  fails (network error, no such repository)
- **THEN** no `[tap.stump-wtf]` table is written to `harness.toml`

#### Scenario: Removing a tap reports its installed packages

- **WHEN** `harness agent tap remove stump-wtf` is run while
  `stump-wtf/pr-reviewer` is installed as harness `pr-reviewer`
- **THEN** the table is removed, the harness `pr-reviewer` is left exactly as
  it was, and the output names it as installed from the now-untrusted tap

#### Scenario: Nothing but tap update fetches

- **WHEN** `harness agent search`, `info`, `install`, or `upgrade` is run
  without an intervening `tap update`
- **THEN** none of them perform a network fetch; they operate on the tap's
  clone as it was left by the last `tap update`

### Requirement: REQ-2 — Tap Layout And Local Discovery

A tap's clone SHALL be expected to contain zero or more package directories
under `packages/<package-name>/`, each holding exactly one `package.toml`
manifest and, optionally, a `skills/` directory of skill directories
(ADR-0011 shape: `skills/<slug>/SKILL.md`) and a `prompts/` directory of
markdown files. `<package-name>` SHALL match the same pattern as a tap name.
A `packages/` directory with no valid subdirectories SHALL NOT be an error;
`harness agent search` on such a tap SHALL report zero packages.

`harness agent search [query] [--tap <name>]` SHALL list, across all
trusted taps or the named one, every package whose name or
`[package].description` matches `query` case-insensitively (or every
package, when `query` is omitted), reading only local clones.
`harness agent info <tap>/<package>` SHALL print the package's manifest,
its `[requests]` table, and the list of bundled files, without installing
it, reading only the local clone. Both commands SHALL fail, naming the tap,
when the named tap is not registered.

#### Scenario: Search across all trusted taps

- **WHEN** two taps are registered and `harness agent search reviewer` is run
- **THEN** every package from either tap whose name or description contains
  "reviewer" (case-insensitive) is listed, with its tap prefix

#### Scenario: Info reads without installing

- **WHEN** `harness agent info stump-wtf/pr-reviewer` is run and the package
  has never been installed
- **THEN** the manifest and bundled file list print, and no entry is created
  under the content-addressed store

#### Scenario: Unknown tap

- **WHEN** `harness agent info ghost/pr-reviewer` is run and no `[tap.ghost]`
  is registered
- **THEN** the command fails, naming `ghost` as not a registered tap

### Requirement: REQ-3 — Package Manifest Schema

A package manifest (`package.toml`) SHALL contain at most three top-level
tables: `[package]`, `[harness]`, and `[requests]`. Any other table SHALL
fail to load, naming the table and the manifest's path.

`[package]` SHALL require `name` (matching the package-name pattern) and
MAY carry `version`, `description`, `author`, and `homepage` as strings.

`[harness]` SHALL require `harness`, naming an adapter exactly as
SPEC-0006/ADR-0039 validate it (an unknown adapter SHALL fail exactly as it
does for a hand-written harness). It MAY carry only the per-harness value
keys ADR-0039 closes over (`args`, `argv`, `model`, `model_pin`,
`auto_accept`, `max_turns`, `quiet`, `system_prompt_file`, `mcp_config`,
`allowed_tools`, `skill_paths`, `use_default_skill_paths`, `mcp_bridge`,
`mcp_exclusive`, `mcp_policy`). It SHALL NOT carry `env_file`,
`secrets_env`, `workdir` as an absolute path outside the package directory,
`enabled`, `restart`, `restart_delay`, `operating_hours`, `schedule`, or
`triggers`; each SHALL fail to load, naming the key. Every string value
under `[harness]` SHALL be rejected if it contains the substring `${`,
because that is the secret-reference grammar ADR-0038 defines, and a
package manifest MUST NOT carry one. A relative path value (such as
`system_prompt_file`) SHALL resolve against the package's own directory
inside the content-addressed store, never against the installing harness's
`workdir`.

`[requests]` MAY carry `skill_paths` (boolean), `mcp_allow` (a list whose
values are `"read"` and/or `"write"`), and `network` (boolean). Any other
key under `[requests]` SHALL fail to load, naming the key.

#### Scenario: An unknown table is rejected

- **WHEN** a manifest declares `[adapter.claude-code]` alongside `[harness]`
- **THEN** the package fails to load, naming `adapter` and the manifest's
  path

#### Scenario: env_file is rejected

- **WHEN** a manifest's `[harness]` table declares `env_file =
  "~/.config/vault/secrets.env"`
- **THEN** the package fails to load, naming `env_file`

#### Scenario: A secret reference in any value is rejected

- **WHEN** a manifest declares `system_prompt_file = "${HOME}/prompt.md"`
- **THEN** the package fails to load, naming the key and stating that `${`
  is not permitted in a package manifest

#### Scenario: An unknown request key

- **WHEN** `[requests]` declares `filesystem = true`
- **THEN** the package fails to load, naming `filesystem`

#### Scenario: Adapter selection is validated like any harness

- **WHEN** a manifest declares `harness = "nonexistent"`
- **THEN** the package fails to load with the same unknown-adapter error a
  hand-written harness table produces

### Requirement: REQ-4 — Capability Requests

The confirmation shown by `install` and `upgrade` (REQ-6, REQ-8) SHALL
render every entry present in `[requests]` as an itemized, human-readable
line, and SHALL state explicitly when a request key is absent (for example,
"does not declare needing network access" when `network` is unset or
`false`). A request of `mcp_allow` containing `"write"` SHALL be rendered
with a distinct warning stating that the installed harness would be able to
start, stop, or restart its siblings (ADR-0010), and installing or
upgrading such a package SHALL require the operator to retype
`<tap>/<package>` at the confirmation prompt, independent of any content
scan finding and independent of `--yes`.

#### Scenario: A read-only package needs no extra confirmation

- **WHEN** a package requests `mcp_allow = ["read"]` and no content scan
  finding is `high`
- **THEN** `--yes` completes the install with the ordinary confirmation
  summary, no retyped name required

#### Scenario: A write request demands typed confirmation

- **WHEN** a package requests `mcp_allow = ["read", "write"]`
- **THEN** install refuses to proceed under `--yes` alone, and an
  interactive install only proceeds once the operator retypes
  `<tap>/<package>` exactly

### Requirement: REQ-5 — Content Scan And Severity

Before every install and every upgrade, the CLI SHALL run a content scan
over the manifest's string values and every bundled file with a `.md` or
`.txt` extension. The scan SHALL classify each finding as `high` or `low`
severity and SHALL be documented as a heuristic tripwire, not a
certification: the CLI output and `harness agent info` SHALL both state
that a clean scan is not a guarantee of safety.

A `high`-severity finding SHALL block `install` and `upgrade` unless
`--force-unsafe` is given together with a re-typed `<tap>/<package>`; when
given, the install record SHALL retain the finding and the fact that it was
overridden. `--yes` alone SHALL NOT suppress a `high`-severity block. A
`low`-severity finding SHALL be shown in the confirmation output and SHALL
NOT block.

On an upgrade, the scan SHALL run against the new pin's content, and any
finding not present against the currently installed pin's content SHALL be
marked as new in the diff output (REQ-8), so a previously accepted package
cannot silently pick up an injected instruction on the next version without
it being called out specifically.

#### Scenario: A high finding blocks by default

- **WHEN** a bundled `SKILL.md` contains a phrase matching a high-severity
  pattern (for example, an instruction to disregard the system prompt)
- **THEN** `install` refuses without `--force-unsafe`, and the finding's
  file, line, and matched pattern name are shown

#### Scenario: --yes never bypasses a high block

- **WHEN** the same install is retried with `--yes` and no
  `--force-unsafe`
- **THEN** the install still refuses

#### Scenario: force-unsafe requires the typed name

- **WHEN** `--force-unsafe` is given without an interactive retype of
  `<tap>/<package>` on a non-interactive terminal
- **THEN** the install refuses, stating that an unattended session cannot
  override a high-severity finding

#### Scenario: A low finding does not block

- **WHEN** a bundled file contains only a low-severity finding (for example,
  an embedded link to a domain other than the package's declared
  `homepage`)
- **THEN** `install` completes under `--yes`, with the finding shown in the
  output

#### Scenario: A new finding on upgrade is called out

- **WHEN** the currently installed pin has zero findings and the candidate
  pin has one `high` finding in a file unchanged by line count but altered
  in content
- **THEN** the upgrade's diff output marks that finding as new relative to
  the installed pin

### Requirement: REQ-6 — Install: Resolution And Pinning

`harness agent install <tap>/<package>[@<version>] [--as <name>]` SHALL
resolve `<version>` (default: the tap's current default branch tip) to an
exact, full 40-character commit SHA in the tap's local clone. When
`<version>` is already a full 40-character SHA present in the local
content-addressed store, resolution SHALL use that local copy directly and
SHALL NOT consult the tap's clone.

After the content scan (REQ-5) and confirmation (REQ-4), a successful
install SHALL copy the package directory's contents at that commit into
`$XDG_STATE_HOME/harness/agents/installed/<tap>/<package>/<sha>/`,
immutably; an existing directory at that exact path SHALL be treated as
already-materialized and SHALL NOT be rewritten. Install SHALL then write
or update a `[harness.<name>]` table in the target `harness.toml`
(`<name>` defaults to `<package>`; `--as` overrides it) with `source =
"<tap>/<package>@<sha>"`, preserving every other key already on that table.
Installing into a harness name that already has a `source` from a
*different* tap or package SHALL require an explicit `--replace` flag;
without it, the command SHALL fail, naming the existing source.

#### Scenario: A branch resolves to a pinned SHA

- **WHEN** `harness agent install stump-wtf/pr-reviewer` is run with no
  `@version` and the tap's default branch tip is commit `abc123...`
- **THEN** the written `source` reads
  `stump-wtf/pr-reviewer@abc123...` (the full SHA), never the branch name

#### Scenario: Repeated install is idempotent

- **WHEN** `install` is run twice in a row with no intervening `tap update`
- **THEN** both resolve to the same commit SHA and the second run reuses
  the existing content-addressed directory without rewriting it

#### Scenario: Installing twice under different names

- **WHEN** `harness agent install stump-wtf/pr-reviewer --as
  pr-reviewer-strict` is run after `pr-reviewer` is already installed
- **THEN** a second harness table, `pr-reviewer-strict`, is created with its
  own `source`, and the first is untouched

#### Scenario: A local SHA install needs no tap clone

- **WHEN** `harness agent install stump-wtf/pr-reviewer@<sha>` is run for a
  `<sha>` already present in the content-addressed store, and the tap's
  clone is unreachable
- **THEN** the install still succeeds, reading only the local store

#### Scenario: Overwriting a different source requires --replace

- **WHEN** `harness.pr-reviewer` already has `source =
  "other-tap/other-pkg@..."` and `harness agent install
  stump-wtf/pr-reviewer --as pr-reviewer` is run without `--replace`
- **THEN** the command fails, naming the existing source

### Requirement: REQ-7 — The Source Field And Config-Load Resolution

The harness schema SHALL accept an optional `source` key, a string of the
form `<tap>/<package>@<sha>` where `<sha>` is a full 40-character commit
SHA. `source` SHALL be legal on a `[harness.*]` table in the global
configuration file and in a project `harness.toml`, unlike `[tap.*]`.

At config load, a harness table declaring `source` SHALL be resolved by
reading `package.toml` from
`$XDG_STATE_HOME/harness/agents/installed/<tap>/<package>/<sha>/` on local
disk only — the daemon SHALL NOT perform a network request or a git
operation of any kind to resolve `source`. The package's `[harness]` values
SHALL be applied first, and any key also present directly on the harness
table SHALL override the package's value for that key, following the
precedence rule ADR-0011 already applies to `skill_paths`. The package's
`skill_paths` (if `[requests].skill_paths` was `true`) SHALL be added at the
lowest merge tier (REQ-10).

When the referenced pin is absent from local disk, config load SHALL fail
with a located error naming the harness, the full `source` value, and an
instruction to run `harness agent install`. This failure SHALL keep the
daemon on its last-good configuration (ADR-0006), exactly as an unknown
adapter does.

#### Scenario: A package-sourced harness resolves identically to a hand-written one

- **WHEN** a harness declares `source = "stump-wtf/pr-reviewer@abc123..."`
  with no other keys, and the pinned manifest declares
  `harness = "claude-code"` and `auto_accept = true`
- **THEN** the loaded, validated configuration for that harness is
  identical to a hand-written table declaring the same two keys directly

#### Scenario: A local override wins

- **WHEN** the same harness table also declares `model = "opus"` directly,
  and the pinned manifest does not set `model`
- **THEN** the effective configuration carries `model = "opus"`

#### Scenario: A missing pin fails the load, not the network

- **WHEN** a harness declares a `source` whose pin directory does not exist
  on disk
- **THEN** config load fails naming the harness and the full source string,
  and no network request or git operation is attempted

#### Scenario: A project file may reference an installed package

- **WHEN** a project `harness.toml` declares `[harness.agent] source =
  "stump-wtf/pr-reviewer@abc123..."`, and that pin is already installed on
  the machine running `harness up`
- **THEN** the project registers and starts the harness normally, with no
  fetch of any kind

#### Scenario: A project file cannot install what it references

- **WHEN** the same project file is used on a machine where that pin was
  never installed
- **THEN** `harness up` fails naming the missing pin, and nothing is
  fetched, scanned, or installed on its behalf

### Requirement: REQ-8 — Upgrade

`harness agent upgrade <tap>/<package>|--all [@<version>]` SHALL, for each
named harness whose `source` names that `<tap>/<package>`, resolve a new
pin exactly as REQ-6 describes for install (using the tap's
already-fetched local clone; `upgrade` SHALL NOT fetch). It SHALL then
produce a diff against the currently installed pin: every changed manifest
value, and, for each bundled file under 64 KiB, a unified text diff; a
larger or non-text file SHALL be reported as changed with its byte count,
not diffed inline. The diff output SHALL incorporate REQ-5's scan-on-new
findings.

Upgrade SHALL be refused, exactly as install is, on a `high`-severity
finding without `--force-unsafe`, and on an `mcp_allow` request including
`"write"` without the typed confirmation, whether or not that request was
already present and confirmed in the currently installed pin. On
confirmation, upgrade SHALL update only the `source` line's `@<sha>` on the
affected harness table; every other key on that table SHALL be left
untouched. The prior pin's content-addressed directory SHALL NOT be
deleted by upgrade.

#### Scenario: A trivial upgrade still requires confirmation

- **WHEN** `upgrade` resolves a new pin that changes only
  `[package].version` and no scanned content
- **THEN** the diff is shown and confirmation is still required (or `--yes`
  accepted, since no `high` finding exists)

#### Scenario: Upgrade needs a prior tap update

- **WHEN** `upgrade` is run for a version newer than what the tap's local
  clone holds, with no intervening `tap update`
- **THEN** the command fails, naming the requested version as not present
  in the local clone and recommending `harness agent tap update`

#### Scenario: The prior pin survives an upgrade

- **WHEN** a harness is upgraded from `@sha1` to `@sha2`
- **THEN** `$XDG_STATE_HOME/harness/agents/installed/<tap>/<package>/sha1/`
  still exists on disk after the upgrade completes

#### Scenario: Rollback is a re-install of a retained pin

- **WHEN** `harness agent install <tap>/<package>@sha1 --as <name>
  --replace` is run after an upgrade to `sha2`, and `sha1`'s directory is
  still present
- **THEN** the harness's `source` is rewritten back to `@sha1` with no
  network access

### Requirement: REQ-9 — Uninstall And Prune

`harness agent uninstall <name>` SHALL, after confirmation, remove the
entire `[harness.<name>]` table from the file that declares it (global or,
when named there, project) when that table's `source` is set; it SHALL
refuse, naming the harness, when the table has no `source` (uninstall
never touches a hand-written harness).

`harness agent prune` SHALL remove every entry under
`$XDG_STATE_HOME/harness/agents/installed/` that no `source` in the
**global** `harness.toml` references. It SHALL NOT scan project files (they
are ephemeral and not always present) and SHALL state this scope
limitation in its own output. A pin that a project file references but that
`prune` removed SHALL surface only as REQ-7's ordinary missing-pin load
error the next time that project is brought up, naming the pin and
recommending reinstall — the same behavior as any other missing pin.

#### Scenario: Uninstall removes the whole table

- **WHEN** `harness agent uninstall pr-reviewer` is run for a harness whose
  table is `source = "stump-wtf/pr-reviewer@abc123..." workdir =
  "~/src/reduit"`
- **THEN** the entire `[harness.pr-reviewer]` table is removed, not just the
  `source` line

#### Scenario: Uninstall refuses a hand-written harness

- **WHEN** `harness agent uninstall claude-src` is run for a harness with
  no `source` key
- **THEN** the command fails, stating that the harness was not installed
  from a package

#### Scenario: Prune removes only globally unreferenced pins

- **WHEN** a pin is referenced only by a project `harness.toml` that is not
  currently `up`, and by no global harness
- **THEN** `harness agent prune` removes it, and the next `harness up` for
  that project fails naming the missing pin rather than silently refetching
  it

### Requirement: REQ-10 — Skill Path Precedence Amendment

This requirement amends SPEC-0006 REQ "Ordered Merge and Shadowing". The
merge order becomes, lowest precedence first:

```
package bundle (installed source's skills/, when [requests].skill_paths
  was true) → adapter defaults → global skill_paths → project skill_paths →
  project-local dirs (highest)
```

A package's bundled skills SHALL contribute at the new lowest tier only
when the harness's effective configuration carries a `source`, and only
the skills bundled at that exact pinned commit. A name collision between
the package tier and any higher tier SHALL resolve to the higher tier, with
the package's copy recorded as shadowed exactly as SPEC-0006 already
requires for any other collision.

#### Scenario: A human's global skill wins over a package's

- **WHEN** a package bundles a skill named `playbook` and the operator's
  global `skill_paths` also supplies a skill named `playbook`
- **THEN** the global copy is projected, and the package's copy is recorded
  as shadowed

#### Scenario: An uninstalled package contributes nothing

- **WHEN** a harness has no `source`
- **THEN** the package tier contributes no roots for that harness, and the
  merge behaves exactly as SPEC-0006 already defines

### Requirement: REQ-11 — No Fleet-Wide Auto-Registration

Installing or upgrading a package SHALL NOT modify any `[mcp.*]` table,
including `[mcp.prompts]`. When a package bundles a `prompts/` directory,
`install` and `upgrade` output SHALL name that directory and state that it
is not registered automatically. No command defined by this spec SHALL
write to `[mcp.*]`, `[job.*]`, `[server]`, `[profile.*]`, `[adapter.*]`, or
`[skill_repo.*]`.

#### Scenario: A bundled prompts directory is not wired in

- **WHEN** a package bundling `prompts/release-notes.md` is installed
- **THEN** `[mcp.prompts]` is unchanged, and the install output names the
  bundled directory and says it must be added by hand

### Requirement: REQ-12 — CLI Visibility

`harness agent list` SHALL show every harness whose table carries a
`source`, its tap, package, pinned SHA (short form for display, full form
with `--json`), and whether the tap's local clone (as of its last
`tap update`) has a newer default-branch commit than the installed pin
(informational only; it SHALL NOT trigger a fetch or an upgrade).
`harness describe` on a package-sourced harness SHALL show its `source`
value and which of its effective keys came from the package versus a local
override. `harness doctor` SHALL add a warn row for any harness whose
`source` pin is missing from local disk, distinct from an ordinary load
error, when config as a whole otherwise loaded (i.e., when the missing pin
was caught before the rest of the file, `doctor` reports it as a load
failure instead, per existing behavior).

#### Scenario: list shows staleness without fetching

- **WHEN** a tap's local clone (from its last `tap update`) is three commits
  ahead of an installed pin
- **THEN** `harness agent list` marks that package as having a newer
  version available, without performing any network access

#### Scenario: describe attributes each field

- **WHEN** `harness describe pr-reviewer` is run for a package-sourced
  harness with one local override
- **THEN** the output marks the overridden key as `local` and every other
  key as coming from the package's pin

### Requirement: Error Handling Standards

All error-producing operations in this spec SHALL follow structured error
handling:

- Errors SHALL be wrapped with context at each layer boundary, naming the
  tap, the package, and the harness where applicable.
- Sentinel errors SHALL be defined for the failure modes callers
  distinguish: unknown tap, unknown package, manifest schema violation,
  blocked high-severity finding, missing local pin, and diverged tap clone.
- An error MUST NOT be swallowed silently. A blocked install or upgrade is
  always a non-zero exit with a message naming the reason, never a partial,
  silent success.
- Logging SHALL be structured key-value, and SHALL never include the
  content of a scanned file or a rendered confirmation prompt, only its
  path, finding category, and severity.

#### Scenario: A blocked install exits non-zero with a named reason

- **WHEN** an install is blocked by a `high`-severity finding
- **THEN** the process exits non-zero, and the error names the file and the
  finding category without echoing the file's content into an unrelated log
  stream
