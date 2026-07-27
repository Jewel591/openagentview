package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"

	tea "charm.land/bubbletea/v2"

	"github.com/Jewel591/openagentview/internal/agent"
	"github.com/Jewel591/openagentview/internal/claude"
	"github.com/Jewel591/openagentview/internal/codex"
	"github.com/Jewel591/openagentview/internal/dismiss"
	"github.com/Jewel591/openagentview/internal/grok"
	"github.com/Jewel591/openagentview/internal/tmux"
	"github.com/Jewel591/openagentview/internal/ui"
)

var version = "dev"

func main() {
	var (
		claudeHome string
		codexHome  string
		grokHome   string
		tmuxOnly   bool
		showVerion bool
	)
	flag.StringVar(&claudeHome, "claude-home", "", "Claude Code data directory (default: $CLAUDE_CONFIG_DIR or ~/.claude)")
	flag.StringVar(&codexHome, "codex-home", "", "Codex data directory (default: $CODEX_HOME or ~/.codex)")
	flag.StringVar(&grokHome, "grok-home", "", "Grok data directory (default: $GROK_HOME or ~/.grok)")
	flag.BoolVar(&tmuxOnly, "t", false, "only show sessions running in tmux panes")
	flag.BoolVar(&showVerion, "version", false, "print version and exit")
	flag.Parse()

	if showVerion {
		fmt.Println("openagentview " + version)
		return
	}

	codexAdapter, err := codex.New(codexHome)
	if err != nil {
		fatal(err)
	}
	grokAdapter, err := grok.New(grokHome)
	if err != nil {
		fatal(err)
	}
	claudeAdapter, err := claude.New(claudeHome)
	if err != nil {
		fatal(err)
	}

	// tmux facts are resolved once for the whole board rather than per agent:
	// which pane a session runs in is a fact about the machine, not the agent.
	panes := tmux.NewAdapter(
		agent.NewMulti(codexAdapter, grokAdapter, claudeAdapter),
		tmux.New(),
		tmuxOnly,
	)

	// The composer only offers agents that are actually installed — a task
	// handed to a missing CLI would start a tmux session that dies on arrival —
	// and only when tmux itself is, since that is where a composed session
	// runs. Without either, the composer is absent rather than a dead end.
	var launchers []ui.Launcher
	if _, err := exec.LookPath("tmux"); err == nil {
		for _, launcher := range []ui.Launcher{
			{Agent: claudeAdapter.Name(), Command: claudeAdapter.NewSessionCommand},
			{Agent: codexAdapter.Name(), Command: codexAdapter.NewSessionCommand},
			{Agent: grokAdapter.Name(), Command: grokAdapter.NewSessionCommand},
		} {
			name, _ := launcher.Command("")
			if _, err := exec.LookPath(name); err == nil {
				launchers = append(launchers, launcher)
			}
		}
	}

	// A board that cannot remember dismissals still works: the store stays
	// nil, and ctrl+x reports the problem instead of writing over whatever
	// the state file still says.
	dismissals, _ := dismiss.Open()

	program := tea.NewProgram(ui.New(panes, panes, panes, launchers, dismissals))
	if _, err := program.Run(); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "openagentview:", err)
	os.Exit(1)
}
