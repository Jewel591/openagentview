package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jewel591/openagentview/internal/agent"
)

func TestPreviewReturnsRecentConversationMessages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	content := strings.Join([]string{
		`{"timestamp":"2026-07-27T08:00:00Z","type":"event_msg","payload":{"type":"user_message","message":"first question"}}`,
		`{"timestamp":"2026-07-27T08:00:01Z","type":"response_item","payload":{"type":"reasoning"}}`,
		`{"timestamp":"2026-07-27T08:00:02Z","type":"event_msg","payload":{"type":"agent_message","message":"first answer"}}`,
		`{"timestamp":"2026-07-27T08:01:00Z","type":"event_msg","payload":{"type":"user_message","message":"second question"}}`,
		`{"timestamp":"2026-07-27T08:01:02Z","type":"event_msg","payload":{"type":"agent_message","message":"second answer"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	adapter := &Adapter{}
	transcript, err := adapter.Preview(
		context.Background(),
		agent.Session{RolloutPath: path},
		3,
	)
	if err != nil {
		t.Fatal(err)
	}
	messages := transcript.Messages
	if len(messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(messages))
	}
	if messages[0].Text != "first answer" || messages[2].Text != "second answer" {
		t.Fatalf("unexpected messages: %#v", messages)
	}
	if messages[1].Role != agent.RoleUser {
		t.Fatalf("middle role = %q, want user", messages[1].Role)
	}
}

func TestPreviewExpandsPastLargeToolOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	content := `{"timestamp":"2026-07-27T08:00:00Z","type":"event_msg","payload":{"type":"user_message","message":"question"}}` + "\n"
	content += strings.Repeat(
		`{"type":"response_item","payload":{"type":"function_call_output","output":"ok"}}`+"\n",
		10_000,
	)
	content += `{"timestamp":"2026-07-27T08:01:00Z","type":"event_msg","payload":{"type":"agent_message","message":"answer"}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	adapter := &Adapter{}
	transcript, err := adapter.Preview(
		context.Background(),
		agent.Session{RolloutPath: path},
		2,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript.Messages) != 2 || transcript.Messages[0].Text != "question" {
		t.Fatalf("unexpected messages: %#v", transcript.Messages)
	}
}

func TestPreviewReportsWorkHappeningAfterTheLastMessage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	content := strings.Join([]string{
		`{"timestamp":"2026-07-27T08:00:00Z","type":"event_msg","payload":{"type":"task_started"}}`,
		`{"timestamp":"2026-07-27T08:00:01Z","type":"event_msg","payload":{"type":"user_message","message":"go"}}`,
		`{"timestamp":"2026-07-27T08:00:02Z","type":"response_item","payload":{"type":"reasoning"}}`,
		`{"timestamp":"2026-07-27T08:00:03Z","type":"response_item","payload":{"type":"function_call","name":"run","namespace":"web"}}`,
		`{"timestamp":"2026-07-27T08:00:04Z","type":"event_msg","payload":{"type":"token_count"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	adapter := &Adapter{}
	transcript, err := adapter.Preview(
		context.Background(),
		agent.Session{RolloutPath: path},
		16,
	)
	if err != nil {
		t.Fatal(err)
	}
	if transcript.Status != agent.StatusRunning {
		t.Fatalf("status = %q, want %q", transcript.Status, agent.StatusRunning)
	}
	if transcript.Activity.Label != "calling web.run" {
		t.Fatalf("activity = %q, want %q", transcript.Activity.Label, "calling web.run")
	}
	if transcript.Activity.At.IsZero() {
		t.Fatal("activity timestamp is zero, want the time of the tool call")
	}
}

func TestPreviewClearsActivityWhenTurnEnds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	content := strings.Join([]string{
		`{"timestamp":"2026-07-27T08:00:00Z","type":"event_msg","payload":{"type":"task_started"}}`,
		`{"timestamp":"2026-07-27T08:00:01Z","type":"response_item","payload":{"type":"reasoning"}}`,
		`{"timestamp":"2026-07-27T08:00:02Z","type":"event_msg","payload":{"type":"agent_message","message":"done"}}`,
		`{"timestamp":"2026-07-27T08:00:03Z","type":"event_msg","payload":{"type":"task_complete"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	adapter := &Adapter{}
	transcript, err := adapter.Preview(
		context.Background(),
		agent.Session{RolloutPath: path},
		16,
	)
	if err != nil {
		t.Fatal(err)
	}
	if transcript.Status != agent.StatusIdle {
		t.Fatalf("status = %q, want %q", transcript.Status, agent.StatusIdle)
	}
	if transcript.Activity.Label != "" {
		t.Fatalf("activity = %q, want empty after the turn completes", transcript.Activity.Label)
	}
}

// The overlay polls once a second. A poll must see what the session wrote since
// the last one without re-reading the rollout, which for a real session means
// re-reading megabytes.
func TestPreviewPicksUpRecordsWrittenSinceTheLastPoll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join([]string{
		`{"timestamp":"2026-07-27T08:00:00Z","type":"event_msg","payload":{"type":"task_started"}}`,
		`{"timestamp":"2026-07-27T08:00:01Z","type":"event_msg","payload":{"type":"user_message","message":"question"}}`,
	}, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	adapter := &Adapter{}
	first, err := adapter.Preview(context.Background(), agent.Session{RolloutPath: path}, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Messages) != 1 || first.Status != agent.StatusRunning {
		t.Fatalf("first poll = %#v, want one message in an open turn", first)
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(strings.Join([]string{
		`{"timestamp":"2026-07-27T08:00:05Z","type":"response_item","payload":{"type":"reasoning"}}`,
		`{"timestamp":"2026-07-27T08:00:09Z","type":"event_msg","payload":{"type":"agent_message","message":"answer"}}`,
		`{"timestamp":"2026-07-27T08:00:10Z","type":"event_msg","payload":{"type":"task_complete"}}`,
	}, "\n") + "\n"); err != nil {
		t.Fatal(err)
	}
	file.Close()

	second, err := adapter.Preview(context.Background(), agent.Session{RolloutPath: path}, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Messages) != 2 || second.Messages[1].Text != "answer" {
		t.Fatalf("second poll = %#v, want the appended answer", second.Messages)
	}
	if second.Status != agent.StatusIdle {
		t.Fatalf("status = %q, want the finished turn", second.Status)
	}
	if second.Activity.Label != "" {
		t.Fatalf("activity = %q, want it cleared by the completed turn",
			second.Activity.Label)
	}
}

// Trimming happens as the scan runs so a huge rollout does not have to be held
// in memory, which must not cost the caller messages it asked for.
func TestPreviewKeepsTheLimitAcrossPolls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	message := func(n string) string {
		return `{"timestamp":"2026-07-27T08:00:00Z","type":"event_msg","payload":{"type":"agent_message","message":"` + n + `"}}` + "\n"
	}
	if err := os.WriteFile(path, []byte(message("a")+message("b")+message("c")), 0o600); err != nil {
		t.Fatal(err)
	}

	adapter := &Adapter{}
	session := agent.Session{RolloutPath: path}
	if _, err := adapter.Preview(context.Background(), session, 2); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(message("d")); err != nil {
		t.Fatal(err)
	}
	file.Close()

	transcript, err := adapter.Preview(context.Background(), session, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript.Messages) != 2 {
		t.Fatalf("messages = %#v, want 2", transcript.Messages)
	}
	if transcript.Messages[0].Text != "c" || transcript.Messages[1].Text != "d" {
		t.Fatalf("messages = %#v, want the newest two", transcript.Messages)
	}
}
