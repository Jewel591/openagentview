package claude

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Jewel591/openagentview/internal/agent"
	"github.com/Jewel591/openagentview/internal/tail"
)

const previewMessageRunes = 12_000

// transcriptEntry is one record of a Claude Code transcript, read for what a
// reader would see: the turns, and what the agent has been doing since the last
// one it can read.
type transcriptEntry struct {
	Type        string `json:"type"`
	Timestamp   string `json:"timestamp"`
	IsMeta      bool   `json:"isMeta"`
	IsSidechain bool   `json:"isSidechain"`
	Message     struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
	Name string `json:"name"`
}

// tailScan folds a transcript into the conversation and the work in progress.
// It is resumable: a poll a moment later folds in only what was appended.
type tailScan struct {
	limit    int
	messages []agent.TranscriptMessage
	activity agent.Activity
}

func (s *tailScan) Consume(buffer []byte, startsMidLine bool) int64 {
	return tail.Lines(buffer, startsMidLine, s.consumeLine)
}

func (s *tailScan) Complete() bool {
	return len(s.messages) >= s.limit
}

func (s *tailScan) consumeLine(line []byte) {
	var record transcriptEntry
	if json.Unmarshal(line, &record) != nil {
		return
	}
	// Sidechain records are a subagent's own conversation, folded into the same
	// file. They are not what the person in this session is reading.
	if record.IsSidechain || record.IsMeta {
		return
	}
	timestamp := parseTime(record.Timestamp)

	switch record.Type {
	case "user":
		// A user record carrying tool results is the transcript feeding the
		// agent its own output back, not somebody typing.
		text := contentText(record.Message.Content)
		if text == "" {
			return
		}
		s.append(agent.TranscriptMessage{
			Role:      agent.RoleUser,
			Text:      limitRunes(text, previewMessageRunes),
			Timestamp: timestamp,
		})
		s.activity = agent.Activity{}
	case "assistant":
		s.consumeAssistant(record, timestamp)
	}
}

// consumeAssistant splits an assistant turn into the part a reader can read and
// the part that only says the agent is still working. A long turn is mostly the
// latter: thinking and tool calls, minutes of them, with no message at the end
// until it is done.
func (s *tailScan) consumeAssistant(record transcriptEntry, timestamp time.Time) {
	var blocks []contentBlock
	if json.Unmarshal(record.Message.Content, &blocks) != nil {
		return
	}
	var texts []string
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if text := strings.TrimSpace(block.Text); text != "" {
				texts = append(texts, text)
			}
		case "thinking":
			s.activity = agent.Activity{Label: "thinking", At: timestamp}
		case "tool_use":
			label := "working"
			if block.Name != "" {
				label = "calling " + block.Name
			}
			s.activity = agent.Activity{Label: label, At: timestamp}
		}
	}
	if len(texts) == 0 {
		return
	}
	s.append(agent.TranscriptMessage{
		Role:      agent.RoleAgent,
		Text:      limitRunes(strings.Join(texts, "\n\n"), previewMessageRunes),
		Timestamp: timestamp,
	})
	s.activity = agent.Activity{}
}

// append keeps only the messages that can still be shown, so scanning a large
// transcript holds a screenful rather than the whole conversation.
func (s *tailScan) append(message agent.TranscriptMessage) {
	s.messages = append(s.messages, message)
	if s.limit > 0 && len(s.messages) > s.limit {
		s.messages = append(s.messages[:0], s.messages[len(s.messages)-s.limit:]...)
	}
}

func (s *tailScan) transcript() []agent.TranscriptMessage {
	return append([]agent.TranscriptMessage(nil), s.messages...)
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
	scan := scanner.(*tailScan)
	transcript := agent.Transcript{
		Messages: scan.transcript(),
		Status:   agent.StatusIdle,
		Activity: scan.activity,
	}

	// Status comes from Claude Code's own registry rather than from the shape of
	// the log: it publishes what it is doing, so there is nothing to infer and
	// nothing to get wrong while a turn runs long.
	if live, ok := a.liveSessions()[session.ID]; ok {
		transcript.Status = runtimeStatus(live.Status)
		if live.WaitingFor != "" {
			transcript.Activity = agent.Activity{
				Label: live.WaitingFor,
				At:    time.UnixMilli(live.StatusUpdatedAt),
			}
		}
	}
	return transcript, nil
}

func limitRunes(value string, limit int) string {
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit]) + "\n… message truncated"
}
