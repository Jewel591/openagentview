package grok

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/Jewel591/openagentview/internal/agent"
	"github.com/Jewel591/openagentview/internal/tail"
)

type Adapter struct {
	home string
	// previews and events hold the tails of the session the Quick Look overlay
	// is showing, so its once-a-second poll costs only what was written since
	// the last one.
	previews tail.Reader
	events   tail.Reader
}

func New(home string) (*Adapter, error) {
	if home == "" {
		home = os.Getenv("GROK_HOME")
	}
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		home = filepath.Join(userHome, ".grok")
	}
	return &Adapter{home: home}, nil
}

func (a *Adapter) Name() string {
	return "grok"
}

// sessionSummary is the subset of a session's summary.json that the board
// needs. Grok writes it continuously, so it is current even for a live
// session. Titles, git fields and session_kind are absent on sessions too
// short to have been summarized.
type sessionSummary struct {
	Info struct {
		ID  string `json:"id"`
		CWD string `json:"cwd"`
	} `json:"info"`
	SessionSummary string    `json:"session_summary"`
	GeneratedTitle string    `json:"generated_title"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	HeadBranch     string    `json:"head_branch"`
	AgentName      string    `json:"agent_name"`
	SessionKind    string    `json:"session_kind"`
	CurrentModelID string    `json:"current_model_id"`
}

// sessionSignals holds the running totals grok keeps beside each session.
type sessionSignals struct {
	ContextTokensUsed int64 `json:"contextTokensUsed"`
}

type activeSession struct {
	SessionID string `json:"session_id"`
	PID       int    `json:"pid"`
}

type sessionDir struct {
	path     string
	cwdDir   string
	modified time.Time
}

func (a *Adapter) Discover(ctx context.Context, limit int) ([]agent.Session, error) {
	dirs, err := a.sessionDirs()
	if err != nil {
		return nil, err
	}
	sort.Slice(dirs, func(i, j int) bool {
		return dirs[i].modified.After(dirs[j].modified)
	})
	if limit > 0 && len(dirs) > limit {
		dirs = dirs[:limit]
	}
	active := a.activeSessions()

	titles := newTitleCache()
	sessions := make([]agent.Session, 0, len(dirs))
	for _, dir := range dirs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		session, ok := a.readSession(ctx, dir, active, titles)
		if !ok {
			continue
		}
		sessions = append(sessions, session)
	}
	return sessions, nil
}

func (a *Adapter) ResumeCommand(s agent.Session) (string, []string) {
	return "grok", []string{"--resume", s.ID}
}

// NewSessionCommand opens a fresh interactive session around an opening
// prompt, which the CLI takes as a positional argument.
func (a *Adapter) NewSessionCommand(prompt string) (string, []string) {
	return "grok", []string{prompt}
}

// Archive is unsupported: grok's only lifecycle verb is `sessions delete`,
// which destroys the transcript instead of shelving it. Deleting on behalf of
// an "archive" keystroke would be an unrecoverable surprise.
func (a *Adapter) Archive(context.Context, agent.Session) error {
	return errors.New(
		"grok has no archive; `grok sessions delete <id>` deletes permanently",
	)
}

func (a *Adapter) sessionsRoot() string {
	return filepath.Join(a.home, "sessions")
}

// sessionDirs lists every <encoded-cwd>/<session-id> directory. The encoded
// directories also hold loose files such as prompt_history.jsonl, which are
// skipped.
func (a *Adapter) sessionDirs() ([]sessionDir, error) {
	root := a.sessionsRoot()
	workspaces, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read Grok sessions directory: %w", err)
	}

	var dirs []sessionDir
	for _, workspace := range workspaces {
		if !workspace.IsDir() {
			continue
		}
		cwdDir := filepath.Join(root, workspace.Name())
		entries, err := os.ReadDir(cwdDir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			dirs = append(dirs, sessionDir{
				path:     filepath.Join(cwdDir, entry.Name()),
				cwdDir:   cwdDir,
				modified: info.ModTime(),
			})
		}
	}
	return dirs, nil
}

func (a *Adapter) readSession(
	ctx context.Context,
	dir sessionDir,
	active map[string]int,
	titles *titleCache,
) (agent.Session, bool) {
	var summary sessionSummary
	if err := readJSON(filepath.Join(dir.path, "summary.json"), &summary); err != nil {
		return agent.Session{}, false
	}
	id := summary.Info.ID
	if id == "" {
		id = filepath.Base(dir.path)
	}

	session := agent.Session{
		ID:            id,
		Agent:         a.Name(),
		Title:         titles.titleFor(dir, id, summary),
		Preview:       summary.SessionSummary,
		CWD:           summary.Info.CWD,
		Branch:        summary.HeadBranch,
		Source:        fallback(summary.AgentName, summary.CurrentModelID),
		RolloutPath:   dir.path,
		CreatedAt:     summary.CreatedAt,
		UpdatedAt:     summary.UpdatedAt,
		RecencyAt:     summary.UpdatedAt,
		RuntimeStatus: agent.StatusIdle,
	}
	if session.CWD == "" {
		session.CWD = decodeWorkspace(dir.cwdDir)
	}
	if session.UpdatedAt.IsZero() {
		session.UpdatedAt = dir.modified
		session.RecencyAt = dir.modified
	}
	if summary.SessionKind != "" {
		session.AgentNickname = summary.AgentName
		session.AgentRole = summary.SessionKind
	}

	var signals sessionSignals
	if err := readJSON(filepath.Join(dir.path, "signals.json"), &signals); err == nil {
		session.TokensUsed = signals.ContextTokensUsed
	}

	if pid, ok := active[id]; ok && processAlive(pid) {
		session.PID = pid
		if scan, err := readEvents(ctx, dir.path); err == nil {
			session.RuntimeStatus = scan.status()
		} else {
			session.RuntimeStatus = agent.StatusError
		}
	}
	return session, true
}

// activeSessions maps session id to the pid holding it open. Grok removes
// entries on exit, but a killed process leaves its entry behind, so callers
// still confirm the pid.
func (a *Adapter) activeSessions() map[string]int {
	var entries []activeSession
	if err := readJSON(filepath.Join(a.home, "active_sessions.json"), &entries); err != nil {
		return map[string]int{}
	}
	result := make(map[string]int, len(entries))
	for _, entry := range entries {
		if entry.SessionID != "" && entry.PID > 0 {
			result[entry.SessionID] = entry.PID
		}
	}
	return result
}

// titleCache resolves the titles of sessions that ended before grok summarized
// them, by reading the per-workspace prompt log at most once.
type titleCache struct {
	prompts map[string]map[string]string
}

func newTitleCache() *titleCache {
	return &titleCache{prompts: map[string]map[string]string{}}
}

func (c *titleCache) titleFor(
	dir sessionDir,
	id string,
	summary sessionSummary,
) string {
	if title := fallback(summary.GeneratedTitle, summary.SessionSummary); title != "" {
		return title
	}
	if prompt := c.firstPrompt(dir.cwdDir, id); prompt != "" {
		return prompt
	}
	return "Untitled session"
}

func (c *titleCache) firstPrompt(cwdDir, id string) string {
	prompts, ok := c.prompts[cwdDir]
	if !ok {
		prompts = readPromptHistory(filepath.Join(cwdDir, "prompt_history.jsonl"))
		c.prompts[cwdDir] = prompts
	}
	return prompts[id]
}

func readPromptHistory(path string) map[string]string {
	file, err := os.Open(path)
	if err != nil {
		return map[string]string{}
	}
	defer file.Close()

	result := map[string]string{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var entry struct {
			SessionID string `json:"session_id"`
			Prompt    string `json:"prompt"`
		}
		if json.Unmarshal(scanner.Bytes(), &entry) != nil {
			continue
		}
		if entry.SessionID == "" || entry.Prompt == "" {
			continue
		}
		if _, seen := result[entry.SessionID]; !seen {
			result[entry.SessionID] = firstLine(entry.Prompt)
		}
	}
	return result
}

func firstLine(value string) string {
	for i, r := range value {
		if r == '\n' {
			return value[:i]
		}
	}
	return value
}

// decodeWorkspace recovers the workspace path from the percent-encoded
// directory name grok uses for it.
func decodeWorkspace(cwdDir string) string {
	name := filepath.Base(cwdDir)
	decoded, err := decodePercent(name)
	if err != nil {
		return name
	}
	return decoded
}

func decodePercent(value string) (string, error) {
	out := make([]byte, 0, len(value))
	for i := 0; i < len(value); i++ {
		if value[i] != '%' {
			out = append(out, value[i])
			continue
		}
		if i+2 >= len(value) {
			return "", errors.New("truncated percent escape")
		}
		high, err := hexValue(value[i+1])
		if err != nil {
			return "", err
		}
		low, err := hexValue(value[i+2])
		if err != nil {
			return "", err
		}
		out = append(out, high<<4|low)
		i += 2
	}
	return string(out), nil
}

func hexValue(c byte) (byte, error) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', nil
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, nil
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, nil
	default:
		return 0, fmt.Errorf("invalid percent escape %q", c)
	}
}

func readJSON(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

func fallback(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
