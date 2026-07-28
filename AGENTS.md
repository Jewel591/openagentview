# openagentview development guide

## Product invariants

- Agent-owned session stores are read-only.
- Mutating agent state must go through that agent's supported CLI or protocol.
- UI code depends on normalized `agent.Session` values, not provider schemas.
- The product exposes preset layouts, not user-configurable workflow state.
- Status is derived from runtime activity; Projects are derived from workspace paths.
- An agent capability the board offers but the product lacks is refused, never
  emulated with a destructive substitute.
- One agent being absent or broken never blanks the board for the others.
- Quick Look reads live state from the agent's log, not from the board's cached
  session list: discovery is paused while the overlay is open.
- Repeated reads of one session log cost only what the agent appended since the
  last read. Session logs reach hundreds of megabytes and are polled once a
  second, so re-reading one from the top is felt as UI stutter.
- A task composed on the board starts through the agent's own CLI in a tmux
  session the product creates; the agent's store is never written to make a
  session appear.

## Commands

Run these before handing off a change:

```sh
gofmt -w cmd internal
go test ./...
go vet ./...
go build -o bin/openagentview ./cmd/openagentview
```

Build to `bin/openagentview`, the path README tells people to run: a build left
anywhere else leaves the binary they launch untouched, and the change looks
like it never happened. A board already on screen is an older binary still
running — quit it and start it again to see the change.

Tests should cover state transitions and adapter parsing behavior. Do not add
snapshot tests for terminal styling.
