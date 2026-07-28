package terminal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestShellLineChangesDirectoryThenBecomesTheCommand(t *testing.T) {
	got := shellLine("/projects/My App", []string{"claude", "--resume", "abc"})
	want := `cd '/projects/My App' && exec claude --resume abc`
	if got != want {
		t.Fatalf("shellLine = %q, want %q", got, want)
	}
}

func TestShellLineKeepsTmuxSeparatorsAsWords(t *testing.T) {
	got := shellLine("", []string{
		"tmux", "select-window", "-t", "%7", ";", "attach", "-t", "%7",
	})
	want := `exec tmux select-window -t %7 ';' attach -t %7`
	if got != want {
		t.Fatalf("shellLine = %q, want %q", got, want)
	}
}

func TestShellQuoteSplicesSingleQuotes(t *testing.T) {
	if got, want := shellQuote("it's"), `'it'\''s'`; got != want {
		t.Fatalf("shellQuote = %q, want %q", got, want)
	}
	if got := shellQuote(""); got != "''" {
		t.Fatalf("shellQuote of empty = %q, want ''", got)
	}
}

func TestDetectRequiresKittyRemoteControl(t *testing.T) {
	fake := t.TempDir()
	kitty := filepath.Join(fake, "kitty")
	t.Setenv("KITTY_WINDOW_ID", "1")
	t.Setenv("TERM_PROGRAM", "")
	t.Setenv("PATH", fake)

	// A kitty that refuses remote control is a terminal the board cannot
	// ask, so detection must say so instead of promising tabs that fail.
	if err := os.WriteFile(kitty, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if Detect() != nil {
		t.Fatal("a kitty with remote control off was detected as tab-capable")
	}

	if err := os.WriteFile(kitty, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if Detect() == nil {
		t.Fatal("a kitty answering remote control was not detected")
	}
}

func TestAppleScriptStringEscapesQuotesAndBackslashes(t *testing.T) {
	if got, want := appleScriptString(`say "hi" \ bye`), `"say \"hi\" \\ bye"`; got != want {
		t.Fatalf("appleScriptString = %q, want %q", got, want)
	}
}
