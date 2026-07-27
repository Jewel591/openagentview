package grok

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Jewel591/openagentview/internal/agent"
	"github.com/Jewel591/openagentview/internal/tail"
)

const previewMessageRunes = 12_000

// updateLine is the subset of updates.jsonl we read. Grok streams a message as
// a run of chunks, so a visible message is the concatenation of consecutive
// chunks of one kind.
type updateLine struct {
	Timestamp int64 `json:"timestamp"`
	Params    struct {
		Update struct {
			SessionUpdate string `json:"sessionUpdate"`
			Content       struct {
				Text string `json:"text"`
			} `json:"content"`
			Meta struct {
				// Grok sets this on the context it injects into the conversation
				// as if the user had typed it — background task results, system
				// reminders — and hides it from its own scrollback.
				HideFromScrollback bool `json:"hideFromScrollback"`
			} `json:"_meta"`
		} `json:"update"`
	} `json:"params"`
	Meta struct {
		AgentTimestampMs int64 `json:"agentTimestampMs"`
	} `json:"_meta"`
}

func (l updateLine) at() time.Time {
	if l.Meta.AgentTimestampMs > 0 {
		return time.UnixMilli(l.Meta.AgentTimestampMs)
	}
	if l.Timestamp > 0 {
		return time.Unix(l.Timestamp, 0)
	}
	return time.Time{}
}

// messageScan assembles streamed chunks into whole messages. The message being
// streamed right now is held apart from the finished ones so that resuming the
// scan a second later appends to it instead of starting a new one.
type messageScan struct {
	limit    int
	messages []agent.TranscriptMessage
	pending  *agent.TranscriptMessage
}

func (s *messageScan) Consume(buffer []byte, startsMidLine bool) int64 {
	return tail.Lines(buffer, startsMidLine, s.consumeLine)
}

func (s *messageScan) Complete() bool {
	return len(s.messages) >= s.limit
}

func (s *messageScan) consumeLine(line []byte) {
	var update updateLine
	if json.Unmarshal(line, &update) != nil {
		return
	}
	var role agent.TranscriptRole
	switch update.Params.Update.SessionUpdate {
	case "user_message_chunk":
		role = agent.RoleUser
	case "agent_message_chunk":
		role = agent.RoleAgent
	default:
		// Anything else — a tool call, a hook, the end of a turn — ends the run
		// of chunks that was being assembled.
		s.flush()
		return
	}
	if update.Params.Update.Meta.HideFromScrollback {
		s.flush()
		return
	}
	if s.pending != nil && s.pending.Role != role {
		s.flush()
	}
	if s.pending == nil {
		s.pending = &agent.TranscriptMessage{Role: role, Timestamp: update.at()}
	}
	s.pending.Text += update.Params.Update.Content.Text
}

func (s *messageScan) flush() {
	if s.pending == nil {
		return
	}
	if text := strings.TrimSpace(s.pending.Text); text != "" {
		s.pending.Text = limitRunes(text, previewMessageRunes)
		s.append(*s.pending)
	}
	s.pending = nil
}

// append keeps only the messages that can still be shown, so scanning a large
// log holds a screenful rather than the whole conversation.
func (s *messageScan) append(message agent.TranscriptMessage) {
	s.messages = append(s.messages, message)
	if s.limit > 0 && len(s.messages) > s.limit {
		s.messages = append(s.messages[:0], s.messages[len(s.messages)-s.limit:]...)
	}
}

// transcript copies the scan out for the UI, which reads it on another
// goroutine while this scanner keeps being extended. The message still being
// streamed is included, since a reply arriving word by word is exactly what a
// live preview is for.
func (s *messageScan) transcript() []agent.TranscriptMessage {
	messages := append([]agent.TranscriptMessage(nil), s.messages...)
	if s.pending != nil {
		if text := strings.TrimSpace(s.pending.Text); text != "" {
			messages = append(messages, agent.TranscriptMessage{
				Role:      s.pending.Role,
				Text:      limitRunes(text, previewMessageRunes),
				Timestamp: s.pending.Timestamp,
			})
		}
	}
	if s.limit > 0 && len(messages) > s.limit {
		messages = messages[len(messages)-s.limit:]
	}
	return messages
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
		updatesPath(session.RolloutPath),
		limit,
		func() tail.Scanner { return &messageScan{limit: limit} },
	)
	if err != nil {
		return agent.Transcript{}, err
	}
	transcript := agent.Transcript{
		Messages: scanner.(*messageScan).transcript(),
	}

	// A missing or unreadable event log costs us the live badge, not the
	// conversation, so it is reported as idle rather than as a failure.
	if scan, err := a.scanEvents(ctx, session.RolloutPath); err == nil {
		transcript.Status = scan.status()
		transcript.Activity = scan.activity
	}
	return transcript, nil
}

func updatesPath(sessionDir string) string {
	return filepath.Join(sessionDir, "updates.jsonl")
}

func limitRunes(value string, limit int) string {
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit]) + "\n… message truncated"
}
