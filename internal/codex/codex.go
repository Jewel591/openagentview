package codex

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Jewel591/openagentview/internal/agent"
	"github.com/Jewel591/openagentview/internal/tail"
	_ "modernc.org/sqlite"
)

const (
	tailBytes    = 256 * 1024
	maxTailBytes = 16 * 1024 * 1024
)

type Adapter struct {
	home string
	// previews holds the tail of the rollout the Quick Look overlay is showing,
	// so its once-a-second poll costs only the records written since the last
	// one.
	previews tail.Reader
}

func New(home string) (*Adapter, error) {
	if home == "" {
		home = os.Getenv("CODEX_HOME")
	}
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		home = filepath.Join(userHome, ".codex")
	}
	return &Adapter{home: home}, nil
}

func (a *Adapter) Name() string {
	return "codex"
}

func (a *Adapter) Discover(ctx context.Context, limit int) ([]agent.Session, error) {
	dbPath, err := latestStateDB(a.home)
	if err != nil {
		return nil, err
	}
	activePaths := activeRollouts()

	u := url.URL{Scheme: "file", Path: dbPath}
	db, err := sql.Open("sqlite", u.String()+"?mode=ro&_pragma=busy_timeout(2500)&_pragma=query_only(1)")
	if err != nil {
		return nil, fmt.Errorf("open Codex state database: %w", err)
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, `
		SELECT
			id,
			rollout_path,
			COALESCE(created_at_ms, created_at * 1000),
			COALESCE(updated_at_ms, updated_at * 1000),
			COALESCE(NULLIF(recency_at_ms, 0), updated_at_ms, updated_at * 1000),
			cwd,
			COALESCE(NULLIF(name, ''), NULLIF(title, ''), NULLIF(preview, ''), 'Untitled session'),
			COALESCE(preview, ''),
			COALESCE(git_branch, ''),
			COALESCE(source, 'unknown'),
			COALESCE(tokens_used, 0),
			archived,
			COALESCE(agent_nickname, ''),
			COALESCE(agent_role, '')
		FROM threads
		WHERE preview <> '' OR title <> '' OR name <> ''
		ORDER BY archived ASC, recency_at_ms DESC, id DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("query Codex sessions: %w", err)
	}
	defer rows.Close()

	var sessions []agent.Session
	for rows.Next() {
		var (
			s                               agent.Session
			createdMS, updatedMS, recencyMS int64
			archived                        int
		)
		if err := rows.Scan(
			&s.ID,
			&s.RolloutPath,
			&createdMS,
			&updatedMS,
			&recencyMS,
			&s.CWD,
			&s.Title,
			&s.Preview,
			&s.Branch,
			&s.Source,
			&s.TokensUsed,
			&archived,
			&s.AgentNickname,
			&s.AgentRole,
		); err != nil {
			return nil, err
		}
		s.Agent = a.Name()
		s.Archived = archived != 0
		s.CreatedAt = time.UnixMilli(createdMS)
		s.UpdatedAt = time.UnixMilli(updatedMS)
		s.RecencyAt = time.UnixMilli(recencyMS)
		s.RuntimeStatus = agent.StatusIdle

		if s.Archived {
			s.RuntimeStatus = agent.StatusArchived
		} else if pid, ok := activePaths[s.RolloutPath]; ok {
			s.PID = pid
			s.RuntimeStatus = rolloutStatus(ctx, s.RolloutPath)
		}
		sessions = append(sessions, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return sessions, nil
}

func (a *Adapter) ResumeCommand(s agent.Session) (string, []string) {
	return "codex", []string{"resume", s.ID}
}

// NewSessionCommand opens a fresh interactive session around an opening
// prompt, which the CLI takes as a positional argument.
func (a *Adapter) NewSessionCommand(prompt string) (string, []string) {
	return "codex", []string{prompt}
}

func (a *Adapter) Archive(ctx context.Context, s agent.Session) error {
	cmd := exec.CommandContext(ctx, "codex", "archive", s.ID)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("codex archive: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func latestStateDB(home string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(home, "state_*.sqlite"))
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no Codex state database found in %s", home)
	}
	sort.Slice(matches, func(i, j int) bool {
		return stateVersion(matches[i]) > stateVersion(matches[j])
	})
	return matches[0], nil
}

func stateVersion(path string) int {
	base := filepath.Base(path)
	value := strings.TrimSuffix(strings.TrimPrefix(base, "state_"), ".sqlite")
	n, _ := strconv.Atoi(value)
	return n
}

func activeRollouts() map[string]int {
	switch runtime.GOOS {
	case "darwin":
		return activeRolloutsDarwin()
	case "linux":
		return activeRolloutsLinux()
	default:
		return map[string]int{}
	}
}

func activeRolloutsDarwin() map[string]int {
	output, err := exec.Command("lsof", "-n", "-Fpn", "-c", "codex").Output()
	if err != nil {
		return map[string]int{}
	}
	result := make(map[string]int)
	pid := 0
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "p") {
			pid, _ = strconv.Atoi(strings.TrimPrefix(line, "p"))
			continue
		}
		if strings.HasPrefix(line, "n") {
			path := strings.TrimPrefix(line, "n")
			if isRollout(path) {
				result[path] = pid
			}
		}
	}
	return result
}

func activeRolloutsLinux() map[string]int {
	result := make(map[string]int)
	processes, _ := filepath.Glob("/proc/[0-9]*")
	for _, process := range processes {
		pid, _ := strconv.Atoi(filepath.Base(process))
		fds, _ := filepath.Glob(filepath.Join(process, "fd", "*"))
		for _, fd := range fds {
			path, err := os.Readlink(fd)
			if err == nil && isRollout(path) {
				result[path] = pid
			}
		}
	}
	return result
}

func isRollout(path string) bool {
	return strings.Contains(path, string(filepath.Separator)+".codex"+string(filepath.Separator)) &&
		strings.Contains(path, string(filepath.Separator)+"sessions"+string(filepath.Separator)) &&
		strings.HasSuffix(path, ".jsonl")
}

func rolloutStatus(ctx context.Context, path string) agent.RuntimeStatus {
	scanner, err := tail.ReadTail(
		ctx, path, tailBytes, maxTailBytes,
		func() tail.Scanner { return &statusScan{} },
	)
	if err != nil {
		return agent.StatusError
	}
	scan := scanner.(*statusScan)
	if scan.foundBoundary {
		return scan.status()
	}
	// An open rollout with no recent task boundary is more likely to be in a
	// very large turn than idle. Prefer a visible false positive over silently
	// hiding active work.
	return agent.StatusRunning
}
