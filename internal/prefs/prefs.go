// Package prefs remembers how the board was arranged: the grouping, the
// layout, and which agent chips were lit. These are choices about this board,
// not facts about any session, so they live in openagentview's own state
// directory next to the dismissals — and unlike a session they are worth
// nothing to lose, so every change is written as it happens and a store that
// cannot be opened just means a board that starts from its defaults.
package prefs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Prefs is the arrangement worth keeping between runs. Zero values are the
// board's own defaults, so an empty file and a missing one read the same.
type Prefs struct {
	// Group is "status" or "projects".
	Group string `json:"group,omitempty"`
	// Layout is "kanban" or "list".
	Layout string `json:"layout,omitempty"`
	// Agents holds the lowercase names of the lit filter chips; empty means
	// every agent shows.
	Agents []string `json:"agents,omitempty"`
}

// Store reads the preferences once and rewrites the whole file on every
// change — the file is a handful of strings, and rewriting it whole keeps it
// plain enough to hand-edit.
type Store struct {
	path  string
	prefs Prefs
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
	return OpenAt(filepath.Join(stateHome, "openagentview", "prefs.json"))
}

// OpenAt loads the store from an explicit path. A missing file is the
// defaults; an unreadable or unparseable one is an error, because the next
// save would write over whatever the file still says.
func OpenAt(path string) (*Store, error) {
	store := &Store{path: path}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &store.prefs); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return store, nil
}

// Load is the arrangement the file held when the store was opened.
func (s *Store) Load() Prefs {
	return s.prefs
}

// Save persists the arrangement. The store only remembers what was actually
// written, so what it reports never disagrees with what the next start will
// read back.
func (s *Store) Save(p Prefs) error {
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	// Written beside and renamed over, so a crash mid-write cannot leave the
	// file half of each arrangement.
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.prefs = p
	return nil
}
