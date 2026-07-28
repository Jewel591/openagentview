package tmux

import (
	"context"

	"github.com/Jewel591/openagentview/internal/agent"
)

// Adapter decorates another adapter with the tmux facts the board cannot get
// from a rollout log: which pane a live session is running in, and how to get
// back to it. Resolving panes here rather than inside each agent's adapter
// keeps the tmux dependency in one place — a session's pane is a fact about the
// machine, not about the agent that wrote the log.
type Adapter struct {
	inner  agent.Adapter
	client *Client
	// only drops sessions that are not running in tmux, which is what turns the
	// board into a view of "what is running in front of me right now".
	only bool
}

func NewAdapter(inner agent.Adapter, client *Client, only bool) *Adapter {
	return &Adapter{inner: inner, client: client, only: only}
}

func (a *Adapter) Name() string {
	return a.inner.Name()
}

func (a *Adapter) Discover(ctx context.Context, limit int) ([]agent.Session, error) {
	sessions, err := a.inner.Discover(ctx, limit)
	if err != nil && sessions == nil {
		return nil, err
	}
	index, indexErr := a.client.NewIndex(ctx)
	if indexErr != nil {
		// No tmux server is the normal state of a machine with no agents in
		// front of anyone, so it must not hide the sessions themselves — unless
		// the board was asked for tmux sessions only, where the honest answer is
		// an empty board.
		if a.only {
			return nil, err
		}
		return sessions, err
	}
	return annotate(sessions, index, a.only), err
}

func annotate(sessions []agent.Session, index *Index, only bool) []agent.Session {
	result := sessions
	if only {
		result = make([]agent.Session, 0, len(sessions))
	}
	for i := range sessions {
		pane, ok := index.PaneFor(sessions[i].PID)
		if ok {
			sessions[i].TmuxPane = pane.ID
			sessions[i].TmuxTarget = pane.Target
		}
		if only {
			if ok {
				result = append(result, sessions[i])
			}
			continue
		}
		result[i] = sessions[i]
	}
	return result
}

func (a *Adapter) Preview(
	ctx context.Context,
	session agent.Session,
	limit int,
) (agent.Transcript, error) {
	return a.inner.Preview(ctx, session, limit)
}

// ResumeCommand returns to the pane the session is already running in instead
// of starting a second copy of the agent against the same rollout. Sessions
// outside tmux keep the agent's own resume command.
func (a *Adapter) ResumeCommand(session agent.Session) (string, []string) {
	if session.TmuxPane == "" {
		return a.inner.ResumeCommand(session)
	}
	// A pane id resolves to its window and session, so every step below can be
	// addressed by the one identifier. Selecting the window and the pane before
	// arriving is what makes the agent the thing on screen, rather than whatever
	// the session happened to leave current.
	focus := []string{
		"select-window", "-t", session.TmuxPane,
		";", "select-pane", "-t", session.TmuxPane,
	}
	if Inside() {
		// Already in tmux: move this client rather than nesting a second one.
		return "tmux", a.client.Args(
			append(focus, ";", "switch-client", "-t", session.TmuxPane)...,
		)
	}
	return "tmux", a.client.Args(
		append(focus, ";", "attach", "-t", session.TmuxPane)...,
	)
}

// Capture, SendText and SendKey expose the pane to the UI so it can mirror a
// live agent and answer it without leaving the board.
func (a *Adapter) Capture(ctx context.Context, paneID string) (Screen, error) {
	return a.client.Capture(ctx, paneID)
}

func (a *Adapter) SendText(ctx context.Context, paneID, text string) error {
	return a.client.SendText(ctx, paneID, text)
}

func (a *Adapter) SendKey(ctx context.Context, paneID, key string) error {
	return a.client.SendKey(ctx, paneID, key)
}

// NewSession starts a fresh agent in a detached session of its own. It writes
// tmux state, never agent state: the agent's own store is only ever touched by
// the agent the command starts.
func (a *Adapter) NewSession(
	ctx context.Context,
	name, dir string,
	command []string,
	width, height int,
) (string, error) {
	return a.client.NewSession(ctx, name, dir, command, width, height)
}
