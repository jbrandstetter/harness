---
title: "Charm ecosystem map"
sidebar_position: 99
---

# Charm ecosystem map

What Harness uses, defers, or only references from the Charm ecosystem: a full
pass over [charm.land](https://charm.land), the
[charmbracelet org](https://github.com/orgs/charmbracelet/repositories) (55+
repos) and the [`x/*`](https://github.com/charmbracelet/x) experimental packages,
mapped onto this project. Charm isn't just Bubble Tea — it's a whole stack that
covers *every* layer Harness needs, from PTY up to SSH directory. ADR-0001 (the
language and ecosystem decision) and ADR-0003 (terminal multiplexing) rest on
this inventory.

Tiers:

* **Core** — used in v1.
* **Likely** — expected in v1 or v1.x.
* **Later** — optional, when a feature needs it.
* **Reference** — not a dependency; read for inspiration or as a model.

## Layer 1 — TUI runtime and rendering (the client)

| Lib | Role here | Tier |
|-----|-----------|------|
| [**bubbletea**](https://github.com/charmbracelet/bubbletea) | The TUI runtime (Model/Update/View) for the whole client. | Core |
| [**bubbles**](https://github.com/charmbracelet/bubbles) | list, viewport, table, textinput, spinner, help, key registry — the dashboard's parts. | Core |
| [**lipgloss**](https://github.com/charmbracelet/lipgloss) | All styling/layout: panes, borders, the status ribbon, adaptive color. | Core |
| [**colorprofile**](https://github.com/charmbracelet/colorprofile) | Detect terminal color depth and degrade the palette gracefully (TrueColor→256→16→no-color). Keeps the TUI themes (SPEC-0001) robust across terminals **and over SSH**. | Core |
| [**ultraviolet**](https://github.com/charmbracelet/ultraviolet) | "TUI primitives" — the newer cell/drawable rendering layer that `x/vt` targets (`uv.Drawable`). This is *how* the embedded terminal pane composites into the UI (ADR-0003). | Core |
| [**harmonica**](https://github.com/charmbracelet/harmonica) | Physics-based spring animation → the "hop" signature moment (SPEC-0001): a smooth slide/flash when switching harnesses. | Likely |
| [**glamour**](https://github.com/charmbracelet/glamour) | Render markdown inside the TUI (help, a harness's README, agent markdown output). | Likely |
| [bubblezone](https://github.com/lrstanley/bubblezone) *(community, not Charm)* | Mouse hit-zones if mouse support is added. Not first-party; optional. | Later |

## Layer 2 — Terminal emulation and PTY (the daemon core — ADR-0003)

This is the layer that makes "bake in the multiplexer" real. It's almost entirely
in [`x/*`](https://github.com/charmbracelet/x).

| Pkg | Role here | Tier |
|-----|-----------|------|
| [**x/vt**](https://pkg.go.dev/github.com/charmbracelet/x/vt) | The virtual terminal emulator: parse harness output → screen, `InputPipe`, `Draw`. Harness caps its scrollback and keeps history in its own byte ring (see the memory note below). **The heart of ADR-0003.** | Core |
| [**x/xpty**](https://github.com/charmbracelet/x/tree/main/xpty) | Cross-platform PTY allocation for the supervised process. | Core |
| **x/conpty** | Windows ConPTY backing for `xpty` — the path to eventual Windows support. | Later |
| **x/ansi** | ANSI/escape-sequence encode and parse — under `vt` and for anything Harness emits. | Core |
| **x/cellbuf** | Cell-based screen buffer — backs diff-based repaints of the terminal pane. | Core |
| **x/input** | Keyboard/mouse event parsing — decoding client input on the attach path. | Core |
| **x/term** / **x/termios** | Raw mode, termios, size — low-level terminal control. | Core |
| **x/wcwidth** | East-Asian/wide and combining char widths — **exactly the correctness risk ADR-0003 flagged**, so agent output with emoji or CJK renders right. | Core |
| **x/vttest** / **x/vt/vttest** | VT conformance test helpers → the emulator golden-test harness (de-risks ADR-0003). | Likely |
| **x/vcr** | Record/replay of terminal interactions (recorder/matcher/marshaler/hooks) → capture real Claude Code / Crush sessions as fixtures for emulator tests, and a seed for future session recording (ADR-0007). | Likely |
| [**sequin**](https://github.com/charmbracelet/sequin) | Human-readable ANSI decoder — a debugging tool while building and hardening the emulator. | Reference |
| **x/editor** | Launch `$EDITOR` cleanly — backs the "edit raw TOML" escape hatch (SPEC-0001). | Likely |
| **x/mosaic** | Render images in the terminal — niche; only if the TUI ever shows images. | Later |

### x/vt memory: the foot-gun

An `x/vt` emulator is much more expensive than its screen suggests. These
numbers are for the version Harness pins:

* **Parser buffer.** Every emulator allocates a fixed 4 MiB before it draws
  anything
  ([charmbracelet/x#973](https://github.com/charmbracelet/x/issues/973)).
* **Scrollback.** Each of its two screens keeps a scrollback of 10,000 rows by
  default. A cell is 112 bytes, so a full main-screen scrollback is about
  85 MiB at 80 columns and about 210 MiB at 200.
* **The reply pump.** The emulator answers terminal queries by writing into an
  unbuffered pipe, so something must drain `Read`. A goroutine parked in that
  `Read` keeps the whole emulator reachable until the pipe is closed.
  `Emulator.Close` races the parked `Read`, so Harness releases an emulator by
  closing its `InputPipe()` instead.

Harness follows three rules for the emulators it creates: the log sanitizer
built for each spawn, the attach mux for each harness, and the TUI's views.

* Each one calls `SetScrollbackSize(1)`, because nothing reads emulator
  scrollback. The alternate screen's scrollback has no public knob and stays
  uncapped.
* Each one's reply pump is released when its owner is done with it.
* No emulator is used to store history. Attach scrollback comes from a raw-byte
  ring, and durable history comes from the per-harness log.

Before these rules, every spawn left its sanitizer emulator behind: about
98 MiB after a 5 MiB run at 80 columns
([stump-wtf/harness#18](https://github.com/stump-wtf/harness/issues/18)).

## Layer 3 — CLI surface (the `harness` command — ADR-0001)

| Lib | Role here | Tier |
|-----|-----------|------|
| [**fang**](https://github.com/charmbracelet/fang) | Cobra starter kit: styled help/usage, styled errors, auto `--version`, `man` generation, shell completions. Wrapping the root command gives a polished `harness` CLI for free. | Core |
| [**log**](https://github.com/charmbracelet/log) | Structured, colorful daemon logging. | Core |
| [**huh**](https://github.com/charmbracelet/huh) | Forms for create/edit-harness (writes back to TOML, ADR-0006); can also drive an interactive `harness init`. | Core |
| [**gum**](https://github.com/charmbracelet/gum) | Not linked in; inspiration for palette and confirm affordances. Users can script around `harness` with it. | Reference |

## Layer 4 — Server, SSH and remote (ADR-0004, ADR-0008)

| Lib | Role here | Tier |
|-----|-----------|------|
| [**wish**](https://github.com/charmbracelet/wish) | SSH server that serves the Bubble Tea TUI per session (PTY and resize wired). The entire remote-attach story. | Core (remote) |
| [**wishlist**](https://github.com/charmbracelet/wishlist) | "The SSH directory" — a menu of SSH endpoints. **Directly enables the multi-host story:** one launcher listing every machine's `harness daemon`, hopping across boxes. Answers "hop into harnesses *anywhere*." | Likely |
| [**keygen**](https://github.com/charmbracelet/keygen) | Generate the daemon's Ed25519 **host key** on first run (ADR-0008 host-key verification). | Likely |
| [**melt**](https://github.com/charmbracelet/melt) | Back up/restore that host key as seed words — so a re-provisioned host keeps its identity and clients don't trip host-key-changed warnings. | Later |
| [**promwish**](https://github.com/charmbracelet/promwish) | Prometheus middleware for Wish → daemon metrics (attach counts, sessions) for the operator's monitoring. | Later |
| [**soft-serve**](https://github.com/charmbracelet/soft-serve) | Not a dependency — the reference implementation of a real Wish-based multi-user SSH TUI daemon. Read it for how it does auth, access levels, and TUI-over-SSH at scale. | Reference |
| [**confettysh**](https://github.com/charmbracelet/confettysh) | A tiny Wish example; useful for a first spike. | Reference |

## Layer 5 — AI-harness awareness (open question)

Harness is agent-agnostic. If it leans into agent-*aware* affordances, Charm
already ships the Go AI plumbing (it's what Crush is built on):

| Lib | Role here | Tier |
|-----|-----------|------|
| [**fantasy**](https://github.com/charmbracelet/fantasy) | "Build AI agents with Go, multiple providers, one API." If Harness ever grows *its own* agentic features (e.g. a supervisor that summarizes what an idle agent is waiting on), this is the substrate. | Later (if Harness becomes agent-aware) |
| [**catwalk**](https://github.com/charmbracelet/catwalk) | Catalog of LLM providers/models (powers Crush). Useful for detecting *which* model/provider a harness is running, to badge it in the UI. | Later (if Harness becomes agent-aware) |
| [**crush**](https://github.com/charmbracelet/crush) | Not a dependency — a flagship harness and a north star for TUI-agent UX. The emulator golden tests (x/vcr) should include real Crush sessions. | Reference |

## Layer 6 — Docs, demos, distribution

| Tool | Role here | Tier |
|------|-----------|------|
| [**vhs**](https://github.com/charmbracelet/vhs) | Scripted terminal recordings → the README demo tape (the "hop" in motion) and regression GIFs. SPEC-0001 asks for one. | Likely |
| [**freeze**](https://github.com/charmbracelet/freeze) | Static images of terminal output/code → polished screenshots for docs. | Later |
| [**vhs-action**](https://github.com/charmbracelet/vhs-action) | Keep demo GIFs fresh in CI. | Later |
| homebrew-tap / scoop-bucket / winget / nur | Distribution patterns to mirror for shipping the `harness` binary. | Reference |

## Not relevant

`pop` (email), `glow` (standalone markdown reader — the TUI embeds glamour
instead), `skate` (KV store — Harness state is a small file, ADR-0007),
`git-lfs-transfer`, `hotdiva2000`, `runway` (3D models), `tree-sitter-vhs`,
`openai-go` (fantasy/catwalk cover the need), `mods` (standalone LLM CLI).

## The takeaway for the architecture

The ecosystem *confirms* the ADR-0003 bet: Charm has a complete, coherent stack
from **PTY (`x/xpty`/`conpty`) → emulation (`x/vt`, `x/ansi`, `x/cellbuf`,
`x/wcwidth`) → rendering (`ultraviolet`, `lipgloss`) → TUI (`bubbletea`,
`bubbles`) → CLI (`fang`) → SSH server (`wish`) → SSH directory (`wishlist`) →
key lifecycle (`keygen`, `melt`) → testing (`x/vttest`, `x/vcr`, `exp/teatest`) →
docs (`vhs`, `freeze`)**. Every box in the ADR-0002 architecture maps to a
maintained Charm package. That's the strongest argument for ADR-0001, and it
means "bake it in" is assembling first-party parts, not inventing a terminal
stack from scratch.
