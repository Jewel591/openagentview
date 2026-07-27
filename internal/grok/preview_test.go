package grok

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jewel591/openagentview/internal/agent"
)

func writeSessionFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestPreviewJoinsStreamedChunksIntoWholeMessages(t *testing.T) {
	dir := writeSessionFiles(t, map[string]string{
		"updates.jsonl": strings.Join([]string{
			`{"timestamp":1785143666,"params":{"update":{"sessionUpdate":"user_message_chunk","content":{"text":"review this"}}}}`,
			`{"timestamp":1785143668,"params":{"update":{"sessionUpdate":"agent_thought_chunk","content":{"text":"thinking out loud"}}}}`,
			`{"timestamp":1785143668,"params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"text":"Reading "}}}}`,
			`{"timestamp":1785143668,"params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"text":"the diff."}}}}`,
			`{"timestamp":1785143669,"params":{"update":{"sessionUpdate":"tool_call","title":"read_file"}}}`,
			`{"timestamp":1785143670,"params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"text":"No findings."}}}}`,
		}, "\n") + "\n",
	})

	adapter := &Adapter{}
	transcript, err := adapter.Preview(
		context.Background(),
		agent.Session{RolloutPath: dir},
		16,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []agent.TranscriptMessage{
		{Role: agent.RoleUser, Text: "review this"},
		{Role: agent.RoleAgent, Text: "Reading the diff."},
		{Role: agent.RoleAgent, Text: "No findings."},
	}
	if len(transcript.Messages) != len(want) {
		t.Fatalf("messages = %#v, want %d", transcript.Messages, len(want))
	}
	for i, message := range transcript.Messages {
		if message.Role != want[i].Role || message.Text != want[i].Text {
			t.Fatalf("message %d = %q/%q, want %q/%q",
				i, message.Role, message.Text, want[i].Role, want[i].Text)
		}
	}
	if transcript.Messages[0].Timestamp.IsZero() {
		t.Fatal("message timestamp is zero, want the chunk's time")
	}
}

func TestPreviewSkipsContextGrokHidesFromItsOwnScrollback(t *testing.T) {
	dir := writeSessionFiles(t, map[string]string{
		"updates.jsonl": strings.Join([]string{
			`{"timestamp":1785143666,"params":{"update":{"sessionUpdate":"user_message_chunk","content":{"text":"review this"}}}}`,
			`{"timestamp":1785143667,"params":{"update":{"sessionUpdate":"user_message_chunk","content":{"text":"<system-reminder>background task done</system-reminder>"},"_meta":{"hideFromScrollback":true}}}}`,
			`{"timestamp":1785143668,"params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"text":"Done."}}}}`,
		}, "\n") + "\n",
	})

	adapter := &Adapter{}
	transcript, err := adapter.Preview(
		context.Background(),
		agent.Session{RolloutPath: dir},
		16,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript.Messages) != 2 {
		t.Fatalf("messages = %#v, want the injected reminder dropped", transcript.Messages)
	}
	for _, message := range transcript.Messages {
		if strings.Contains(message.Text, "system-reminder") {
			t.Fatalf("injected context leaked into the preview: %q", message.Text)
		}
	}
}

func TestPreviewKeepsOnlyTheMostRecentMessages(t *testing.T) {
	var lines []string
	for i := range 20 {
		lines = append(lines, `{"timestamp":1785143666,"params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"text":"message `+string(rune('a'+i))+`"}}}}`)
		lines = append(lines, `{"timestamp":1785143666,"params":{"update":{"sessionUpdate":"tool_call","title":"read_file"}}}`)
	}
	dir := writeSessionFiles(t, map[string]string{
		"updates.jsonl": strings.Join(lines, "\n") + "\n",
	})

	adapter := &Adapter{}
	transcript, err := adapter.Preview(
		context.Background(),
		agent.Session{RolloutPath: dir},
		3,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(transcript.Messages))
	}
	if got := transcript.Messages[2].Text; got != "message t" {
		t.Fatalf("last message = %q, want the newest", got)
	}
}

func TestPreviewReportsAnOpenTurnAsRunningWork(t *testing.T) {
	dir := writeSessionFiles(t, map[string]string{
		"updates.jsonl": `{"timestamp":1785143666,"params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"text":"working"}}}}` + "\n",
		"events.jsonl": strings.Join([]string{
			`{"ts":"2026-07-27T08:00:00.000Z","type":"turn_started"}`,
			`{"ts":"2026-07-27T08:00:01.000Z","type":"tool_started","tool_name":"grep"}`,
			`{"ts":"2026-07-27T08:00:02.000Z","type":"phase_changed","phase":"streaming_reasoning"}`,
		}, "\n") + "\n",
	})

	adapter := &Adapter{}
	transcript, err := adapter.Preview(
		context.Background(),
		agent.Session{RolloutPath: dir},
		16,
	)
	if err != nil {
		t.Fatal(err)
	}
	if transcript.Status != agent.StatusRunning {
		t.Fatalf("status = %q, want %q", transcript.Status, agent.StatusRunning)
	}
	if transcript.Activity.Label != "thinking" {
		t.Fatalf("activity = %q, want thinking", transcript.Activity.Label)
	}
	if transcript.Activity.At.IsZero() {
		t.Fatal("activity timestamp is zero")
	}
}

func TestPreviewReportsAnUnresolvedPermissionPromptAsNeedingYou(t *testing.T) {
	dir := writeSessionFiles(t, map[string]string{
		"updates.jsonl": "",
		"events.jsonl": strings.Join([]string{
			`{"ts":"2026-07-27T08:00:00.000Z","type":"turn_started"}`,
			`{"ts":"2026-07-27T08:00:01.000Z","type":"permission_requested","tool_name":"bash"}`,
		}, "\n") + "\n",
	})

	adapter := &Adapter{}
	transcript, err := adapter.Preview(
		context.Background(),
		agent.Session{RolloutPath: dir},
		16,
	)
	if err != nil {
		t.Fatal(err)
	}
	if transcript.Status != agent.StatusNeedsYou {
		t.Fatalf("status = %q, want %q", transcript.Status, agent.StatusNeedsYou)
	}
	if transcript.Activity.Label != "waiting for approval" {
		t.Fatalf("activity = %q", transcript.Activity.Label)
	}
}

func TestPreviewReportsAFinishedTurnAsIdle(t *testing.T) {
	dir := writeSessionFiles(t, map[string]string{
		"updates.jsonl": "",
		"events.jsonl": strings.Join([]string{
			`{"ts":"2026-07-27T08:00:00.000Z","type":"turn_started"}`,
			`{"ts":"2026-07-27T08:00:01.000Z","type":"permission_requested","tool_name":"bash"}`,
			`{"ts":"2026-07-27T08:00:01.100Z","type":"permission_resolved","tool_name":"bash","decision":"allow"}`,
			`{"ts":"2026-07-27T08:00:09.000Z","type":"turn_ended","outcome":"completed"}`,
		}, "\n") + "\n",
	})

	adapter := &Adapter{}
	transcript, err := adapter.Preview(
		context.Background(),
		agent.Session{RolloutPath: dir},
		16,
	)
	if err != nil {
		t.Fatal(err)
	}
	if transcript.Status != agent.StatusIdle {
		t.Fatalf("status = %q, want %q", transcript.Status, agent.StatusIdle)
	}
	if transcript.Activity.Label != "" {
		t.Fatalf("activity = %q, want empty once the turn ends", transcript.Activity.Label)
	}
}

func TestPreviewStillReturnsMessagesWithoutAnEventLog(t *testing.T) {
	dir := writeSessionFiles(t, map[string]string{
		"updates.jsonl": `{"timestamp":1785143666,"params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"text":"answer"}}}}` + "\n",
	})

	adapter := &Adapter{}
	transcript, err := adapter.Preview(
		context.Background(),
		agent.Session{RolloutPath: dir},
		16,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript.Messages) != 1 {
		t.Fatalf("messages = %#v, want the conversation despite the missing log", transcript.Messages)
	}
	if transcript.Status != "" {
		t.Fatalf("status = %q, want it left unset", transcript.Status)
	}
}

// Grok writes a reply chunk by chunk, so a poll usually lands mid-message. The
// half-written reply must keep growing across polls instead of being restarted
// or dropped.
func TestPreviewGrowsTheReplyBeingStreamedAcrossPolls(t *testing.T) {
	dir := writeSessionFiles(t, map[string]string{
		"updates.jsonl": `{"timestamp":1785143666,"params":{"update":{"sessionUpdate":"user_message_chunk","content":{"text":"summarize"}}}}` + "\n" +
			`{"timestamp":1785143668,"params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"text":"The "}}}}` + "\n",
	})
	adapter := &Adapter{}
	session := agent.Session{RolloutPath: dir}

	first, err := adapter.Preview(context.Background(), session, 16)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Messages) != 2 || first.Messages[1].Text != "The" {
		t.Fatalf("first poll = %#v, want the reply so far", first.Messages)
	}

	path := filepath.Join(dir, "updates.jsonl")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(
		`{"timestamp":1785143669,"params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"text":"answer is 42."}}}}` + "\n" +
			`{"timestamp":1785143670,"params":{"update":{"sessionUpdate":"turn_completed"}}}` + "\n",
	); err != nil {
		t.Fatal(err)
	}
	file.Close()

	second, err := adapter.Preview(context.Background(), session, 16)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Messages) != 2 {
		t.Fatalf("second poll = %#v, want the same two messages", second.Messages)
	}
	if second.Messages[1].Text != "The answer is 42." {
		t.Fatalf("reply = %q, want the whole streamed message",
			second.Messages[1].Text)
	}
}
