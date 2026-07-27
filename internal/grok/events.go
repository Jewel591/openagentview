package grok

import (
	"context"
	"encoding/json"
	"path/filepath"
	"time"

	"github.com/Jewel591/openagentview/internal/agent"
	"github.com/Jewel591/openagentview/internal/tail"
)

const eventsInitialBytes = 256 * 1024

// eventLine is the subset of ~/.grok/sessions/<cwd>/<id>/events.jsonl that
// describes what a session is doing. Grok records turn boundaries, permission
// prompts and streaming phases here; the conversation itself lives in
// updates.jsonl.
type eventLine struct {
	TS       string `json:"ts"`
	Type     string `json:"type"`
	Phase    string `json:"phase"`
	ToolName string `json:"tool_name"`
	Outcome  string `json:"outcome"`
}

// eventScan is the runtime state of a session as its event log tells it. It is
// resumable, so watching a live session folds in only the events since the last
// look.
type eventScan struct {
	activity          agent.Activity
	active            bool
	pendingPermission bool
	foundBoundary     bool
}

func (s *eventScan) Consume(buffer []byte, startsMidLine bool) int64 {
	return tail.Lines(buffer, startsMidLine, s.consumeLine)
}

// Complete stops a cold scan at the first turn boundary, which is all it takes
// to know whether a turn is open.
func (s *eventScan) Complete() bool {
	return s.foundBoundary
}

func (s *eventScan) consumeLine(line []byte) {
	var event eventLine
	if json.Unmarshal(line, &event) != nil {
		return
	}
	timestamp, _ := time.Parse(time.RFC3339Nano, event.TS)

	switch event.Type {
	case "turn_started":
		s.foundBoundary = true
		s.active = true
		s.pendingPermission = false
		s.activity = agent.Activity{Label: "starting turn", At: timestamp}
	case "turn_ended":
		s.foundBoundary = true
		s.active = false
		s.pendingPermission = false
		s.activity = agent.Activity{}
	case "permission_requested":
		s.pendingPermission = true
		s.activity = agent.Activity{
			Label: "waiting for approval",
			At:    timestamp,
		}
	case "permission_resolved":
		s.pendingPermission = false
	case "tool_started":
		label := "running a tool"
		if event.ToolName != "" {
			label = "running " + event.ToolName
		}
		s.activity = agent.Activity{Label: label, At: timestamp}
	case "phase_changed":
		if label := phaseLabel(event.Phase); label != "" {
			s.activity = agent.Activity{Label: label, At: timestamp}
		}
	}
}

func (s *eventScan) status() agent.RuntimeStatus {
	switch {
	case s.pendingPermission && s.active:
		return agent.StatusNeedsYou
	case s.active:
		return agent.StatusRunning
	default:
		return agent.StatusIdle
	}
}

func eventsPath(sessionDir string) string {
	return filepath.Join(sessionDir, "events.jsonl")
}

// scanEvents keeps the previewed session's event log open across polls.
func (a *Adapter) scanEvents(
	ctx context.Context,
	sessionDir string,
) (*eventScan, error) {
	scanner, err := a.events.Scan(
		ctx,
		eventsPath(sessionDir),
		0,
		func() tail.Scanner { return &eventScan{} },
	)
	if err != nil {
		return nil, err
	}
	return scanner.(*eventScan), nil
}

// readEvents reads one session's event log without keeping it open, which is
// what discovery needs as it sweeps every live session in turn.
func readEvents(ctx context.Context, sessionDir string) (*eventScan, error) {
	scanner, err := tail.ReadTail(
		ctx,
		eventsPath(sessionDir),
		eventsInitialBytes,
		tail.MaxWindow,
		func() tail.Scanner { return &eventScan{} },
	)
	if err != nil {
		return nil, err
	}
	return scanner.(*eventScan), nil
}

func phaseLabel(phase string) string {
	switch phase {
	case "waiting_for_model":
		return "waiting for the model"
	case "streaming_reasoning":
		return "thinking"
	case "streaming_text":
		return "writing a reply"
	case "tool_execution":
		return "running a tool"
	case "permission_prompt":
		return "waiting for approval"
	default:
		return ""
	}
}
