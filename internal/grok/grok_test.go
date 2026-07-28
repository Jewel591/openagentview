package grok

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jewel591/openagentview/internal/agent"
)

// writeSession lays out one session the way grok does: a percent-encoded
// workspace directory holding one directory per session id.
func writeSession(t *testing.T, home, cwd, id string, files map[string]string) string {
	t.Helper()
	workspace := filepath.Join(home, "sessions", encodeWorkspace(cwd))
	dir := filepath.Join(workspace, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		target := filepath.Join(dir, name)
		if strings.HasPrefix(name, "../") {
			target = filepath.Join(workspace, strings.TrimPrefix(name, "../"))
		}
		if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func encodeWorkspace(cwd string) string {
	encoded := ""
	for i := 0; i < len(cwd); i++ {
		c := cwd[i]
		if c == '/' {
			encoded += "%2F"
			continue
		}
		encoded += string(c)
	}
	return encoded
}

func writeActiveSessions(t *testing.T, home string, entries string) {
	t.Helper()
	if err := os.WriteFile(
		filepath.Join(home, "active_sessions.json"),
		[]byte(entries),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverReadsSummariesAndLiveTurnState(t *testing.T) {
	home := t.TempDir()
	writeSession(t, home, "/projects/mono", "AAAA", map[string]string{
		"summary.json": `{
			"info": {"id": "AAAA", "cwd": "/projects/mono"},
			"session_summary": "Fix token refresh",
			"generated_title": "Fix token refresh",
			"created_at": "2026-07-27T08:00:00.000000Z",
			"updated_at": "2026-07-27T08:30:00.000000Z",
			"head_branch": "feat/auth",
			"agent_name": "grok-build-plan",
			"current_model_id": "grok-4.5"
		}`,
		"signals.json": `{"contextTokensUsed": 4321}`,
		"events.jsonl": strings.Join([]string{
			`{"ts":"2026-07-27T08:00:00.000Z","type":"turn_started"}`,
			`{"ts":"2026-07-27T08:00:01.000Z","type":"tool_started","tool_name":"read_file"}`,
		}, "\n") + "\n",
		"updates.jsonl": "",
	})
	writeActiveSessions(t, home, fmt.Sprintf(
		`[{"session_id":"AAAA","pid":%d,"cwd":"/projects/mono"}]`,
		os.Getpid(),
	))

	adapter := &Adapter{home: home}
	sessions, err := adapter.Discover(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(sessions))
	}
	session := sessions[0]
	if session.Agent != "grok" {
		t.Fatalf("agent = %q, want grok", session.Agent)
	}
	if session.Title != "Fix token refresh" {
		t.Fatalf("title = %q", session.Title)
	}
	if session.CWD != "/projects/mono" || session.Branch != "feat/auth" {
		t.Fatalf("workspace = %q, branch = %q", session.CWD, session.Branch)
	}
	if session.TokensUsed != 4321 {
		t.Fatalf("tokens = %d, want 4321", session.TokensUsed)
	}
	if session.RuntimeStatus != agent.StatusRunning {
		t.Fatalf("status = %q, want %q", session.RuntimeStatus, agent.StatusRunning)
	}
	if session.PID != os.Getpid() {
		t.Fatalf("pid = %d, want %d", session.PID, os.Getpid())
	}
}

func TestDiscoverIgnoresSessionsWhoseProcessIsGone(t *testing.T) {
	home := t.TempDir()
	writeSession(t, home, "/projects/mono", "AAAA", map[string]string{
		"summary.json": `{
			"info": {"id": "AAAA", "cwd": "/projects/mono"},
			"generated_title": "Abandoned",
			"updated_at": "2026-07-27T08:30:00.000000Z"
		}`,
		"events.jsonl": `{"ts":"2026-07-27T08:00:00.000Z","type":"turn_started"}` + "\n",
	})
	// A pid that cannot be running: grok removes entries on a clean exit, but a
	// killed process leaves its claim behind.
	writeActiveSessions(t, home, `[{"session_id":"AAAA","pid":2147483640}]`)

	adapter := &Adapter{home: home}
	sessions, err := adapter.Discover(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if sessions[0].RuntimeStatus != agent.StatusIdle {
		t.Fatalf("status = %q, want %q", sessions[0].RuntimeStatus, agent.StatusIdle)
	}
	if sessions[0].PID != 0 {
		t.Fatalf("pid = %d, want 0", sessions[0].PID)
	}
}

func TestDiscoverTitlesUnsummarizedSessionsFromTheirFirstPrompt(t *testing.T) {
	home := t.TempDir()
	writeSession(t, home, "/projects/mono", "BBBB", map[string]string{
		"summary.json": `{
			"info": {"id": "BBBB", "cwd": "/projects/mono"},
			"session_summary": "",
			"updated_at": "2026-07-27T08:30:00.000000Z"
		}`,
		"../prompt_history.jsonl": strings.Join([]string{
			`{"timestamp":"2026-07-27T08:00:00Z","session_id":"BBBB","prompt":"why is the board empty?\nmore detail"}`,
			`{"timestamp":"2026-07-27T08:01:00Z","session_id":"BBBB","prompt":"second prompt"}`,
		}, "\n") + "\n",
	})

	adapter := &Adapter{home: home}
	sessions, err := adapter.Discover(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := sessions[0].Title; got != "why is the board empty?" {
		t.Fatalf("title = %q, want the first line of the first prompt", got)
	}
}

func TestDiscoverFallsBackToUntitledWithoutAnyPromptHistory(t *testing.T) {
	home := t.TempDir()
	writeSession(t, home, "/projects/mono", "CCCC", map[string]string{
		"summary.json": `{"info": {"id": "CCCC", "cwd": "/projects/mono"}}`,
	})

	adapter := &Adapter{home: home}
	sessions, err := adapter.Discover(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := sessions[0].Title; got != "Untitled session" {
		t.Fatalf("title = %q, want Untitled session", got)
	}
}

func TestDiscoverKeepsTheMostRecentlyTouchedSessionsWithinTheLimit(t *testing.T) {
	home := t.TempDir()
	for i := range 5 {
		id := fmt.Sprintf("S%d", i)
		writeSession(t, home, "/projects/mono", id, map[string]string{
			"summary.json": fmt.Sprintf(
				`{"info":{"id":%q,"cwd":"/projects/mono"},"generated_title":%q,"updated_at":"2026-07-27T08:0%d:00.000000Z"}`,
				id, id, i,
			),
		})
	}

	adapter := &Adapter{home: home}
	sessions, err := adapter.Discover(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("sessions = %d, want the 2 the limit allows", len(sessions))
	}
}

func TestResumeCommandUsesResumeFlag(t *testing.T) {
	adapter := &Adapter{home: t.TempDir()}
	name, args := adapter.ResumeCommand(agent.Session{ID: "AAAA"})
	if name != "grok" || len(args) != 2 || args[0] != "--resume" || args[1] != "AAAA" {
		t.Fatalf("resume command = %q %v", name, args)
	}
}

func TestDecodeWorkspaceRecoversThePath(t *testing.T) {
	got := decodeWorkspace(filepath.Join("/root", "%2FUsers%2Fme%2Fcode"))
	if got != "/Users/me/code" {
		t.Fatalf("decodeWorkspace() = %q", got)
	}
}
