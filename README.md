# OpenAgentView

[![CI](https://github.com/Jewel591/openagentview/actions/workflows/ci.yml/badge.svg)](https://github.com/Jewel591/openagentview/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go)](go.mod)

**OpenAgentView** (`openagentview`) is like Claude Code's agent view, but for
every coding agent — and handier. It is a local-first terminal control plane
that discovers the saved sessions of every agent CLI on your machine and
presents them along two independent axes: grouped by runtime status or by
project, drawn as a kanban board or as one list — any grouping in either
layout.

Codex CLI, Claude Code and Grok CLI are all supported, side by side on one
board: see who needs you, answer a blocked prompt without leaving the board,
resume any session, or start a new one.

## Current features

- Discovers Codex sessions from the newest `~/.codex/state_*.sqlite`
- Discovers Claude Code sessions from `~/.claude/projects`, and takes their
  status from the registry Claude Code keeps of its running sessions
- Discovers Grok sessions from `~/.grok/sessions`
- Identifies open sessions of any agent on macOS and Linux
- Classifies sessions as Needs You, Running, or Idle, led by the ones waiting
  on you; an Error group appears only while a session's log cannot be read,
  and archived sessions stay off the board
- Groups sessions by status or by project, with no user configuration
- Draws either grouping as a kanban board or as a list that puts every
  session on one line
- Shows the last 24 hours by default while search covers full history
- Searches agents, titles, prompts, workspaces, branches, sources, and
  sub-agent names
- Shows session metadata in a detail panel
- Quick Look follows a live session, including the tool calls and reasoning a
  long turn produces before it says anything
- Mirrors the tmux pane of a session running in one, and types into it, so a
  prompt the agent is blocked on can be answered without leaving the board
- Goes to a session in a new tab of the terminal it is running in — attaching
  to the tmux pane a live session occupies, or resuming a finished one with
  that agent's installed CLI — so the board keeps its own window; terminals
  without a tab interface fall back to opening in place
- Starts a new session from the board: a standing input under the board takes
  a task description and opens it as a fresh agent — any installed one — in a
  detached tmux session of its own, which discovery then picks up like any
  other
- Dismisses any session from the board with ctrl+x pressed twice, remembered
  in openagentview's own state file without touching the agent's store
- Refreshes automatically without writing to any agent's private state

## Requirements

- Go 1.26 or newer to build
- Codex CLI as `codex`, Claude Code as `claude`, Grok CLI as `grok`, or any
  subset of them
- macOS or Linux for live process detection
- tmux, optionally: without it the board still discovers, previews and
  resumes sessions, but the composer, the pane mirror and `-t` need it

Any agent may be absent. A missing agent is reported in the footer while the
other agents' sessions keep working.

## Install

```sh
go install github.com/Jewel591/openagentview/cmd/openagentview@latest
```

`@latest` installs the newest tagged release. `main` moves faster than the
tags; to try what has merged but not yet shipped:

```sh
go install github.com/Jewel591/openagentview/cmd/openagentview@main
```

Or build from a clone:

```sh
go build -o bin/openagentview ./cmd/openagentview
./bin/openagentview
```

To watch only what is running in front of you, `-t` limits the board to
sessions running inside a tmux pane:

```sh
./bin/openagentview -t
```

`-t` requires tmux to be installed and exits with an error when it is not,
since a board of tmux sessions can only ever be empty without it. The tmux
server itself may come and go: an empty `-t` board fills in as agents start
inside tmux.

To use non-default agent directories:

```sh
openagentview --codex-home /path/to/.codex \
  --claude-home /path/to/.claude \
  --grok-home /path/to/.grok
```

## Keyboard

| Key | Action |
| --- | --- |
| `←` `→` / `h` `l` | Move between columns |
| `↑` `↓` / `k` `j` | Move between cards |
| `Enter` / `Space` | Quick Look the selected conversation |
| `Ctrl+Enter` | Open the session in a new terminal tab |
| `d` | Open session details |
| `Ctrl+S` / `Tab` | Group by Status / Projects |
| `v` | Switch the Kanban / List layout |
| `?` | Show or hide all shortcuts |
| `/` / `s` | Search |
| `n` | Describe a task and start it as a new tmux session |
| `Ctrl+X` twice | Dismiss the selected session from the board |
| `r` | Refresh |
| `q` | Quit |

While the composer is focused, the keyboard belongs to it: `Enter` starts the
session, `Tab` picks which agent runs it, and `Esc` puts the draft down
without losing it. Typing `@` picks where the task starts: a menu offers the
directory openagentview was launched from and every project that already has
a session on the board, filtered as you type, and a query starting with `/`
or `~` completes filesystem paths shell-style instead (`Tab` completes,
`Enter` picks, `Esc` keeps the text as typed). A directory picked this way
shows as `@name` next to the agent tag until the task starts, and survives
the draft being put down; the board's own directory goes unlabelled. The composer
appears only when tmux and at least one agent CLI are installed.

Quick Look on a session running in a tmux pane opens typing into that agent.
The pane is mirrored at its own width, in a window over the board. Every key
goes to the agent, `Esc` and `Enter` included:

| Key | Action |
| --- | --- |
| `Ctrl+Space` | Type a space — the space bar closes Quick Look instead |
| `Ctrl+C` | Interrupt the agent in the pane |
| `Space` | Close Quick Look |
| `Ctrl+]` | Stop typing and hand the board's keys back |

Once `Ctrl+]` has stopped typing:

| Key | Action |
| --- | --- |
| `↑` / `↓`, `PgUp` / `PgDn`, `g` / `G` | Scroll the pane or transcript |
| `t` | Switch between the live pane and the stored transcript |
| `i` | Start typing again |
| `Enter` | Open the session in a new terminal tab |
| `Esc` / `Space` | Close Quick Look |

Full-screen agent TUIs use the terminal's alternate screen, which tmux keeps
only as a current frame rather than scrollback. Quick Look labels those mirrors
`current screen only`; switch to the session transcript to read earlier
conversation content. Ordinary tmux history is loaded in growing pages as it
is needed, up to the pane's actual history limit.

Opening a tab drives the terminal's own scripting interface — AppleScript
for Ghostty, iTerm2 and Terminal.app (macOS asks once for automation
permission), the remote-control CLI for kitty and WezTerm. kitty ships with
remote control disabled, so it needs `allow_remote_control yes` in its
config to count. In a terminal the board cannot ask for a tab, going to a
session falls back to taking over the board's window until the agent exits.

## Data safety

Agent-owned stores are always read-only: the Codex state database is opened
read-only, and Grok's session directory is only ever read. Mutating actions go
through the agent's own CLI. openagentview never writes to Codex SQLite rows,
rollout JSONL files, or anything under `~/.grok`.

openagentview has no archive or delete action. Taking a session off the board
is a dismissal, recorded in openagentview's own state file — no agent's
transcript or store is ever modified, and sessions a Codex user archived
through Codex itself simply stay off the board.

## Architecture

```text
Codex adapter  ─┐
Claude adapter  ├── normalized sessions ── preset layouts ── TUI
Grok adapter   ─┘            │
                            live process observer
```

The `agent.Adapter` interface separates discovery and lifecycle commands from
the board, and `agent.Multi` fans one board out over several agents: discovery
runs them in parallel, and every other operation is routed back to the adapter
whose name the session carries. A new adapter only needs to map its product's
session model into the normalized `agent.Session` type.

Sessions are matched to tmux panes by walking each live process up to the pane
that contains it, so an agent started through a shim or wrapper still resolves.
That mapping lives in one decorator around the agents rather than in each
adapter: which pane a session runs in is a fact about the machine, not about the
agent. A mirrored pane is a rendered screen rather than text and cannot be
rewrapped, so the mirror is sized to the pane instead of to a fraction of the
terminal, and its frame is sized to what is left over. A pane with room to
spare gets a bordered, padded window; a pane that fills the terminal — the
common case, since agents are usually run in a window the size of this one —
gets the tightest frame that still reads as a window, a border and no padding,
and the columns that costs are reported in the window's subtitle. Only a
terminal too small for either drops the frame and mirrors edge to edge. The
pane's own width comes back from the same tmux call as its cells: sizing the
window to the widest captured line would instead resize it every time the agent
printed a longer one. A capture holds cells and not terminal state, so the
mirror has no cursor of its own — tmux is asked for the pane's cursor in the
same call, and this terminal's cursor is placed where the agent's sits.

Status and in-turn activity are derived from each agent's own log tail
(`rollout-*.jsonl` for Codex, `events.jsonl` for Grok) rather than from a
cached board scan, so Quick Look stays current while it is open. Claude Code is
the exception and the easier case: it publishes what every running session is
doing in `~/.claude/sessions/<pid>.json`, so its status is read rather than
inferred. Its transcripts hold every session ever recorded, and what the board
shows of each one — the title it gave itself, the prompt it opened with — is
parsed from the two ends of the file and cached against size and mtime, so a
scan re-reads only the sessions being written to.

## Development

```sh
go test ./...
go vet ./...
go build ./cmd/openagentview
```

This initial version intentionally derives live status locally. Codex's
app-server protocol is still experimental and a separately started app-server
does not own sessions already running in independent Codex processes.

## Contributing

Contributions are welcome — see [CONTRIBUTING.md](CONTRIBUTING.md) for the
check sequence to run before opening a pull request and the product
invariants the codebase is built around. Adding support for a new agent CLI
only takes one new adapter; the board itself needs no changes.

## License

[MIT](LICENSE)
