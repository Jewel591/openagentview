package terminal

import "testing"

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

func TestAppleScriptStringEscapesQuotesAndBackslashes(t *testing.T) {
	if got, want := appleScriptString(`say "hi" \ bye`), `"say \"hi\" \\ bye"`; got != want {
		t.Fatalf("appleScriptString = %q, want %q", got, want)
	}
}
