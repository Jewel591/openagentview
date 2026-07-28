package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type stubAdapter struct {
	name     string
	sessions []Session
	err      error
	previews int
}

func (a *stubAdapter) Name() string {
	return a.name
}

func (a *stubAdapter) Discover(context.Context, int) ([]Session, error) {
	if a.err != nil {
		return nil, a.err
	}
	return a.sessions, nil
}

func (a *stubAdapter) Preview(context.Context, Session, int) (Transcript, error) {
	a.previews++
	return Transcript{
		Messages: []TranscriptMessage{{Role: RoleAgent, Text: a.name}},
	}, nil
}

func (a *stubAdapter) ResumeCommand(Session) (string, []string) {
	return a.name, []string{"resume"}
}

func TestMultiMergesSessionsNewestFirst(t *testing.T) {
	now := time.Now()
	codex := &stubAdapter{
		name: "codex",
		sessions: []Session{
			{ID: "codex-old", Agent: "codex", RecencyAt: now.Add(-time.Hour)},
		},
	}
	grok := &stubAdapter{
		name: "grok",
		sessions: []Session{
			{ID: "grok-new", Agent: "grok", RecencyAt: now},
		},
	}

	sessions, err := NewMulti(codex, grok).Discover(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("sessions = %d, want both agents", len(sessions))
	}
	if sessions[0].ID != "grok-new" {
		t.Fatalf("first session = %q, want the most recent", sessions[0].ID)
	}
}

func TestMultiKeepsWorkingAgentsWhenAnotherFails(t *testing.T) {
	codex := &stubAdapter{
		name:     "codex",
		sessions: []Session{{ID: "codex-1", Agent: "codex"}},
	}
	grok := &stubAdapter{name: "grok", err: errors.New("no such directory")}

	sessions, err := NewMulti(codex, grok).Discover(context.Background(), 10)
	if len(sessions) != 1 || sessions[0].ID != "codex-1" {
		t.Fatalf("sessions = %#v, want the healthy agent's session", sessions)
	}
	if err == nil {
		t.Fatal("Discover() hid the failing agent entirely")
	}
	if !strings.Contains(err.Error(), "grok") {
		t.Fatalf("error = %q, want it to name the failing agent", err)
	}
}

func TestMultiFailsOnlyWhenEveryAgentFails(t *testing.T) {
	codex := &stubAdapter{name: "codex", err: errors.New("codex down")}
	grok := &stubAdapter{name: "grok", err: errors.New("grok down")}

	sessions, err := NewMulti(codex, grok).Discover(context.Background(), 10)
	if sessions != nil {
		t.Fatalf("sessions = %#v, want none", sessions)
	}
	if err == nil {
		t.Fatal("Discover() succeeded with no working agent")
	}
}

func TestMultiRoutesWorkToTheAgentThatOwnsTheSession(t *testing.T) {
	codex := &stubAdapter{name: "codex"}
	grok := &stubAdapter{name: "grok"}
	multi := NewMulti(codex, grok)

	transcript, err := multi.Preview(
		context.Background(),
		Session{ID: "g", Agent: "grok"},
		4,
	)
	if err != nil {
		t.Fatal(err)
	}
	if grok.previews != 1 || codex.previews != 0 {
		t.Fatalf("previews: grok=%d codex=%d, want the owning agent only",
			grok.previews, codex.previews)
	}
	if transcript.Messages[0].Text != "grok" {
		t.Fatalf("transcript came from %q", transcript.Messages[0].Text)
	}

	name, _ := multi.ResumeCommand(Session{Agent: "codex"})
	if name != "codex" {
		t.Fatalf("resume command = %q, want codex", name)
	}
}

func TestMultiReportsSessionsFromAnAgentItDoesNotHave(t *testing.T) {
	multi := NewMulti(&stubAdapter{name: "codex"})
	if _, err := multi.Preview(context.Background(), Session{Agent: "claude"}, 4); err == nil {
		t.Fatal("Preview() accepted a session from an unconfigured agent")
	}
}
