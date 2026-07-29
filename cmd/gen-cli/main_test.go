package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureRouter = `package main

type Command struct {
	Name        string
	Description string
	Aliases     []string
	MinArgs     int
	MaxArgs     int
}

type Router struct {
	Commands map[string]*Command
}

func NewRouter() *Router {
	r := &Router{}
	r.Commands = map[string]*Command{
		"version": {Name: "version", Description: "Show version info", MinArgs: 0, MaxArgs: 0},
		"help":    {Name: "help", Description: "Show this help", MinArgs: 0, MaxArgs: 0},
		"recall":  {Name: "recall", Description: "Search memories", MinArgs: 1, Aliases: []string{"s"}},
		"add":     {Name: "add", Description: "Add memory", MinArgs: 1},
	}
	return r
}
`

func TestExtractCommands(t *testing.T) {
	dir := t.TempDir()
	routerPath := filepath.Join(dir, "router.go")
	if err := os.WriteFile(routerPath, []byte(fixtureRouter), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	entries, err := extractCommands(routerPath)
	if err != nil {
		t.Fatalf("extractCommands: %v", err)
	}

	if len(entries) != 4 {
		t.Fatalf("got %d entries, want 4", len(entries))
	}

	// Sorted by name: add, help, recall, version
	want := []struct {
		name string
		desc string
		als  []string
	}{
		{"add", "Add memory", nil},
		{"help", "Show this help", nil},
		{"recall", "Search memories", []string{"s"}},
		{"version", "Show version info", nil},
	}
	for i, w := range want {
		if entries[i].name != w.name {
			t.Errorf("entry[%d].name = %q, want %q", i, entries[i].name, w.name)
		}
		if entries[i].description != w.desc {
			t.Errorf("entry[%d].description = %q, want %q", i, entries[i].description, w.desc)
		}
		if len(entries[i].aliases) != len(w.als) {
			t.Errorf("entry[%d].aliases len = %d, want %d", i, len(entries[i].aliases), len(w.als))
			continue
		}
		for j, a := range w.als {
			if entries[i].aliases[j] != a {
				t.Errorf("entry[%d].aliases[%d] = %q, want %q", i, j, entries[i].aliases[j], a)
			}
		}
	}
}

func TestRun_SentinelsMissing(t *testing.T) {
	dir := t.TempDir()
	routerPath := filepath.Join(dir, "router.go")
	readmePath := filepath.Join(dir, "README.md")
	if err := os.WriteFile(routerPath, []byte(fixtureRouter), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	// README without sentinels — expect error
	if err := os.WriteFile(readmePath, []byte("# README\n\nNo sentinels here.\n"), 0644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	if err := run(routerPath, readmePath); err == nil {
		t.Fatal("expected error when sentinels missing, got nil")
	} else if !strings.Contains(err.Error(), "sentinels") {
		t.Fatalf("expected sentinel error, got: %v", err)
	}
}

func TestRun_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	routerPath := filepath.Join(dir, "router.go")
	readmePath := filepath.Join(dir, "README.md")
	if err := os.WriteFile(routerPath, []byte(fixtureRouter), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	readme := "# README\n\nBefore content.\n\n" + readmeBeginSentinel + "\nold generated content\n" + readmeEndSentinel + "\n\nAfter content.\n"
	if err := os.WriteFile(readmePath, []byte(readme), 0644); err != nil {
		t.Fatalf("write README: %v", err)
	}

	if err := run(routerPath, readmePath); err != nil {
		t.Fatalf("run: %v", err)
	}

	out, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("re-read README: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "Before content.") {
		t.Error("before content lost")
	}
	if !strings.Contains(got, "After content.") {
		t.Error("after content lost")
	}
	if strings.Contains(got, "old generated content") {
		t.Error("old generated content not replaced")
	}
	if !strings.Contains(got, "**`add`**") {
		t.Error("generated catalogue missing 'add' command")
	}
	if !strings.Contains(got, "**`recall`** _(aliases: s)_") {
		t.Error("generated catalogue missing recall aliases")
	}
}