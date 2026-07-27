package agent

import (
	"context"
	"time"
)

type RuntimeStatus string

const (
	StatusRunning  RuntimeStatus = "running"
	StatusNeedsYou RuntimeStatus = "needs-you"
	StatusIdle     RuntimeStatus = "idle"
	StatusError    RuntimeStatus = "error"
	StatusArchived RuntimeStatus = "archived"
)

type TranscriptRole string

const (
	RoleUser  TranscriptRole = "user"
	RoleAgent TranscriptRole = "agent"
)

type TranscriptMessage struct {
	Role      TranscriptRole
	Text      string
	Timestamp time.Time
}

// Activity is the most recent work a session performed after its last visible
// message. Long turns produce tool calls and reasoning for minutes at a time
// without emitting a message, so this is what tells a reader that a quiet
// transcript is still moving.
type Activity struct {
	Label string
	At    time.Time
}

// Transcript is a point-in-time read of the tail of a session's log. Status is
// derived from the log itself rather than from the process table, so a preview
// can stay current without repeating the expensive discovery scan.
type Transcript struct {
	Messages []TranscriptMessage
	Status   RuntimeStatus
	Activity Activity
}

type Session struct {
	ID            string
	Agent         string
	Title         string
	Preview       string
	CWD           string
	Branch        string
	Source        string
	RolloutPath   string
	AgentNickname string
	AgentRole     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	RecencyAt     time.Time
	TokensUsed    int64
	Archived      bool
	RuntimeStatus RuntimeStatus
	PID           int

	// TmuxPane is the pane the session's process is running in, empty when it
	// is not running under tmux. TmuxTarget is the same pane written the way a
	// person addresses it ("cw:2.0"), for display and for returning to it.
	TmuxPane   string
	TmuxTarget string
}

type Adapter interface {
	Name() string
	Discover(context.Context, int) ([]Session, error)
	Preview(context.Context, Session, int) (Transcript, error)
	ResumeCommand(Session) (string, []string)
	Archive(context.Context, Session) error
}
