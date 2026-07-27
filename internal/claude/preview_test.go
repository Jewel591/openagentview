package claude

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Jewel591/openagentview/internal/agent"
)

func assistantRecord(at time.Time, blocks ...map[string]any) map[string]any {
	return map[string]any{
		"type":      "assistant",
		"timestamp": at.Format(time.RFC3339Nano),
		"message": map[string]any{
			"role":    "assistant",
			"content": blocks,
		},
	}
}

func textBlock(text string) map[string]any {
	return map[string]any{"type": "text", "text": text}
}

func toolBlock(name string) map[string]any {
	return map[string]any{"type": "tool_use", "name": name, "input": map[string]any{}}
}

func toolResultRecord(at time.Time) map[string]any {
	return map[string]any{
		"type":      "user",
		"timestamp": at.Format(time.RFC3339Nano),
		"message": map[string]any{
			"role": "user",
			"content": []map[string]any{
				{"type": "tool_result", "tool_use_id": "toolu_1", "content": "ok"},
			},
		},
	}
}

func previewSession(path, id string) agent.Session {
	return agent.Session{ID: id, Agent: "claude", RolloutPath: path}
}

func texts(messages []agent.TranscriptMessage) []string {
	result := make([]string, 0, len(messages))
	for _, message := range messages {
		result = append(result, string(message.Role)+":"+message.Text)
	}
	return result
}

func TestPreviewReadsTheConversationAndSkipsThePlumbing(t *testing.T) {
	h := newHome(t)
	now := time.Now()
	path := h.transcript("-projects-mono", "chat",
		userRecord("fix the tests", now),
		assistantRecord(now, toolBlock("Bash")),
		toolResultRecord(now),
		assistantRecord(now, textBlock("Fixed them.")),
		// A subagent's turns share the file but are not this conversation.
		map[string]any{
			"type":        "assistant",
			"isSidechain": true,
			"message":     map[string]any{"content": []map[string]any{textBlock("subagent talk")}},
		},
		map[string]any{
			"type":    "user",
			"isMeta":  true,
			"message": map[string]any{"role": "user", "content": "injected context"},
		},
	)

	transcript, err := h.adapter().Preview(context.Background(), previewSession(path, "chat"), 16)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	got := strings.Join(texts(transcript.Messages), " | ")
	want := "user:fix the tests | agent:Fixed them."
	if got != want {
		t.Fatalf("messages = %q, want %q", got, want)
	}
}

// A long turn is minutes of tool calls with no message at the end of it until
// it is done, so the tool it is on is the only sign it is still moving.
func TestPreviewReportsTheToolATurnIsOn(t *testing.T) {
	h := newHome(t)
	now := time.Now()
	path := h.transcript("-projects-mono", "working",
		userRecord("do the long thing", now),
		assistantRecord(now, map[string]any{"type": "thinking", "thinking": "…"}),
		assistantRecord(now, toolBlock("Edit")),
	)

	transcript, err := h.adapter().Preview(context.Background(), previewSession(path, "working"), 16)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if transcript.Activity.Label != "calling Edit" {
		t.Fatalf("activity = %q, want the tool the turn is on", transcript.Activity.Label)
	}

	// A message ends the turn, and with it anything worth reporting as activity.
	h.append(path, assistantRecord(now, textBlock("Done.")))
	transcript, err = h.adapter().Preview(context.Background(), previewSession(path, "working"), 16)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if transcript.Activity.Label != "" {
		t.Fatalf("activity = %q, want none once the turn produced a message",
			transcript.Activity.Label)
	}
}

// Status is read from Claude Code's registry rather than inferred from the log,
// so a turn that runs long is never mistaken for a finished one.
func TestPreviewTakesStatusFromTheRegistry(t *testing.T) {
	h := newHome(t)
	now := time.Now()
	path := h.transcript("-projects-mono", "asking", userRecord("go", now))
	h.live(os.Getpid(), map[string]any{
		"sessionId":       "asking",
		"status":          "waiting",
		"waitingFor":      "input needed",
		"statusUpdatedAt": now.UnixMilli(),
	})

	transcript, err := h.adapter().Preview(context.Background(), previewSession(path, "asking"), 16)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if transcript.Status != agent.StatusNeedsYou {
		t.Fatalf("status = %q, want needs-you", transcript.Status)
	}
	if transcript.Activity.Label != "input needed" {
		t.Fatalf("activity = %q, want what the session is waiting for",
			transcript.Activity.Label)
	}
}

func TestPreviewOfASessionThatIsNotRunningIsIdle(t *testing.T) {
	h := newHome(t)
	path := h.transcript("-projects-mono", "over", userRecord("go", time.Now()))

	transcript, err := h.adapter().Preview(context.Background(), previewSession(path, "over"), 16)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if transcript.Status != agent.StatusIdle {
		t.Fatalf("status = %q, want idle", transcript.Status)
	}
}

// The overlay polls while a session is being written to, and each poll must
// cost only what was appended since the last one.
func TestPreviewPicksUpWhatWasAppendedSinceTheLastRead(t *testing.T) {
	h := newHome(t)
	now := time.Now()
	path := h.transcript("-projects-mono", "live", userRecord("first", now))
	adapter := h.adapter()

	transcript, err := adapter.Preview(context.Background(), previewSession(path, "live"), 16)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if len(transcript.Messages) != 1 {
		t.Fatalf("messages = %#v, want the opening turn", texts(transcript.Messages))
	}

	h.append(path, assistantRecord(now, textBlock("second")))
	transcript, err = adapter.Preview(context.Background(), previewSession(path, "live"), 16)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if got := texts(transcript.Messages); len(got) != 2 || got[1] != "agent:second" {
		t.Fatalf("messages = %q, want the appended reply", got)
	}
}

func TestPreviewKeepsOnlyTheMessagesItCanShow(t *testing.T) {
	h := newHome(t)
	now := time.Now()
	records := make([]any, 0, 20)
	for i := range 20 {
		records = append(records, assistantRecord(now, textBlock(strings.Repeat("x", i+1))))
	}
	path := h.transcript("-projects-mono", "long", records...)

	transcript, err := h.adapter().Preview(context.Background(), previewSession(path, "long"), 5)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if len(transcript.Messages) != 5 {
		t.Fatalf("messages = %d, want the last 5", len(transcript.Messages))
	}
	if last := transcript.Messages[4].Text; last != strings.Repeat("x", 20) {
		t.Fatalf("last message = %q, want the newest", last)
	}
}

func TestPreviewOfAMissingTranscriptFails(t *testing.T) {
	h := newHome(t)
	_, err := h.adapter().Preview(
		context.Background(),
		previewSession(h.path+"/absent.jsonl", "absent"),
		16,
	)
	if err == nil {
		t.Fatal("Preview() reported no error for a transcript that is not there")
	}
}
