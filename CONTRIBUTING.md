# Contributing to OpenAgentView

Thanks for your interest in contributing! This document explains how to get a
change from your editor into the project.

## Getting started

You need Go 1.26 or newer. Clone the repository and make sure everything
passes before you start:

```sh
go test ./...
go vet ./...
go build ./cmd/openagentview
```

To exercise the TUI against real sessions, install any of the supported agent
CLIs (`codex`, `claude`, `grok`) and run the binary. `--codex-home`,
`--claude-home`, and `--grok-home` point discovery at fixture directories,
which is useful for testing without touching your real agent state.

## Before opening a pull request

Run the full check sequence:

```sh
gofmt -w cmd internal
go test ./...
go vet ./...
go build ./cmd/openagentview
```

## Product invariants

These are the rules the codebase is built around. A pull request that breaks
one of them will be asked to change course, so it is worth reading them first
— they also live in [AGENTS.md](AGENTS.md), which coding agents pick up
automatically:

- Agent-owned session stores are read-only. Mutating agent state must go
  through that agent's supported CLI or protocol.
- UI code depends on normalized `agent.Session` values, not provider schemas.
- The product exposes preset layouts, not user-configurable workflow state.
- An agent capability the board offers but the product lacks is refused, never
  emulated with a destructive substitute.
- One agent being absent or broken never blanks the board for the others.
- Repeated reads of one session log must cost only what the agent appended
  since the last read.

## Adding support for a new agent

The `agent.Adapter` interface is the seam. A new adapter maps its product's
session model into the normalized `agent.Session` type and plugs into
`agent.Multi`; the board needs no changes. Look at `internal/codex`,
`internal/claude`, and `internal/grok` for three complete examples.

## Testing philosophy

- Test state transitions and adapter parsing behavior.
- Do not add snapshot tests for terminal styling; visual details are free to
  change.
- A real bug fix should come with a regression test at the lowest level that
  can catch it.

## Reporting issues

Bug reports are most useful with: your OS, the agent CLIs installed and their
versions, and what the board showed versus what you expected. If a session is
classified wrongly, the tail of its log file (with anything private removed)
is usually the fastest path to a fix.
