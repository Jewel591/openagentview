// Package dismiss remembers which sessions the board has been asked to stop
// showing. A dismissal is a fact about this board, not about any agent, so it
// lives in openagentview's own state file: the agents' stores stay read-only,
// and every agent gets the same way off the board.
package dismiss

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Key names one session across every agent. Session IDs are only unique
// within the agent that issued them, so the agent's name is part of the key.
func Key(agentName, sessionID string) string {
	return agentName + "/" + sessionID
}

// Store is the set of dismissed sessions, persisted as one JSON file mapping
// each session's key to when it was dismissed — small enough to hold in
// memory whole and rewrite on every change, and plain enough to hand-edit if
// a dismissal ever needs taking back.
type Store struct {
	path    string
	entries map[string]time.Time
}

// Open loads the store from its default location under $XDG_STATE_HOME,
// falling back to ~/.local/state.
func Open() (*Store, error) {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		stateHome = filepath.Join(home, ".local", "state")
	}
	return OpenAt(filepath.Join(stateHome, "openagentview", "dismissed.json"))
}

// OpenAt loads the store from an explicit path. A missing file is an empty
// store; an unreadable or unparseable one is an error, because the next save
// would write over whatever the file still says.
func OpenAt(path string) (*Store, error) {
	entries, err := readEntries(path)
	if err != nil {
		return nil, err
	}
	return &Store{path: path, entries: entries}, nil
}

// Dismissed reports whether a session has been dismissed.
func (s *Store) Dismissed(agentName, sessionID string) bool {
	_, ok := s.entries[Key(agentName, sessionID)]
	return ok
}

// Dismiss records a session as dismissed and persists the change. The store
// only remembers what was actually written: a dismissal that could not be
// saved is not held in memory either, so what the board shows never disagrees
// with what the next start will read back.
func (s *Store) Dismiss(agentName, sessionID string) error {
	next := make(map[string]time.Time, len(s.entries)+1)
	for key, dismissedAt := range s.entries {
		next[key] = dismissedAt
	}
	next[Key(agentName, sessionID)] = time.Now().UTC()
	merged, err := s.persist(next, nil)
	if err != nil {
		return err
	}
	s.entries = merged
	return nil
}

// Prune drops the entries of sessions that are gone, keyed by Key, so the
// file never outgrows the sessions that exist. An absent session must also
// have been dismissed since before the cutoff: discovery can hide a session
// without it being gone — an agent that failed to answer, a filtered run —
// and an entry dropped on such a run would put the session back on the board.
func (s *Store) Prune(present map[string]bool, cutoff time.Time) error {
	pruned := map[string]bool{}
	next := make(map[string]time.Time, len(s.entries))
	for key, dismissedAt := range s.entries {
		if !present[key] && dismissedAt.Before(cutoff) {
			pruned[key] = true
			continue
		}
		next[key] = dismissedAt
	}
	if len(pruned) == 0 {
		return nil
	}
	merged, err := s.persist(next, pruned)
	if err != nil {
		return err
	}
	s.entries = merged
	return nil
}

// persist writes the entries out and returns what was written. The file is
// re-read and merged first, because another instance of the board may have
// saved dismissals since this one loaded: every board watching its own
// project directory is ordinary use, and a plain rewrite would throw away
// whatever the others recorded. Keys this call deliberately pruned are the
// one thing not merged back.
func (s *Store) persist(
	entries map[string]time.Time,
	pruned map[string]bool,
) (map[string]time.Time, error) {
	onDisk, err := readEntries(s.path)
	if err != nil {
		return nil, err
	}
	for key, dismissedAt := range onDisk {
		if pruned[key] {
			continue
		}
		if current, ok := entries[key]; !ok || dismissedAt.After(current) {
			entries[key] = dismissedAt
		}
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return nil, err
	}
	// Written beside the file under a unique name and renamed over it, so a
	// crash mid-write cannot leave a half-written store, and two instances
	// saving at once cannot hand each other a half-written temp file.
	temp, err := os.CreateTemp(filepath.Dir(s.path), ".dismissed-*.tmp")
	if err != nil {
		return nil, err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		os.Remove(temp.Name())
		return nil, err
	}
	if err := temp.Close(); err != nil {
		os.Remove(temp.Name())
		return nil, err
	}
	if err := os.Rename(temp.Name(), s.path); err != nil {
		os.Remove(temp.Name())
		return nil, err
	}
	return entries, nil
}

func readEntries(path string) (map[string]time.Time, error) {
	entries := map[string]time.Time{}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return entries, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return entries, nil
}
