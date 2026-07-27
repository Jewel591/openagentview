// Package dismiss remembers which sessions the board has been asked to stop
// showing. A dismissal is a fact about this board, not about any agent, so it
// lives in openagentview's own state file: the agents' stores stay read-only,
// and an agent without an archive verb still gets a way off the board.
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
	store := &Store{path: path, entries: map[string]time.Time{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &store.entries); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return store, nil
}

// Dismissed reports whether a session has been dismissed.
func (s *Store) Dismissed(agentName, sessionID string) bool {
	_, ok := s.entries[Key(agentName, sessionID)]
	return ok
}

// Dismiss records a session as dismissed and persists the change.
func (s *Store) Dismiss(agentName, sessionID string) error {
	s.entries[Key(agentName, sessionID)] = time.Now().UTC()
	return s.save()
}

// Prune drops the entries of sessions that are gone, keyed by Key, so the
// file never outgrows the sessions that exist. An absent session must also
// have been dismissed since before the cutoff: discovery can hide a session
// without it being gone — a filtered run, an agent that failed to answer —
// and an entry dropped on such a run would put the session back on the board.
func (s *Store) Prune(present map[string]bool, cutoff time.Time) error {
	changed := false
	for key, dismissedAt := range s.entries {
		if !present[key] && dismissedAt.Before(cutoff) {
			delete(s.entries, key)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.save()
}

func (s *Store) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return err
	}
	// Written beside the file and renamed over it, so a crash mid-write
	// cannot leave a half-written store behind.
	temp := s.path + ".tmp"
	if err := os.WriteFile(temp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(temp, s.path)
}
