package prefs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMissingFileIsTheDefaults(t *testing.T) {
	store, err := OpenAt(filepath.Join(t.TempDir(), "prefs.json"))
	if err != nil {
		t.Fatalf("OpenAt: %v", err)
	}
	got := store.Load()
	if got.Group != "" || got.Layout != "" || len(got.Agents) != 0 {
		t.Fatalf("Load = %+v, want the zero defaults", got)
	}
}

func TestArrangementSurvivesReopening(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prefs.json")
	store, err := OpenAt(path)
	if err != nil {
		t.Fatalf("OpenAt: %v", err)
	}
	saved := Prefs{Group: "projects", Layout: "list", Agents: []string{"claude", "codex"}}
	if err := store.Save(saved); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reopened, err := OpenAt(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got := reopened.Load()
	if got.Group != saved.Group || got.Layout != saved.Layout {
		t.Fatalf("Load = %+v, want %+v", got, saved)
	}
	if len(got.Agents) != 2 || got.Agents[0] != "claude" || got.Agents[1] != "codex" {
		t.Fatalf("Agents = %v, want %v", got.Agents, saved.Agents)
	}
}

func TestCorruptFileIsAnErrorRatherThanAnOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prefs.json")
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenAt(path); err == nil {
		t.Fatal("a corrupt store opened without an error")
	}
}
