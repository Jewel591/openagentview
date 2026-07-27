package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jewel591/openagentview/internal/agent"
)

func TestLatestStateDBUsesHighestVersion(t *testing.T) {
	home := t.TempDir()
	for _, name := range []string{"state_2.sqlite", "state_11.sqlite", "state_5.sqlite"} {
		if err := os.WriteFile(filepath.Join(home, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got, err := latestStateDB(home)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "state_11.sqlite"); got != want {
		t.Fatalf("latestStateDB() = %q, want %q", got, want)
	}
}

func TestRolloutStatusExpandsPastLargeTurnOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	content := `{"type":"event_msg","payload":{"type":"task_started"}}` + "\n"
	content += strings.Repeat(
		`{"type":"response_item","payload":{"type":"function_call_output"}}`+"\n",
		8_000,
	)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := rolloutStatus(context.Background(), path); got != agent.StatusRunning {
		t.Fatalf("rolloutStatus(context.Background(), ) = %q, want %q", got, agent.StatusRunning)
	}
}

func TestRolloutStatus(t *testing.T) {
	tests := []struct {
		name   string
		events []string
		want   agent.RuntimeStatus
	}{
		{
			name: "running",
			events: []string{
				`{"type":"event_msg","payload":{"type":"task_started"}}`,
			},
			want: agent.StatusRunning,
		},
		{
			name: "needs user",
			events: []string{
				`{"type":"event_msg","payload":{"type":"task_started"}}`,
				`{"type":"event_msg","payload":{"type":"request_user_input"}}`,
			},
			want: agent.StatusNeedsYou,
		},
		{
			name: "complete",
			events: []string{
				`{"type":"event_msg","payload":{"type":"task_started"}}`,
				`{"type":"event_msg","payload":{"type":"task_complete"}}`,
			},
			want: agent.StatusIdle,
		},
		{
			name: "aborted",
			events: []string{
				`{"type":"event_msg","payload":{"type":"task_started"}}`,
				`{"type":"event_msg","payload":{"type":"turn_aborted"}}`,
			},
			want: agent.StatusIdle,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rollout.jsonl")
			content := ""
			for _, event := range test.events {
				content += event + "\n"
			}
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := rolloutStatus(context.Background(), path); got != test.want {
				t.Fatalf("rolloutStatus(context.Background(), ) = %q, want %q", got, test.want)
			}
		})
	}
}
