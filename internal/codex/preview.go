package codex

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Jewel591/openagentview/internal/agent"
	"github.com/Jewel591/openagentview/internal/tail"
)

const (
	previewInitialBytes = 512 * 1024
	previewMaxBytes     = 16 * 1024 * 1024
	previewMessageRunes = 12_000
)

type transcriptEvent struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Payload   struct {
		Type      string `json:"type"`
		Message   string `json:"message"`
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"payload"`
}

// tailScan is everything a walk through a rollout can tell us: the visible
// conversation, whether a turn is open, and what the session was doing most
// recently. It is resumable, so a poll one second later folds in only the
// records written since.
type tailScan struct {
	limit         int
	messages      []agent.TranscriptMessage
	activity      agent.Activity
	active        bool
	waiting       bool
	foundBoundary bool
}

func (s *tailScan) Consume(buffer []byte, startsMidLine bool) int64 {
	return tail.Lines(buffer, startsMidLine, s.consumeLine)
}

// Complete stops the cold scan once the window holds a full screen of
// conversation. It deliberately does not wait for a turn boundary: a long turn
// can bury one under megabytes of tool output, and re-reading that on every
// poll costs far more than the status heuristic in transcript() gives back.
func (s *tailScan) Complete() bool {
	return len(s.messages) >= s.limit
}

func (s *tailScan) consumeLine(line []byte) {
	var event transcriptEvent
	if json.Unmarshal(line, &event) != nil {
		return
	}
	timestamp, _ := time.Parse(time.RFC3339Nano, event.Timestamp)

	switch event.Type {
	case "event_msg":
		switch event.Payload.Type {
		case "user_message", "agent_message":
			role := agent.RoleAgent
			if event.Payload.Type == "user_message" {
				role = agent.RoleUser
			}
			text := strings.TrimSpace(event.Payload.Message)
			if text == "" {
				return
			}
			s.append(agent.TranscriptMessage{
				Role:      role,
				Text:      limitRunes(text, previewMessageRunes),
				Timestamp: timestamp,
			})
			s.activity = agent.Activity{}
		case "task_started":
			s.foundBoundary = true
			s.active = true
			s.waiting = false
			s.activity = agent.Activity{Label: "starting turn", At: timestamp}
		case "task_complete", "turn_aborted":
			s.foundBoundary = true
			s.active = false
			s.waiting = false
			s.activity = agent.Activity{}
		case "token_count", "thread_settings_applied":
			// Bookkeeping, not work worth reporting.
		default:
			eventType := strings.ToLower(event.Payload.Type)
			if s.active && (strings.Contains(eventType, "approval") ||
				strings.Contains(eventType, "user_input")) {
				s.waiting = true
			}
			if label := eventActivity(event.Payload.Type); label != "" {
				s.activity = agent.Activity{Label: label, At: timestamp}
			}
		}
	case "response_item":
		if label := responseActivity(event); label != "" {
			s.activity = agent.Activity{Label: label, At: timestamp}
		}
	}
}

// append keeps only the messages that can still be shown, so a scan of a
// hundred-megabyte rollout holds a screenful rather than the whole history.
func (s *tailScan) append(message agent.TranscriptMessage) {
	s.messages = append(s.messages, message)
	if s.limit > 0 && len(s.messages) > s.limit {
		s.messages = append(s.messages[:0], s.messages[len(s.messages)-s.limit:]...)
	}
}

func (s *tailScan) status() agent.RuntimeStatus {
	switch {
	case s.waiting:
		return agent.StatusNeedsYou
	case s.active:
		return agent.StatusRunning
	default:
		return agent.StatusIdle
	}
}

// transcript copies the scan out for the UI, which reads it on another
// goroutine while this scanner keeps being extended.
func (s *tailScan) transcript() agent.Transcript {
	status := s.status()
	if !s.foundBoundary && !s.activity.At.IsZero() {
		// The scan ends at EOF, so a finished turn always leaves its boundary
		// behind it. Seeing none means the turn began further back than we
		// read — an in-progress turn, not an idle session.
		status = agent.StatusRunning
	}
	return agent.Transcript{
		Messages: append([]agent.TranscriptMessage(nil), s.messages...),
		Status:   status,
		Activity: s.activity,
	}
}

// statusScan is what discovery needs from a rollout: nothing but whether a turn
// is open, which is known as soon as a boundary shows up.
type statusScan struct {
	tailScan
}

func (s *statusScan) Complete() bool {
	return s.foundBoundary
}

func (a *Adapter) Preview(
	ctx context.Context,
	session agent.Session,
	limit int,
) (agent.Transcript, error) {
	if limit <= 0 {
		return agent.Transcript{}, nil
	}
	scanner, err := a.previews.Scan(
		ctx,
		session.RolloutPath,
		limit,
		func() tail.Scanner { return &tailScan{limit: limit} },
	)
	if err != nil {
		return agent.Transcript{}, err
	}
	return scanner.(*tailScan).transcript(), nil
}

// eventActivity labels the event_msg records that represent real work.
func eventActivity(payloadType string) string {
	switch payloadType {
	case "patch_apply_begin":
		return "editing files"
	case "patch_apply_end":
		return "edited files"
	case "web_search_begin":
		return "searching the web"
	case "web_search_end":
		return "read search results"
	case "exec_command_begin":
		return "running a command"
	case "exec_command_end":
		return "ran a command"
	case "sub_agent_activity":
		return "delegating to a sub-agent"
	default:
		return ""
	}
}

// responseActivity labels model output records. Tool names are surfaced as-is
// because they are the most specific thing available without decoding each
// tool's private argument format.
func responseActivity(event transcriptEvent) string {
	switch event.Payload.Type {
	case "reasoning":
		return "thinking"
	case "custom_tool_call", "function_call":
		name := event.Payload.Name
		if event.Payload.Namespace != "" {
			name = event.Payload.Namespace + "." + name
		}
		if name == "" {
			return "calling a tool"
		}
		return "calling " + name
	case "custom_tool_call_output", "function_call_output":
		return "reading tool output"
	default:
		return ""
	}
}

func limitRunes(value string, limit int) string {
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit]) + "\n… message truncated"
}
