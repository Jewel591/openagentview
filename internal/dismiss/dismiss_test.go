package dismiss

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func storeAt(t *testing.T, path string) *Store {
	t.Helper()
	store, err := OpenAt(path)
	if err != nil {
		t.Fatalf("OpenAt: %v", err)
	}
	return store
}

func TestMissingFileOpensAnEmptyStore(t *testing.T) {
	store := storeAt(t, filepath.Join(t.TempDir(), "dismissed.json"))
	if store.Dismissed("codex", "abc") {
		t.Fatal("an empty store reported a session dismissed")
	}
}

func TestDismissalSurvivesReopening(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dismissed.json")
	if err := storeAt(t, path).Dismiss("codex", "abc"); err != nil {
		t.Fatalf("Dismiss: %v", err)
	}
	reopened := storeAt(t, path)
	if !reopened.Dismissed("codex", "abc") {
		t.Fatal("a dismissal did not survive reopening the store")
	}
	if reopened.Dismissed("claude", "abc") {
		t.Fatal("a dismissal leaked across agents sharing a session ID")
	}
}

func TestCorruptFileIsAnErrorRatherThanAnOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dismissed.json")
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenAt(path); err == nil {
		t.Fatal("a corrupt store opened without an error")
	}
}

func TestPruneOnlyDropsSessionsBothGoneAndPastTheCutoff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dismissed.json")
	store := storeAt(t, path)
	for _, id := range []string{"kept-present", "kept-recent", "dropped"} {
		if err := store.Dismiss("codex", id); err != nil {
			t.Fatalf("Dismiss: %v", err)
		}
	}
	store.entries[Key("codex", "kept-present")] = time.Now().Add(-48 * time.Hour)
	store.entries[Key("codex", "dropped")] = time.Now().Add(-48 * time.Hour)

	present := map[string]bool{Key("codex", "kept-present"): true}
	if err := store.Prune(present, time.Now().Add(-24*time.Hour)); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	reopened := storeAt(t, path)
	if !reopened.Dismissed("codex", "kept-present") {
		t.Fatal("pruning dropped a session discovery still returns")
	}
	if !reopened.Dismissed("codex", "kept-recent") {
		t.Fatal("pruning dropped a recent dismissal whose session was merely hidden")
	}
	if reopened.Dismissed("codex", "dropped") {
		t.Fatal("pruning kept a session both gone and past the cutoff")
	}
}
