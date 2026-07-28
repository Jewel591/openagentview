package tmux

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Jewel591/openagentview/internal/agent"
)

type stubAdapter struct {
	sessions []agent.Session
	err      error
}

func (a *stubAdapter) Name() string { return "codex" }

func (a *stubAdapter) Discover(context.Context, int) ([]agent.Session, error) {
	return append([]agent.Session(nil), a.sessions...), a.err
}

func (a *stubAdapter) Preview(
	context.Context,
	agent.Session,
	int,
) (agent.Transcript, error) {
	return agent.Transcript{}, nil
}

func (a *stubAdapter) ResumeCommand(s agent.Session) (string, []string) {
	return "codex", []string{"resume", s.ID}
}

func TestAnnotateTagsSessionsWithTheirPane(t *testing.T) {
	sessions := []agent.Session{
		{ID: "in-tmux", PID: 999},
		{ID: "bare-terminal", PID: 555},
		{ID: "not-running"},
	}
	index := newIndex(
		[]Pane{{ID: "%2", PID: 200, Target: "cw:1.0"}},
		map[int]int{999: 200, 200: 1, 555: 1},
	)

	got := annotate(sessions, index, false)
	if len(got) != 3 {
		t.Fatalf("sessions = %d, want all three kept", len(got))
	}
	if got[0].TmuxPane != "%2" || got[0].TmuxTarget != "cw:1.0" {
		t.Fatalf("session in tmux = %#v, want its pane", got[0])
	}
	if got[1].TmuxPane != "" || got[2].TmuxPane != "" {
		t.Fatalf("sessions outside tmux were given panes: %#v", got[1:])
	}
}

func TestAnnotateDropsSessionsOutsideTmuxWhenAskedFor(t *testing.T) {
	sessions := []agent.Session{
		{ID: "in-tmux", PID: 999},
		{ID: "bare-terminal", PID: 555},
	}
	index := newIndex(
		[]Pane{{ID: "%2", PID: 200, Target: "cw:1.0"}},
		map[int]int{999: 200, 200: 1, 555: 1},
	)

	got := annotate(sessions, index, true)
	if len(got) != 1 || got[0].ID != "in-tmux" {
		t.Fatalf("sessions = %#v, want only the one running in a pane", got)
	}
}

// No tmux server is the ordinary state of a machine with no agents in front of
// anyone. It must not empty a board that was not asked to be tmux-only.
func TestDiscoverKeepsSessionsWhenNoTmuxServerIsRunning(t *testing.T) {
	inner := &stubAdapter{sessions: []agent.Session{{ID: "one", PID: 5}}}
	client := NewWithSocket("oav-test-no-server")

	sessions, err := NewAdapter(inner, client, false).Discover(context.Background(), 10)
	if err != nil {
		t.Fatalf("Discover() = %v, want the sessions anyway", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions = %#v, want the inner adapter's", sessions)
	}

	sessions, err = NewAdapter(inner, client, true).Discover(context.Background(), 10)
	if err != nil || len(sessions) != 0 {
		t.Fatalf("tmux-only Discover() = %#v, %v, want an empty board", sessions, err)
	}
}

func TestDiscoverPassesThroughAPartialFailure(t *testing.T) {
	failure := errors.New("grok unavailable")
	inner := &stubAdapter{
		sessions: []agent.Session{{ID: "codex-one"}},
		err:      failure,
	}

	sessions, err := NewAdapter(inner, NewWithSocket("oav-test-none"), false).
		Discover(context.Background(), 10)
	if !errors.Is(err, failure) {
		t.Fatalf("Discover() error = %v, want the inner failure", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions = %#v, want what the healthy agent returned", sessions)
	}
}

// Resuming a session that is already running would start a second agent against
// the same rollout. When it lives in a pane there is somewhere to go instead.
func TestResumeCommandReturnsToTheExistingPane(t *testing.T) {
	adapter := NewAdapter(&stubAdapter{}, NewWithSocket("oavtest"), false)

	name, args := adapter.ResumeCommand(agent.Session{ID: "s", TmuxPane: "%4"})
	joined := name + " " + strings.Join(args, " ")
	if !strings.Contains(joined, "-L oavtest") {
		t.Fatalf("resume = %q, want the client's socket", joined)
	}
	for _, want := range []string{"select-window -t %4", "select-pane -t %4"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("resume = %q, want it to focus the pane with %q", joined, want)
		}
	}
	if !strings.Contains(joined, "attach -t %4") &&
		!strings.Contains(joined, "switch-client -t %4") {
		t.Fatalf("resume = %q, want it to arrive at the pane", joined)
	}

	name, args = adapter.ResumeCommand(agent.Session{ID: "s"})
	if name != "codex" || strings.Join(args, " ") != "resume s" {
		t.Fatalf("resume outside tmux = %q %v, want the agent's own command", name, args)
	}
}
