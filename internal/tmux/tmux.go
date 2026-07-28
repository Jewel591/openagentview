// Package tmux resolves the pane a running agent lives in, and reads and
// writes that pane without attaching a client to it. A pane is the only place
// where an agent's real screen exists: its rollout log records what the agent
// wrote, but not the prompt it is currently blocked on.
package tmux

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// unit separator: tmux passes it through a format string untouched, and no
// pane title or working directory contains one.
const fieldSeparator = "\x1f"

const paneFormat = "#{pane_id}" + fieldSeparator +
	"#{pane_pid}" + fieldSeparator +
	"#{session_name}" + fieldSeparator +
	"#{window_index}" + fieldSeparator +
	"#{pane_index}" + fieldSeparator +
	"#{window_name}" + fieldSeparator +
	"#{pane_width}" + fieldSeparator +
	"#{pane_height}" + fieldSeparator +
	"#{pane_current_command}" + fieldSeparator +
	"#{pane_current_path}"

// Pane is one tmux pane. ID is stable for the life of the tmux server but not
// across restarts, so it is resolved fresh on every discovery scan rather than
// stored anywhere durable.
type Pane struct {
	ID         string
	PID        int
	Session    string
	WindowName string
	// Target addresses the pane the way a person would type it, "cw:2.0".
	Target  string
	Width   int
	Height  int
	Command string
	Path    string
}

// Client talks to one tmux server. The socket is only ever set by tests, which
// run against a throwaway server so they cannot disturb a real one.
type Client struct {
	socket string
}

func New() *Client {
	return &Client{}
}

func NewWithSocket(socket string) *Client {
	return &Client{socket: socket}
}

// Args returns the argv for a tmux invocation, including the socket flags a
// caller would otherwise forget. Exported because resuming a session runs tmux
// in the foreground rather than through this package.
func (c *Client) Args(args ...string) []string {
	if c.socket == "" {
		return args
	}
	return append([]string{"-L", c.socket}, args...)
}

func (c *Client) command(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "tmux", c.Args(args...)...)
}

// Running reports whether a tmux server is up. Every other call fails while it
// is not, and that failure is the normal state of a machine with no agents
// running rather than something worth reporting as an error.
func (c *Client) Running(ctx context.Context) bool {
	if _, err := exec.LookPath("tmux"); err != nil {
		return false
	}
	return c.command(ctx, "list-panes", "-a", "-F", "#{pane_id}").Run() == nil
}

func (c *Client) ListPanes(ctx context.Context) ([]Pane, error) {
	output, err := c.command(ctx, "list-panes", "-a", "-F", paneFormat).Output()
	if err != nil {
		return nil, err
	}
	return parsePanes(string(output)), nil
}

func parsePanes(output string) []Pane {
	var panes []Pane
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), fieldSeparator)
		if len(fields) < 10 {
			continue
		}
		pid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		width, _ := strconv.Atoi(fields[6])
		height, _ := strconv.Atoi(fields[7])
		panes = append(panes, Pane{
			ID:         fields[0],
			PID:        pid,
			Session:    fields[2],
			WindowName: fields[5],
			Target:     fields[2] + ":" + fields[3] + "." + fields[4],
			Width:      width,
			Height:     height,
			Command:    fields[8],
			Path:       fields[9],
		})
	}
	return panes
}

// Index answers which pane a process is running inside. An agent is never the
// pane's own process — tmux starts a shell there and the agent is one or more
// levels below it — so the answer comes from walking the process tree upwards
// until it reaches a pane.
type Index struct {
	byPanePID map[int]Pane
	parents   map[int]int
}

func (c *Client) NewIndex(ctx context.Context) (*Index, error) {
	panes, err := c.ListPanes(ctx)
	if err != nil {
		return nil, err
	}
	parents, err := processParents(ctx)
	if err != nil {
		return nil, err
	}
	return newIndex(panes, parents), nil
}

func newIndex(panes []Pane, parents map[int]int) *Index {
	byPanePID := make(map[int]Pane, len(panes))
	for _, pane := range panes {
		byPanePID[pane.PID] = pane
	}
	return &Index{byPanePID: byPanePID, parents: parents}
}

// PaneFor walks pid's ancestry to the pane containing it. The walk is bounded
// and remembers where it has been: a corrupt or racing snapshot of the process
// table can contain a cycle, and discovery must not hang on one.
func (ix *Index) PaneFor(pid int) (Pane, bool) {
	if ix == nil || pid <= 0 {
		return Pane{}, false
	}
	seen := make(map[int]bool, 8)
	for current := pid; current > 1 && !seen[current]; current = ix.parents[current] {
		seen[current] = true
		if pane, ok := ix.byPanePID[current]; ok {
			return pane, true
		}
	}
	return Pane{}, false
}

func processParents(ctx context.Context) (map[int]int, error) {
	output, err := exec.CommandContext(ctx, "ps", "-Ao", "pid=,ppid=").Output()
	if err != nil {
		return nil, err
	}
	return parseProcessParents(string(output)), nil
}

func parseProcessParents(output string) map[int]int {
	parents := make(map[int]int, 512)
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		parents[pid] = ppid
	}
	return parents
}

// Screen is a pane's visible content together with its cursor. The cursor is
// not part of the content — it is terminal state, and a capture of the cells
// alone cannot show where the agent is waiting for input.
type Screen struct {
	Lines         []string
	CursorX       int
	CursorY       int
	CursorVisible bool
	// AlternateScreen is true while the pane's application is using the
	// terminal alternate screen. Tmux retains no scrollback for that screen,
	// so mirrors must treat Lines as a current frame rather than a transcript.
	AlternateScreen bool
	// Width and Height are the pane's own size. They are what a mirror has to
	// be measured against: the widest captured line only says how much of the
	// pane is in use right now, and sizing a window to that would resize it
	// every time the agent printed a longer line.
	Width  int
	Height int
	// History is how many rows of scrollback precede the visible screen in
	// Lines. HistorySize is how much scrollback the pane holds in total,
	// whether or not it was captured — the measure of the content that lets a
	// reader keep their place while the pane goes on printing.
	History     int
	HistorySize int
}

const cursorFormat = "#{cursor_x} #{cursor_y} #{cursor_flag} " +
	"#{pane_width} #{pane_height} #{history_size} #{alternate_on}"

// Capture returns the pane's visible screen, one string per row, with the
// pane's own colours intact. This is the screen a person would see after
// attaching, including whatever prompt the agent is waiting on. A positive
// history asks for up to that many rows of scrollback above the screen —
// tmux hands back what the pane actually holds.
//
// The cursor and the cells come back from a single tmux invocation: the mirror
// polls several times a second while someone is typing into it, and a second
// process per poll would double that cost for one line of output.
func (c *Client) Capture(
	ctx context.Context,
	paneID string,
	history int,
) (Screen, error) {
	if paneID == "" {
		return Screen{}, errors.New("tmux: no pane")
	}
	capture := []string{"capture-pane", "-p", "-e", "-t", paneID}
	if history > 0 {
		capture = append(capture, "-S", "-"+strconv.Itoa(history))
	}
	args := append(
		[]string{"display-message", "-p", "-t", paneID, cursorFormat, ";"},
		capture...,
	)
	output, err := c.command(ctx, args...).Output()
	if err != nil {
		return Screen{}, captureError(err)
	}
	screen := parseScreen(string(output))
	if history > 0 {
		screen.History = min(history, screen.HistorySize)
	}
	return screen, nil
}

func parseScreen(output string) Screen {
	text := strings.TrimSuffix(output, "\n")
	if text == "" {
		return Screen{}
	}
	rows := strings.Split(text, "\n")
	screen := Screen{Lines: rows[1:]}
	fields := strings.Fields(rows[0])
	if len(fields) < 3 {
		// Without a metadata line the rest is still a usable screen, so the
		// content is kept and only the cursor and the size are given up.
		return Screen{Lines: rows}
	}
	screen.CursorX, _ = strconv.Atoi(fields[0])
	screen.CursorY, _ = strconv.Atoi(fields[1])
	screen.CursorVisible = fields[2] == "1"
	if len(fields) >= 5 {
		screen.Width, _ = strconv.Atoi(fields[3])
		screen.Height, _ = strconv.Atoi(fields[4])
	}
	if len(fields) >= 6 {
		screen.HistorySize, _ = strconv.Atoi(fields[5])
	}
	if len(fields) >= 7 {
		screen.AlternateScreen = fields[6] == "1"
	}
	return screen
}

// sessionNameAttempts is how many derived names are tried before the name is
// given up on. Past that many collisions the prefix has stopped identifying
// anything, and the task still deserves to start.
const sessionNameAttempts = 10

// NewSession starts command in dir inside a detached session of its own, and
// returns the name tmux gave it. The name is asked back rather than assumed:
// a taken name gets a numbered suffix, because two tasks described the same
// way are still two tasks — and when every suffix is taken too, tmux's own
// numbering names the session rather than the name refusing the task.
// A positive width and height size the detached session — tmux falls back to
// 80×24 otherwise, and the size only governs the session until a client
// attaches and brings a real size along; zero leaves the sizing to tmux.
func (c *Client) NewSession(
	ctx context.Context,
	name, dir string,
	command []string,
	width, height int,
) (string, error) {
	if len(command) == 0 {
		return "", errors.New("tmux: no command")
	}
	name = SessionName(name)
	for attempt := 1; attempt <= sessionNameAttempts; attempt++ {
		candidate := name
		if attempt > 1 {
			candidate = name + "-" + strconv.Itoa(attempt)
		}
		created, err := c.startSession(ctx, candidate, dir, command, width, height)
		if err == nil {
			return created, nil
		}
		if !strings.Contains(err.Error(), "duplicate session") {
			return "", err
		}
	}
	return c.startSession(ctx, "", dir, command, width, height)
}

// startSession is one new-session attempt. An empty name leaves the naming to
// tmux.
func (c *Client) startSession(
	ctx context.Context,
	name, dir string,
	command []string,
	width, height int,
) (string, error) {
	args := []string{"new-session", "-d", "-P", "-F", "#{session_name}"}
	if name != "" {
		args = append(args, "-s", name)
	}
	if dir != "" {
		args = append(args, "-c", dir)
	}
	if width > 0 && height > 0 {
		args = append(args,
			"-x", strconv.Itoa(width),
			"-y", strconv.Itoa(height),
		)
	}
	// The command goes through as one shell word: handed over as several,
	// older tmux joins them with spaces and a prompt argument dissolves into
	// separate words by the time it reaches the agent.
	args = append(args, shellCommand(command))
	output, err := c.command(ctx, args...).Output()
	if err != nil {
		return "", captureError(err)
	}
	return strings.TrimSpace(string(output)), nil
}

// shellCommand renders an argv as one shell command, each word single-quoted,
// so a prompt keeps its spaces, dollars and newlines on the way to the agent.
func shellCommand(command []string) string {
	quoted := make([]string, len(command))
	for i, word := range command {
		quoted[i] = "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}

// sessionNameLimit caps a derived name: it lives in a card's meta line and a
// status bar, both of which a whole task description would flood.
const sessionNameLimit = 24

// SessionName derives a usable tmux session name from free text such as a
// task description. tmux rejects names holding ':' or '.', and whitespace
// would make the name unaddressable without quoting, so runs of all three
// become single dashes.
func SessionName(text string) string {
	var b strings.Builder
	dash := true // leading separators are dropped, not turned into a dash
	runes := 0
	for _, r := range text {
		if runes >= sessionNameLimit {
			break
		}
		switch {
		case r == ':' || r == '.' || r <= ' ' || r == 0x7f:
			if !dash {
				b.WriteRune('-')
				dash = true
				runes++
			}
		default:
			b.WriteRune(r)
			dash = false
			runes++
		}
	}
	name := strings.TrimSuffix(b.String(), "-")
	if name == "" {
		return "task"
	}
	return name
}

// SendText types text into the pane. It is sent literally, so a session name
// that looks like a key ("Enter", "C-c") reaches the agent as characters.
func (c *Client) SendText(ctx context.Context, paneID, text string) error {
	if paneID == "" {
		return errors.New("tmux: no pane")
	}
	if text == "" {
		return nil
	}
	return run(c.command(ctx, "send-keys", "-t", paneID, "-l", text))
}

// SendKey sends one named key ("Enter", "C-c", "Escape"). Keys are always sent
// on their own: agent TUIs treat a paste and the newline that follows it as one
// event and swallow the newline, so a key batched behind text can be lost.
func (c *Client) SendKey(ctx context.Context, paneID, key string) error {
	if paneID == "" {
		return errors.New("tmux: no pane")
	}
	return run(c.command(ctx, "send-keys", "-t", paneID, key))
}

func run(cmd *exec.Cmd) error {
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			return err
		}
		return errors.New(message)
	}
	return nil
}

func captureError(err error) error {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if message := strings.TrimSpace(string(exit.Stderr)); message != "" {
			return errors.New(message)
		}
	}
	return err
}

// Inside reports whether this process is itself running in a tmux pane, which
// decides whether returning to a session means attaching a client or moving the
// one already attached.
func Inside() bool {
	return os.Getenv("TMUX") != ""
}
