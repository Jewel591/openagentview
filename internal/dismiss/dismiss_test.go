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

func TestFailedSaveKeepsTheStoreUnchanged(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	store := storeAt(t, filepath.Join(dir, "dismissed.json"))
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	if err := store.Dismiss("codex", "abc"); err == nil {
		t.Fatal("dismissing into a read-only directory reported success")
	}
	if store.Dismissed("codex", "abc") {
		t.Fatal("a dismissal that was never written is still shown as dismissed")
	}
}

func TestStoresSharingTheFileMergeInsteadOfOverwriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dismissed.json")
	first := storeAt(t, path)
	second := storeAt(t, path)

	if err := first.Dismiss("codex", "from-first"); err != nil {
		t.Fatalf("Dismiss: %v", err)
	}
	if err := second.Dismiss("codex", "from-second"); err != nil {
		t.Fatalf("Dismiss: %v", err)
	}

	if !second.Dismissed("codex", "from-first") {
		t.Fatal("saving merged nothing: the second store lost the first's dismissal")
	}
	reopened := storeAt(t, path)
	for _, id := range []string{"from-first", "from-second"} {
		if !reopened.Dismissed("codex", id) {
			t.Fatalf("the file lost %q to a concurrent instance's save", id)
		}
	}
}
