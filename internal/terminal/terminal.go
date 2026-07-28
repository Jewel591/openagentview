// Package terminal opens a command in a new tab of the terminal emulator the
// board is running in, so going to a session never costs the board its own
// window. Which emulator that is comes from the environment it set up; the
// way to ask it for a tab is per-emulator, since no portable interface
// exists: AppleScript for the macOS apps, a remote-control CLI for the
// emulators that ship one.
package terminal

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// Opener opens commands in new tabs of one recognized terminal emulator.
type Opener struct {
	program string
}

// Detect identifies the surrounding terminal, returning nil when it is not
// one an Opener knows how to ask for a tab — the caller keeps its in-window
// fallback for that case rather than guessing at keystrokes blind.
func Detect() *Opener {
	if os.Getenv("KITTY_WINDOW_ID") != "" {
		// kitty ships with remote control off, and a request against it then
		// fails outright. One probe at detection time decides honestly: a
		// kitty that cannot be asked is a terminal the board does not know
		// how to ask, and the in-window fallback applies.
		if exec.Command("kitty", "@", "ls").Run() == nil {
			return &Opener{program: "kitty"}
		}
		return nil
	}
	program := os.Getenv("TERM_PROGRAM")
	switch program {
	case "WezTerm":
		return &Opener{program: program}
	case "ghostty", "iTerm.app", "Apple_Terminal":
		// These three are driven over AppleScript, which exists nowhere else.
		if runtime.GOOS == "darwin" {
			return &Opener{program: program}
		}
	}
	return nil
}

// OpenTab starts command in a new tab, in dir. It blocks until the terminal
// has taken the request, which is why callers run it off the update loop.
func (o *Opener) OpenTab(dir string, command []string) error {
	switch o.program {
	case "kitty":
		args := []string{"@", "launch", "--type=tab"}
		if dir != "" {
			args = append(args, "--cwd", dir)
		}
		return run("kitty", append(append(args, "--"), command...)...)
	case "WezTerm":
		args := []string{"cli", "spawn"}
		if dir != "" {
			args = append(args, "--cwd", dir)
		}
		return run("wezterm", append(append(args, "--"), command...)...)
	case "ghostty":
		return o.openGhosttyTab(dir, command)
	case "iTerm.app":
		return runScript(fmt.Sprintf(`
tell application "iTerm"
	activate
	if (count of windows) = 0 then
		create window with default profile
	else
		tell current window to create tab with default profile
	end if
	tell current session of current window to write text %s
end tell`, appleScriptString(shellLine(dir, command))))
	case "Apple_Terminal":
		return runScript(fmt.Sprintf(`
tell application "Terminal" to activate
tell application "System Events" to tell process "Terminal" to keystroke "t" using command down
delay 0.4
tell application "Terminal" to do script %s in front window`,
			appleScriptString(shellLine(dir, command))))
	}
	return fmt.Errorf("no way to open a tab in %s", o.program)
}

// openGhosttyTab types the command into a fresh tab, since Ghostty has no
// scripting interface for "new tab running this" yet: cmd+T through System
// Events opens the tab, and the command is keyed in after it. When that is
// refused — automation permission takes one grant, and some setups rebind
// cmd+T — a new Ghostty window running the command is the fallback, which
// macOS folds into a tab for anyone whose system prefers tabs.
func (o *Opener) openGhosttyTab(dir string, command []string) error {
	line := shellLine(dir, command)
	err := runScript(fmt.Sprintf(`
tell application "Ghostty" to activate
delay 0.3
tell application "System Events" to tell process "Ghostty"
	keystroke "t" using command down
	delay 0.5
	keystroke %s
	key code 36
end tell`, appleScriptString(line)))
	if err == nil {
		return nil
	}
	if fallback := run(
		"open", "-na", "Ghostty", "--args", "-e", "/bin/sh", "-lc", line,
	); fallback == nil {
		return nil
	}
	return err
}

// shellLine is the one line a fresh tab's shell runs: land in the session's
// directory, then become the command.
func shellLine(dir string, command []string) string {
	quoted := make([]string, 0, len(command))
	for _, arg := range command {
		quoted = append(quoted, shellQuote(arg))
	}
	line := "exec " + strings.Join(quoted, " ")
	if dir != "" {
		line = "cd " + shellQuote(dir) + " && " + line
	}
	return line
}

// shellQuote wraps one argument for /bin/sh: single quotes, with embedded
// single quotes spliced through as '\”. tmux's command separator ";" must
// stay a word of its own, which quoting preserves.
func shellQuote(arg string) string {
	if arg == "" {
		return "''"
	}
	if !strings.ContainsAny(arg, " \t\n'\"\\$`&|;<>(){}[]*?~#") {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
}

// appleScriptString quotes a Go string as an AppleScript string literal.
func appleScriptString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

func runScript(script string) error {
	return run("osascript", "-e", script)
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return fmt.Errorf("%s: %s", name, detail)
		}
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}
