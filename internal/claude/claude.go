// Package claude reads Claude Code's local session store.
//
// Claude Code keeps two halves of the picture in different places: every
// session's transcript is a JSONL file under projects/<encoded-cwd>/, and every
// *running* session registers itself in sessions/<pid>.json with the status it
// is in right now. The registry is what makes a Claude session's state cheap and
// exact — it says "busy", "idle" or "waiting", so nothing has to be inferred
// from the shape of the log.
package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Jewel591/openagentview/internal/agent"
	"github.com/Jewel591/openagentview/internal/tail"
)

const (
	// A session's opening records — the first prompt, its workspace and branch —
	// are at the head of the transcript, and its title is rewritten near the end.
	// Both are read in bounded windows: transcripts reach tens of megabytes, and
	// discovery runs every couple of seconds.
	headWindow = 64 << 10
	tailWindow = 96 << 10
)

type Adapter struct {
	home string

	// meta caches what was parsed out of each transcript, keyed by path and
	// invalidated by size and mtime. Discovery re-runs every couple of seconds
	// over every session ever recorded, while at most a handful of them are
	// being written to.
	mu   sync.Mutex
	meta map[string]cachedMeta

	// previews holds the tail of the session Quick Look is showing, so its poll
	// costs only what was appended since the last one.
	previews tail.Reader
}

func New(home string) (*Adapter, error) {
	if home == "" {
		home = os.Getenv("CLAUDE_CONFIG_DIR")
	}
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		home = filepath.Join(userHome, ".claude")
	}
	return &Adapter{home: home, meta: map[string]cachedMeta{}}, nil
}

func (a *Adapter) Name() string {
	return "claude"
}

// liveSession is sessions/<pid>.json: what Claude Code publishes about a
// session while it is running.
type liveSession struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
	Version   string `json:"version"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	// WaitingFor says what a waiting session wants, which is how a permission
	// prompt is told apart from a finished turn.
	WaitingFor      string `json:"waitingFor"`
	StartedAt       int64  `json:"startedAt"`
	UpdatedAt       int64  `json:"updatedAt"`
	StatusUpdatedAt int64  `json:"statusUpdatedAt"`
}

// meta is what one transcript says about itself.
type meta struct {
	FirstPrompt string
	Title       string
	CWD         string
	Branch      string
	Version     string
	CreatedAt   time.Time
	Tokens      int64
}

type cachedMeta struct {
	meta    meta
	size    int64
	modTime time.Time
}

type transcriptFile struct {
	path      string
	sessionID string
	project   string
	size      int64
	modTime   time.Time
}

func (a *Adapter) Discover(ctx context.Context, limit int) ([]agent.Session, error) {
	files, err := a.transcripts()
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime.After(files[j].modTime)
	})
	if limit > 0 && len(files) > limit {
		files = files[:limit]
	}
	live := a.liveSessions()

	sessions := make([]agent.Session, 0, len(files))
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		sessions = append(sessions, a.session(file, live[file.sessionID]))
	}
	return sessions, nil
}

func (a *Adapter) session(file transcriptFile, live *liveSession) agent.Session {
	info := a.metaFor(file)
	session := agent.Session{
		ID:            file.sessionID,
		Agent:         a.Name(),
		Title:         fallback(info.Title, info.FirstPrompt, "Untitled session"),
		Preview:       info.FirstPrompt,
		CWD:           fallback(info.CWD, decodeProject(file.project)),
		Branch:        info.Branch,
		Source:        info.Version,
		RolloutPath:   file.path,
		CreatedAt:     info.CreatedAt,
		UpdatedAt:     file.modTime,
		RecencyAt:     file.modTime,
		TokensUsed:    info.Tokens,
		RuntimeStatus: agent.StatusIdle,
	}
	if live == nil {
		return session
	}
	session.PID = live.PID
	session.RuntimeStatus = runtimeStatus(live.Status)
	if live.Name != "" {
		// Claude Code derives a short handle per session and shows it in its own
		// window title, so it is the name a reader already associates with the
		// terminal this session is in.
		session.AgentNickname = live.Name
		session.AgentRole = live.Kind
	}
	if live.CWD != "" {
		session.CWD = live.CWD
	}
	return session
}

// runtimeStatus maps Claude Code's own words for what a session is doing.
// "waiting" is the state a permission prompt leaves it in, which is exactly
// what the board's Needs You column is for.
func runtimeStatus(status string) agent.RuntimeStatus {
	switch status {
	case "busy":
		return agent.StatusRunning
	case "waiting":
		return agent.StatusNeedsYou
	case "idle":
		return agent.StatusIdle
	default:
		return agent.StatusIdle
	}
}

func (a *Adapter) ResumeCommand(s agent.Session) (string, []string) {
	return "claude", []string{"--resume", s.ID}
}

// NewSessionCommand opens a fresh interactive session around an opening
// prompt, which the CLI takes as a positional argument.
func (a *Adapter) NewSessionCommand(prompt string) (string, []string) {
	return "claude", []string{prompt}
}

func (a *Adapter) projectsRoot() string {
	return filepath.Join(a.home, "projects")
}

func (a *Adapter) sessionsRoot() string {
	return filepath.Join(a.home, "sessions")
}

func (a *Adapter) transcripts() ([]transcriptFile, error) {
	root := a.projectsRoot()
	projects, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read Claude projects directory: %w", err)
	}

	var files []transcriptFile
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		dir := filepath.Join(root, project.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			files = append(files, transcriptFile{
				path:      filepath.Join(dir, name),
				sessionID: strings.TrimSuffix(name, ".jsonl"),
				project:   project.Name(),
				size:      info.Size(),
				modTime:   info.ModTime(),
			})
		}
	}
	return files, nil
}

// liveSessions reads the registry of running sessions. Claude Code removes an
// entry when it exits, but a killed process leaves one behind, so every pid is
// confirmed before it is believed.
func (a *Adapter) liveSessions() map[string]*liveSession {
	entries, err := os.ReadDir(a.sessionsRoot())
	if err != nil {
		return map[string]*liveSession{}
	}
	result := make(map[string]*liveSession, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var live liveSession
		if readJSON(filepath.Join(a.sessionsRoot(), entry.Name()), &live) != nil {
			continue
		}
		if live.SessionID == "" || !processAlive(live.PID) {
			continue
		}
		// Two entries for one session means a stale file outlived a resumed
		// session; the newer one describes the process that is actually running.
		if existing, ok := result[live.SessionID]; ok && existing.UpdatedAt > live.UpdatedAt {
			continue
		}
		result[live.SessionID] = &live
	}
	return result
}

func (a *Adapter) metaFor(file transcriptFile) meta {
	a.mu.Lock()
	cached, ok := a.meta[file.path]
	a.mu.Unlock()
	if ok && cached.size == file.size && cached.modTime.Equal(file.modTime) {
		return cached.meta
	}

	info := readMeta(file.path, file.size)
	a.mu.Lock()
	a.meta[file.path] = cachedMeta{meta: info, size: file.size, modTime: file.modTime}
	a.mu.Unlock()
	return info
}

// readMeta pulls a session's description out of the two ends of its transcript:
// what it opened with, and what it last called itself.
func readMeta(path string, size int64) meta {
	file, err := os.Open(path)
	if err != nil {
		return meta{}
	}
	defer file.Close()

	var info meta
	head := readWindow(file, 0, min64(size, headWindow))
	eachLine(head, false, func(line []byte) { info.readHead(line) })

	if size > headWindow {
		offset := max64(headWindow, size-tailWindow)
		tailBytes := readWindow(file, offset, size-offset)
		eachLine(tailBytes, offset > 0, func(line []byte) { info.readTail(line) })
	} else {
		eachLine(head, false, func(line []byte) { info.readTail(line) })
	}
	return info
}

// entry is the subset of a transcript record the board reads. Claude Code
// writes many record types into one file; the ones not named here are bookkeeping
// the board has no use for.
type entry struct {
	Type        string          `json:"type"`
	AITitle     string          `json:"aiTitle"`
	CWD         string          `json:"cwd"`
	GitBranch   string          `json:"gitBranch"`
	Version     string          `json:"version"`
	Timestamp   string          `json:"timestamp"`
	IsMeta      bool            `json:"isMeta"`
	IsSidechain bool            `json:"isSidechain"`
	Message     json.RawMessage `json:"message"`
}

func (m *meta) readHead(line []byte) {
	var record entry
	if json.Unmarshal(line, &record) != nil {
		return
	}
	if m.CWD == "" {
		m.CWD = record.CWD
	}
	if m.Branch == "" {
		m.Branch = record.GitBranch
	}
	if m.Version == "" {
		m.Version = record.Version
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = parseTime(record.Timestamp)
	}
	if m.FirstPrompt != "" || record.Type != "user" ||
		record.IsMeta || record.IsSidechain {
		return
	}
	// Only a human turn opens a session. A user record carrying tool results is
	// the transcript's way of feeding the agent its own output back.
	if text := userText(record.Message); text != "" {
		m.FirstPrompt = firstLine(text)
	}
}

func (m *meta) readTail(line []byte) {
	var record entry
	if json.Unmarshal(line, &record) != nil {
		return
	}
	switch record.Type {
	case "ai-title":
		// Claude Code rewrites the title as a session goes on, so the last one
		// in the file is the one it would show.
		if record.AITitle != "" {
			m.Title = record.AITitle
		}
	case "assistant":
		if tokens := contextTokens(record.Message); tokens > 0 {
			m.Tokens = tokens
		}
	}
}

// contextTokens reports how much context the last turn carried, which is the
// number worth showing: totalling every turn would count the same cached
// conversation once per message.
func contextTokens(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var message struct {
		Usage struct {
			InputTokens         int64 `json:"input_tokens"`
			OutputTokens        int64 `json:"output_tokens"`
			CacheReadTokens     int64 `json:"cache_read_input_tokens"`
			CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(raw, &message) != nil {
		return 0
	}
	usage := message.Usage
	return usage.InputTokens + usage.OutputTokens +
		usage.CacheReadTokens + usage.CacheCreationTokens
}

// userText returns the human text of a user record. Content is a plain string
// when a person typed it and a block list when it carries tool results.
func userText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var message struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &message) != nil {
		return ""
	}
	return contentText(message.Content)
}

// contentText reads the human-readable text out of a message body. A person's
// turn is stored as a bare string; a block list is the transcript feeding the
// agent its own tool output back, and only its text blocks are anyone's words.
func contentText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(content, &text) == nil {
		return humanText(text)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, block := range blocks {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			parts = append(parts, strings.TrimSpace(block.Text))
		}
	}
	return humanText(strings.Join(parts, "\n"))
}

// humanText turns a user record into what the person actually typed. Claude
// Code stores several things as user turns that nobody typed: the output of a
// slash command, and the reminders it injects into a prompt. A board showing
// those as somebody's words is showing the transcript's plumbing.
func humanText(text string) string {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "<local-command-stdout>") {
		return ""
	}
	if name, ok := tagContent(text, "command-name"); ok {
		// A slash command is a real turn, and reads as the person typed it.
		args, _ := tagContent(text, "command-args")
		return strings.TrimSpace(name + " " + args)
	}
	for {
		start := strings.Index(text, "<system-reminder>")
		if start < 0 {
			break
		}
		end := strings.Index(text[start:], "</system-reminder>")
		if end < 0 {
			text = text[:start]
			break
		}
		text = text[:start] + text[start+end+len("</system-reminder>"):]
	}
	return strings.TrimSpace(text)
}

func tagContent(text, tag string) (string, bool) {
	open, close := "<"+tag+">", "</"+tag+">"
	start := strings.Index(text, open)
	if start < 0 {
		return "", false
	}
	rest := text[start+len(open):]
	end := strings.Index(rest, close)
	if end < 0 {
		return "", false
	}
	return strings.TrimSpace(rest[:end]), true
}

func readWindow(file *os.File, offset, length int64) []byte {
	if length <= 0 {
		return nil
	}
	buffer := make([]byte, length)
	n, err := file.ReadAt(buffer, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil
	}
	return buffer[:n]
}

func eachLine(buffer []byte, startsMidLine bool, visit func([]byte)) {
	tail.Lines(buffer, startsMidLine, visit)
	// tail.Lines stops at the last newline; a window ending mid-record is the
	// normal case here and that fragment is deliberately dropped.
}

// decodeProject recovers a workspace path from the directory name Claude Code
// encodes it into. The encoding replaces every separator with a dash and is
// lossy — a path with a dash in it cannot be told apart from one with a
// separator — so it is only a fallback for transcripts that never recorded cwd.
func decodeProject(name string) string {
	if !strings.HasPrefix(name, "-") {
		return name
	}
	return "/" + strings.ReplaceAll(strings.TrimPrefix(name, "-"), "-", "/")
}

func parseTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func firstLine(value string) string {
	if index := strings.IndexByte(value, '\n'); index >= 0 {
		return strings.TrimSpace(value[:index])
	}
	return value
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

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
