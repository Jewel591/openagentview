package claude

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Jewel591/openagentview/internal/agent"
)

// home builds a Claude Code data directory: transcripts under projects, and the
// registry of running sessions under sessions.
type home struct {
	t    *testing.T
	path string
}

func newHome(t *testing.T) *home {
	t.Helper()
	return &home{t: t, path: t.TempDir()}
}

func (h *home) adapter() *Adapter {
	h.t.Helper()
	adapter, err := New(h.path)
	if err != nil {
		h.t.Fatalf("New: %v", err)
	}
	return adapter
}

func (h *home) transcript(project, sessionID string, records ...any) string {
	h.t.Helper()
	dir := filepath.Join(h.path, "projects", project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, sessionID+".jsonl")
	h.append(path, records...)
	return path
}

func (h *home) append(path string, records ...any) {
	h.t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		h.t.Fatalf("open transcript: %v", err)
	}
	defer file.Close()
	for _, record := range records {
		line, err := json.Marshal(record)
		if err != nil {
			h.t.Fatalf("marshal record: %v", err)
		}
		if _, err := file.Write(append(line, '\n')); err != nil {
			h.t.Fatalf("write record: %v", err)
		}
	}
}

func (h *home) live(pid int, entry map[string]any) {
	h.t.Helper()
	dir := filepath.Join(h.path, "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.t.Fatalf("mkdir: %v", err)
	}
	entry["pid"] = pid
	data, err := json.Marshal(entry)
	if err != nil {
		h.t.Fatalf("marshal registry entry: %v", err)
	}
	name := filepath.Join(dir, strconv.Itoa(pid)+".json")
	if err := os.WriteFile(name, data, 0o644); err != nil {
		h.t.Fatalf("write registry entry: %v", err)
	}
}

func userRecord(text string, at time.Time) map[string]any {
	return map[string]any{
		"type":      "user",
		"timestamp": at.Format(time.RFC3339Nano),
		"cwd":       "/projects/mono",
		"gitBranch": "feat/auth",
		"version":   "2.1.220",
		"message":   map[string]any{"role": "user", "content": text},
	}
}

func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run true: %v", err)
	}
	return cmd.Process.Pid
}

func sessionByID(sessions []agent.Session, id string) (agent.Session, bool) {
	for _, session := range sessions {
		if session.ID == id {
			return session, true
		}
	}
	return agent.Session{}, false
}

// Claude Code publishes what each running session is doing, so the board's
// status is read rather than inferred.
func TestDiscoverTakesStatusFromTheLiveRegistry(t *testing.T) {
	h := newHome(t)
	now := time.Now()
	for _, id := range []string{"busy-one", "waiting-one", "idle-one", "gone-one"} {
		h.transcript("-projects-mono", id, userRecord("do the thing", now))
	}
	h.live(os.Getpid(), map[string]any{
		"sessionId": "busy-one",
		"status":    "busy",
		"name":      "mono-ab",
		"kind":      "interactive",
		"cwd":       "/projects/mono",
	})
	h.live(os.Getpid()+100000, map[string]any{
		"sessionId": "gone-one",
		"status":    "busy",
	})

	sessions, err := h.adapter().Discover(context.Background(), 0)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(sessions) != 4 {
		t.Fatalf("sessions = %d, want 4", len(sessions))
	}

	busy, _ := sessionByID(sessions, "busy-one")
	if busy.RuntimeStatus != agent.StatusRunning {
		t.Fatalf("busy session = %q, want running", busy.RuntimeStatus)
	}
	if busy.PID != os.Getpid() || busy.AgentNickname != "mono-ab" {
		t.Fatalf("busy session = %#v, want the registry's pid and name", busy)
	}
	// A registry entry whose process is gone is a leftover, not a live session.
	gone, _ := sessionByID(sessions, "gone-one")
	if gone.RuntimeStatus != agent.StatusIdle || gone.PID != 0 {
		t.Fatalf("dead session = %#v, want idle with no pid", gone)
	}
}

func TestRuntimeStatusMapsEveryStateClaudePublishes(t *testing.T) {
	cases := map[string]agent.RuntimeStatus{
		"busy":      agent.StatusRunning,
		"waiting":   agent.StatusNeedsYou,
		"idle":      agent.StatusIdle,
		"":          agent.StatusIdle,
		"something": agent.StatusIdle,
	}
	for status, want := range cases {
		if got := runtimeStatus(status); got != want {
			t.Fatalf("runtimeStatus(%q) = %q, want %q", status, got, want)
		}
	}
}

func TestRegistryLeftoverLosesToTheProcessThatIsRunning(t *testing.T) {
	h := newHome(t)
	h.transcript("-projects-mono", "resumed", userRecord("hello", time.Now()))
	h.live(deadPID(t), map[string]any{
		"sessionId": "resumed",
		"status":    "busy",
		"updatedAt": 1,
	})
	h.live(os.Getpid(), map[string]any{
		"sessionId": "resumed",
		"status":    "waiting",
		"updatedAt": 2,
	})

	sessions, err := h.adapter().Discover(context.Background(), 0)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if sessions[0].RuntimeStatus != agent.StatusNeedsYou || sessions[0].PID != os.Getpid() {
		t.Fatalf("session = %#v, want the running process's entry", sessions[0])
	}
}

func TestTitlePrefersTheTitleClaudeGaveTheSession(t *testing.T) {
	h := newHome(t)
	now := time.Now()
	h.transcript("-projects-mono", "titled",
		userRecord("fix the token refresh please", now),
		map[string]any{"type": "ai-title", "aiTitle": "First title"},
		map[string]any{"type": "ai-title", "aiTitle": "Token refresh fix"},
	)
	h.transcript("-projects-mono", "untitled", userRecord("just this prompt\nsecond line", now))
	h.transcript("-projects-mono", "empty", map[string]any{"type": "mode", "mode": "normal"})

	sessions, err := h.adapter().Discover(context.Background(), 0)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	titled, _ := sessionByID(sessions, "titled")
	if titled.Title != "Token refresh fix" {
		t.Fatalf("title = %q, want the newest ai-title", titled.Title)
	}
	if titled.Preview != "fix the token refresh please" {
		t.Fatalf("preview = %q, want the opening prompt", titled.Preview)
	}
	untitled, _ := sessionByID(sessions, "untitled")
	if untitled.Title != "just this prompt" {
		t.Fatalf("title = %q, want the first line of the opening prompt", untitled.Title)
	}
	empty, _ := sessionByID(sessions, "empty")
	if empty.Title != "Untitled session" {
		t.Fatalf("title = %q, want a placeholder", empty.Title)
	}
}

// Claude Code stores several things as user turns that nobody typed.
func TestHumanTextKeepsWhatAPersonTyped(t *testing.T) {
	cases := map[string]string{
		"plain question": "plain question",
		"<local-command-stdout>output</local-command-stdout>":                                                          "",
		"<command-name>/clear</command-name>\n<command-message>clear</command-message>\n<command-args></command-args>": "/clear",
		"<command-name>/loop</command-name>\n<command-args>5m /check</command-args>":                                   "/loop 5m /check",
		"real words<system-reminder>ignore this</system-reminder> more words":                                          "real words more words",
		"trailing<system-reminder>unclosed":                                                                            "trailing",
	}
	for input, want := range cases {
		if got := humanText(input); got != want {
			t.Fatalf("humanText(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestWorkspaceComesFromTheTranscriptAndFallsBackToItsDirectory(t *testing.T) {
	h := newHome(t)
	now := time.Now()
	h.transcript("-projects-mono", "recorded", userRecord("hello", now))
	h.transcript("-Users-someone-code-thing", "unrecorded",
		map[string]any{"type": "mode", "mode": "normal"})

	sessions, err := h.adapter().Discover(context.Background(), 0)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	recorded, _ := sessionByID(sessions, "recorded")
	if recorded.CWD != "/projects/mono" || recorded.Branch != "feat/auth" {
		t.Fatalf("session = %#v, want the workspace the transcript recorded", recorded)
	}
	unrecorded, _ := sessionByID(sessions, "unrecorded")
	if unrecorded.CWD != "/Users/someone/code/thing" {
		t.Fatalf("cwd = %q, want it decoded from the directory name", unrecorded.CWD)
	}
}

// Discovery re-runs every couple of seconds over every session ever recorded,
// so a transcript is only parsed again once it has been written to.
func TestMetadataIsParsedAgainOnlyWhenTheTranscriptChanges(t *testing.T) {
	h := newHome(t)
	now := time.Now()
	path := h.transcript("-projects-mono", "growing",
		userRecord("first prompt", now),
		map[string]any{"type": "ai-title", "aiTitle": "Original"},
	)
	adapter := h.adapter()

	if _, err := adapter.Discover(context.Background(), 0); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	// Rewritten in place, keeping size and mtime: a re-read would see the new
	// title, and the cache is what must not.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	rewritten := strings.Replace(string(data), "Original", "Rewrite0", 1)
	if len(rewritten) != len(data) {
		t.Fatalf("rewrite changed the file size, so the test proves nothing")
	}
	if err := os.WriteFile(path, []byte(rewritten), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	sessions, err := adapter.Discover(context.Background(), 0)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if sessions[0].Title != "Original" {
		t.Fatalf("title = %q, want the cached one", sessions[0].Title)
	}

	// Appending moves both size and mtime, which is what a live session does.
	h.append(path, map[string]any{"type": "ai-title", "aiTitle": "Appended"})
	sessions, err = adapter.Discover(context.Background(), 0)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if sessions[0].Title != "Appended" {
		t.Fatalf("title = %q, want the transcript re-read after it grew", sessions[0].Title)
	}
}

// A subagent's transcript lives in a directory beside its parent's. It is part
// of that session, not a session of its own.
func TestSubagentTranscriptsAreNotSessions(t *testing.T) {
	h := newHome(t)
	h.transcript("-projects-mono", "parent", userRecord("do it", time.Now()))
	subagents := filepath.Join(h.path, "projects", "-projects-mono", "parent", "subagents")
	if err := os.MkdirAll(subagents, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(subagents, "agent-abc.jsonl"),
		[]byte("{\"type\":\"user\"}\n"),
		0o644,
	); err != nil {
		t.Fatalf("write: %v", err)
	}

	sessions, err := h.adapter().Discover(context.Background(), 0)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(sessions) != 1 || sessions[0].ID != "parent" {
		t.Fatalf("sessions = %#v, want only the parent session", sessions)
	}
}

func TestResumeUsesTheSessionID(t *testing.T) {
	name, args := (&Adapter{}).ResumeCommand(agent.Session{ID: "abc-123"})
	if name != "claude" || strings.Join(args, " ") != "--resume abc-123" {
		t.Fatalf("resume = %q %v", name, args)
	}
}

// Claude Code has no archive verb, and its only way of removing a session
// destroys the transcript.
func TestArchiveIsRefusedRatherThanMappedOntoDeletion(t *testing.T) {
	err := (&Adapter{}).Archive(context.Background(), agent.Session{ID: "abc"})
	if err == nil {
		t.Fatal("Archive() accepted a session Claude cannot archive")
	}
}

func TestDiscoverReportsAMissingStore(t *testing.T) {
	adapter, err := New(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := adapter.Discover(context.Background(), 0); err == nil {
		t.Fatal("Discover() reported no error for a store that is not there")
	}
}
