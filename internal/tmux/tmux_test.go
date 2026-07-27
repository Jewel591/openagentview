package tmux

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParsePanesReadsEveryField(t *testing.T) {
	line := strings.Join([]string{
		"%3", "5589", "cw", "2", "1", "shipley-patrol-claude",
		"120", "40", "codex", "/projects/mono",
	}, fieldSeparator)

	panes := parsePanes(line + "\n")
	if len(panes) != 1 {
		t.Fatalf("panes = %d, want 1", len(panes))
	}
	got := panes[0]
	want := Pane{
		ID:         "%3",
		PID:        5589,
		Session:    "cw",
		WindowName: "shipley-patrol-claude",
		Target:     "cw:2.1",
		Width:      120,
		Height:     40,
		Command:    "codex",
		Path:       "/projects/mono",
	}
	if got != want {
		t.Fatalf("pane = %#v, want %#v", got, want)
	}
}

func TestParsePanesSkipsMalformedRows(t *testing.T) {
	output := "not a pane row\n" +
		strings.Join([]string{"%1", "notapid", "s", "0", "0", "n", "1", "1", "c", "/"}, fieldSeparator) +
		"\n"
	if panes := parsePanes(output); len(panes) != 0 {
		t.Fatalf("panes = %#v, want none", panes)
	}
}

// An agent is never the pane's own process: tmux starts a shell there and the
// agent runs below it, sometimes several levels down.
func TestPaneForFindsThePaneOfADescendantProcess(t *testing.T) {
	panes := []Pane{{ID: "%1", PID: 100}, {ID: "%2", PID: 200}}
	parents := map[int]int{
		999: 320, // codex
		320: 300, // a wrapper script
		300: 200, // the pane's shell
		200: 1,
	}
	index := newIndex(panes, parents)

	pane, ok := index.PaneFor(999)
	if !ok || pane.ID != "%2" {
		t.Fatalf("PaneFor(999) = %#v, %v, want pane %%2", pane, ok)
	}
	if _, ok := index.PaneFor(4242); ok {
		t.Fatal("PaneFor() claimed a pane for a process outside tmux")
	}
	if _, ok := index.PaneFor(0); ok {
		t.Fatal("PaneFor() claimed a pane for an unknown pid")
	}
}

// A snapshot of the process table is taken while processes are exiting, so it
// can contain a loop. Discovery runs once a second and must not hang on one.
func TestPaneForStopsOnACycleInTheProcessTable(t *testing.T) {
	index := newIndex(
		[]Pane{{ID: "%1", PID: 100}},
		map[int]int{5: 6, 6: 7, 7: 5},
	)

	done := make(chan bool, 1)
	go func() {
		_, ok := index.PaneFor(5)
		done <- ok
	}()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("PaneFor() found a pane that does not contain the cycle")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PaneFor() did not terminate on a cyclic process table")
	}
}

func TestParseProcessParents(t *testing.T) {
	parents := parseProcessParents("  5611  5589\n 5589 5387\nheader junk\n")
	if parents[5611] != 5589 || parents[5589] != 5387 {
		t.Fatalf("parents = %#v", parents)
	}
	if len(parents) != 2 {
		t.Fatalf("parents = %#v, want only the two parsable rows", parents)
	}
}

func TestPaneKeyArgsCarryTheSocket(t *testing.T) {
	got := NewWithSocket("oavtest").Args("capture-pane", "-p")
	want := []string{"-L", "oavtest", "capture-pane", "-p"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("Args() = %v, want %v", got, want)
	}
}

// The rest of the package is a thin wrapper over the tmux CLI, and what makes
// it worth wrapping is that the CLI behaves as expected — so it is tested
// against a real server, on a throwaway socket that cannot touch a live one.
func testClient(t *testing.T) *Client {
	t.Helper()
	if testing.Short() {
		t.Skip("real tmux server tests are skipped in -short mode")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	socket := "oav-test-" + strconv.Itoa(os.Getpid()) + "-" + t.Name()
	client := NewWithSocket(socket)
	t.Cleanup(func() {
		_ = client.command(context.Background(), "kill-server").Run()
	})
	return client
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestClientMirrorsAndTypesIntoARealPane(t *testing.T) {
	client := testClient(t)
	ctx := context.Background()

	if client.Running(ctx) {
		t.Fatal("Running() reported a server before one was started")
	}
	start := client.command(ctx, "new-session", "-d", "-s", "t", "-x", "80", "-y", "12")
	if out, err := start.CombinedOutput(); err != nil {
		t.Fatalf("new-session: %v: %s", err, out)
	}
	if !client.Running(ctx) {
		t.Fatal("Running() did not see the server that was just started")
	}

	panes, err := client.ListPanes(ctx)
	if err != nil {
		t.Fatalf("ListPanes: %v", err)
	}
	if len(panes) != 1 {
		t.Fatalf("panes = %#v, want exactly the one just created", panes)
	}
	pane := panes[0]
	if pane.ID == "" || pane.PID == 0 || pane.Target != "t:0.0" {
		t.Fatalf("pane = %#v, want an addressable pane", pane)
	}

	// Typing is what makes the mirror worth having: the agent's own log never
	// records the prompt it is blocked on, let alone the answer.
	if err := client.SendText(ctx, pane.ID, "echo mirrored-input"); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	if err := client.SendKey(ctx, pane.ID, "Enter"); err != nil {
		t.Fatalf("SendKey: %v", err)
	}
	waitFor(t, "the typed command to run", func() bool {
		screen, err := client.Capture(ctx, pane.ID)
		if err != nil {
			return false
		}
		lines := screen.Lines
		// The echoed command line is on screen either way, so the output only
		// counts once it appears on a line of its own.
		for _, line := range lines {
			if strings.TrimSpace(line) == "mirrored-input" {
				return true
			}
		}
		return false
	})

	screen, err := client.Capture(ctx, pane.ID)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	// tmux drops the blank rows below the last written line, so a capture is at
	// most the pane's height and usually shorter — the overlay pads it back out
	// rather than assuming a full screen.
	if len(screen.Lines) == 0 || len(screen.Lines) > 12 {
		t.Fatalf("captured %d lines, want between 1 and the pane's 12 rows",
			len(screen.Lines))
	}
	// The cursor is terminal state rather than cell content, so a capture of
	// the cells alone cannot say where the shell is waiting for input.
	if !screen.CursorVisible {
		t.Fatal("Capture() did not report the pane's visible cursor")
	}
	if screen.CursorY < 0 || screen.CursorY >= 12 || screen.CursorX < 0 {
		t.Fatalf("cursor = %d,%d, want a position inside the pane",
			screen.CursorX, screen.CursorY)
	}

	// A process started inside the pane must resolve back to it, which is the
	// whole basis for matching a running agent to a pane.
	if err := client.SendText(ctx, pane.ID, "sleep 47"); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	if err := client.SendKey(ctx, pane.ID, "Enter"); err != nil {
		t.Fatalf("SendKey: %v", err)
	}
	var childPID int
	waitFor(t, "the child process to start", func() bool {
		out, err := exec.Command("pgrep", "-f", "sleep 47").Output()
		if err != nil {
			return false
		}
		fields := strings.Fields(string(out))
		if len(fields) == 0 {
			return false
		}
		childPID, _ = strconv.Atoi(fields[0])
		return childPID > 0
	})
	index, err := client.NewIndex(ctx)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	found, ok := index.PaneFor(childPID)
	if !ok || found.ID != pane.ID {
		t.Fatalf("PaneFor(child) = %#v, %v, want pane %s", found, ok, pane.ID)
	}
	if _, ok := index.PaneFor(os.Getpid()); ok {
		t.Fatal("PaneFor() placed the test process inside the throwaway server")
	}
	_ = client.SendKey(ctx, pane.ID, "C-c")
}

func TestParseScreenKeepsContentWhenTheCursorLineIsMissing(t *testing.T) {
	screen := parseScreen("7 3 1\nfirst\nsecond\n")
	if len(screen.Lines) != 2 || screen.Lines[0] != "first" {
		t.Fatalf("lines = %#v, want the rows after the cursor line", screen.Lines)
	}
	if screen.CursorX != 7 || screen.CursorY != 3 || !screen.CursorVisible {
		t.Fatalf("cursor = %#v, want 7,3 visible", screen)
	}

	// A cursor tmux would not report must not cost the screen a row.
	screen = parseScreen("only content\n")
	if len(screen.Lines) != 1 || screen.Lines[0] != "only content" {
		t.Fatalf("lines = %#v, want the content kept", screen.Lines)
	}
	if screen.CursorVisible {
		t.Fatal("a screen without a cursor line reported a cursor")
	}
}

func TestCaptureReportsAMissingPane(t *testing.T) {
	client := testClient(t)
	ctx := context.Background()
	if _, err := client.Capture(ctx, ""); err == nil {
		t.Fatal("Capture() accepted an empty pane id")
	}
	start := client.command(ctx, "new-session", "-d", "-s", "t")
	if out, err := start.CombinedOutput(); err != nil {
		t.Fatalf("new-session: %v: %s", err, out)
	}
	_, err := client.Capture(ctx, "%999")
	if err == nil || !strings.Contains(err.Error(), "can't find pane") {
		t.Fatalf("Capture(missing) error = %v, want tmux's own message", err)
	}
}

// The metadata line grew the pane's own size behind the cursor fields; a tmux
// that reports all five must fill the size in, and one that stops at three
// must still yield a usable cursor.
func TestParseScreenReadsThePaneSize(t *testing.T) {
	screen := parseScreen("7 3 1 120 40\nrow\n")
	if screen.Width != 120 || screen.Height != 40 {
		t.Fatalf("size = %dx%d, want 120x40", screen.Width, screen.Height)
	}
	if screen.CursorX != 7 || screen.CursorY != 3 || !screen.CursorVisible {
		t.Fatalf("cursor = %#v, want 7,3 visible", screen)
	}

	screen = parseScreen("7 3 1\nrow\n")
	if screen.Width != 0 || screen.Height != 0 {
		t.Fatalf("three-field metadata invented a size: %#v", screen)
	}
}

// A task description becomes a session name a person can address without
// quoting: tmux rejects ':' and '.', and whitespace would need escaping.
func TestSessionNameMakesFreeTextAddressable(t *testing.T) {
	cases := map[string]string{
		"quick fix: login":                 "quick-fix-login",
		"  spaced   out  ":                 "spaced-out",
		"v2.1 release notes":               "v2-1-release-notes",
		"修复登录问题":                           "修复登录问题",
		"":                                 "task",
		" .:. ":                            "task",
		"a task described at great length": "a-task-described-at-grea",
	}
	for text, want := range cases {
		if got := SessionName(text); got != want {
			t.Errorf("SessionName(%q) = %q, want %q", text, got, want)
		}
	}
}

func TestNewSessionStartsDetachedAndSuffixesDuplicates(t *testing.T) {
	client := testClient(t)
	ctx := context.Background()

	// The command must reach the pane as an argv, not as space-joined words: a
	// prompt with spaces and an apostrophe has to arrive as one argument.
	// The compound command keeps sh alive as the pane's own process, so its
	// argv can be read back; a lone command would be exec'd over it.
	name, err := client.NewSession(ctx, "fix: login", "", []string{
		"sh", "-c", "sleep 47; true # don't split",
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if name != "fix-login" {
		t.Fatalf("session name = %q, want the sanitized one", name)
	}

	var panePID int
	waitFor(t, "the session's command to start", func() bool {
		panes, err := client.ListPanes(ctx)
		if err != nil || len(panes) != 1 {
			return false
		}
		panePID = panes[0].PID
		return panePID > 0
	})
	out, err := exec.Command(
		"ps", "-p", strconv.Itoa(panePID), "-o", "args=",
	).Output()
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	if !strings.Contains(string(out), "don't split") {
		t.Fatalf("pane argv = %q, want the quoted argument intact", out)
	}

	// The same description again is still a new task.
	name, err = client.NewSession(ctx, "fix: login", "", []string{"sleep", "48"})
	if err != nil {
		t.Fatalf("NewSession(duplicate): %v", err)
	}
	if name != "fix-login-2" {
		t.Fatalf("duplicate name = %q, want a numbered suffix", name)
	}

	// Past every numbered suffix the name is given up rather than the task:
	// tmux's own numbering still gets the session started.
	seen := map[string]bool{"fix-login": true, name: true}
	for i := 3; i <= sessionNameAttempts+1; i++ {
		name, err = client.NewSession(ctx, "fix: login", "", []string{"sleep", "49"})
		if err != nil {
			t.Fatalf("NewSession(collision %d): %v", i, err)
		}
		if seen[name] {
			t.Fatalf("collision %d reused session name %q", i, name)
		}
		seen[name] = true
	}
	if strings.HasPrefix(name, "fix-login") {
		t.Fatalf("final name = %q, want tmux's own numbering once suffixes ran out",
			name)
	}
}

func TestNewSessionRefusesAnEmptyCommand(t *testing.T) {
	if _, err := New().NewSession(context.Background(), "x", "", nil); err == nil {
		t.Fatal("NewSession accepted an empty command")
	}
}
