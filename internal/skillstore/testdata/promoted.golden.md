---
name: pin-go-version-in-ci
description: Pin the Go toolchain in CI before bumping go.mod so linters and builds agree on the toolchain.
level: atomic
status: promoted
task_family: configuration
action: pin
target: ci
tags:
  - ci
  - go
purpose_key: configuration/pin/ci/go-mod
symptoms:
  - "go: cannot find main module"
  - 'workflow: The version of Go used to build is not supported'
applies_to:
  - ".gitea/workflows/*"
  - "go.mod"
provenance:
  projects: 3
  first_seen: "2026-09-01T10:00:00Z"
  last_seen: "2026-09-20T18:30:00Z"
  trajectories:
    - reduit/agent/a1b2c3
    - spotter/agent/d4e5f6
    - harness/agent/00aa11
---

## When to use

A CI run fails because the toolchain version disagrees between jobs.

## Steps

1. Set `go-version` in every workflow that builds or lints.
2. Bump `go.mod` in the same change.

## Invariants

- The toolchain and `go.mod` never disagree within one change.

## Failure modes

- Pinning only one workflow leaves the others red.

## Do not

- Do not bump `go.mod` without pinning the workflow toolchain.

## Evidence

- reduit#191, merged 2026-09-19.
