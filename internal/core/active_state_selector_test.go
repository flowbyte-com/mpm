package internal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// setupTestWorkspace creates a temporary workspace with the given persona
// and mode .md files. Each entry in personas/modes is the basename
// (without extension) of a definition file to create; an empty name means
// the directory is missing entirely. Returns the workspace path.
//
// The workspace contains an active.json pre-populated with the supplied
// ActiveState (so callers can drive the resolver through a chosen
// persisted state). Pass a nil active to start with no active.json at all.
func setupTestWorkspace(t *testing.T, personas []string, modes []string, active *ActiveState) string {
	t.Helper()
	root := t.TempDir()

	if len(personas) > 0 {
		pdir := filepath.Join(root, "persona")
		if err := os.MkdirAll(pdir, 0700); err != nil {
			t.Fatalf("mkdir persona: %v", err)
		}
		for _, name := range personas {
			path := filepath.Join(pdir, name+".md")
			front := "---\nname: " + name + "\n---\nbody for " + name + "\n"
			if err := os.WriteFile(path, []byte(front), 0644); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
		}
	}
	if len(modes) > 0 {
		mdir := filepath.Join(root, "mode")
		if err := os.MkdirAll(mdir, 0700); err != nil {
			t.Fatalf("mkdir mode: %v", err)
		}
		for _, name := range modes {
			path := filepath.Join(mdir, name+".md")
			front := "---\nname: " + name + "\n---\nbody for " + name + "\n"
			if err := os.WriteFile(path, []byte(front), 0644); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
		}
	}
	if active != nil {
		if err := SaveActiveJSON(active); err != nil {
			t.Fatalf("save active: %v", err)
		}
	}
	t.Setenv("MPM_WORKSPACE", root)
	return root
}

// ── Persona resolver: pointer-aware intent model ──────────────────────────

func TestResolveActivePersona_PersonaAbsent_FallsBackToDefault(t *testing.T) {
	ws := setupTestWorkspace(t, []string{"critic", "default"}, []string{"default"}, nil)
	dm, err := NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	defer dm.Close()

	res := ResolveActivePersonaIntent(dm, nil) // absent
	if res.Name != "default" {
		t.Errorf("absent persona: Name=%q, want %q", res.Name, "default")
	}
	if res.Source != SourceFallback {
		t.Errorf("absent persona: Source=%q, want %q", res.Source, SourceFallback)
	}
}

func TestResolveActivePersona_ExplicitValid_ReturnsExplicit(t *testing.T) {
	ws := setupTestWorkspace(t, []string{"critic", "default"}, []string{"default"}, nil)
	dm, _ := NewDatabaseManager(ws)
	defer dm.Close()

	c := "critic"
	res := ResolveActivePersonaIntent(dm, &c)
	if res.Name != "critic" {
		t.Errorf("explicit critic: Name=%q, want %q", res.Name, "critic")
	}
	if res.Source != SourceExplicit {
		t.Errorf("explicit critic: Source=%q, want %q", res.Source, SourceExplicit)
	}
}

func TestResolveActivePersona_ExplicitClear_ReturnsEmpty(t *testing.T) {
	ws := setupTestWorkspace(t, []string{"critic", "default"}, []string{"default"}, nil)
	dm, _ := NewDatabaseManager(ws)
	defer dm.Close()

	empty := ""
	res := ResolveActivePersonaIntent(dm, &empty)
	if res.Name != "" {
		t.Errorf("explicit clear: Name=%q, want \"\"", res.Name)
	}
	if res.Source != SourceEmpty {
		t.Errorf("explicit clear: Source=%q, want %q", res.Source, SourceEmpty)
	}
}

func TestResolveActivePersona_StoredFileLaterRemoved_FallsBack(t *testing.T) {
	ws := setupTestWorkspace(t, []string{"critic", "default"}, []string{"default"}, nil)
	dm, _ := NewDatabaseManager(ws)
	defer dm.Close()

	// Selection points at "ghost" which doesn't exist.
	g := "ghost"
	res := ResolveActivePersonaIntent(dm, &g)
	if res.Name != "default" {
		t.Errorf("stale persona: Name=%q, want %q", res.Name, "default")
	}
	if res.Source != SourceFallback {
		t.Errorf("stale persona: Source=%q, want %q", res.Source, SourceFallback)
	}
}

func TestResolveActivePersona_AllMissing_ReturnsEmpty(t *testing.T) {
	// No persona files on disk at all — both requested and default
	// missing → empty + SourceEmpty.
	ws := setupTestWorkspace(t, nil, []string{"default"}, nil)
	dm, _ := NewDatabaseManager(ws)
	defer dm.Close()

	g := "ghost"
	res := ResolveActivePersonaIntent(dm, &g)
	if res.Name != "" {
		t.Errorf("all-missing persona: Name=%q, want \"\"", res.Name)
	}
	if res.Source != SourceEmpty {
		t.Errorf("all-missing persona: Source=%q, want %q", res.Source, SourceEmpty)
	}
}

// ── Mode resolver: 0..N contract ──────────────────────────────────────────

func TestResolveActiveModes_Absent_BootstrapsToDefault(t *testing.T) {
	ws := setupTestWorkspace(t, []string{"default"}, []string{"default"}, nil)
	dm, _ := NewDatabaseManager(ws)
	defer dm.Close()

	res := ResolveActiveModes(dm, nil)
	if res.Source != SourceFallback {
		t.Errorf("absent modes: Source=%q, want %q", res.Source, SourceFallback)
	}
	if got := res.Names(); !reflect.DeepEqual(got, []string{"default"}) {
		t.Errorf("absent modes: Names=%v, want [default]", got)
	}
}

func TestResolveActiveModes_ExplicitEmpty_ReturnsEmpty(t *testing.T) {
	ws := setupTestWorkspace(t, []string{"default"}, []string{"default"}, nil)
	dm, _ := NewDatabaseManager(ws)
	defer dm.Close()

	empty := []string{}
	res := ResolveActiveModes(dm, &empty)
	if res.Source != SourceEmpty {
		t.Errorf("explicit clear modes: Source=%q, want %q", res.Source, SourceEmpty)
	}
	if len(res.Modes) != 0 {
		t.Errorf("explicit clear modes: Modes=%v, want []", res.Modes)
	}
}

func TestResolveActiveModes_SingleValid_ReturnsExplicit(t *testing.T) {
	ws := setupTestWorkspace(t, []string{"default"}, []string{"debugging", "default"}, nil)
	dm, _ := NewDatabaseManager(ws)
	defer dm.Close()

	d := []string{"debugging"}
	res := ResolveActiveModes(dm, &d)
	if res.Source != SourceExplicit {
		t.Errorf("single mode: Source=%q, want %q", res.Source, SourceExplicit)
	}
	if got := res.Names(); !reflect.DeepEqual(got, []string{"debugging"}) {
		t.Errorf("single mode: Names=%v, want [debugging]", got)
	}
}

func TestResolveActiveModes_TwoValid_ReturnsBoth(t *testing.T) {
	ws := setupTestWorkspace(t, []string{"default"},
		[]string{"debugging", "forensic", "default"}, nil)
	dm, _ := NewDatabaseManager(ws)
	defer dm.Close()

	sel := []string{"debugging", "forensic"}
	res := ResolveActiveModes(dm, &sel)
	if res.Source != SourceExplicit {
		t.Errorf("two modes: Source=%q, want %q", res.Source, SourceExplicit)
	}
	if got := res.Names(); !reflect.DeepEqual(got, []string{"debugging", "forensic"}) {
		t.Errorf("two modes: Names=%v, want [debugging forensic]", got)
	}
}

func TestResolveActiveModes_OneStaleDropsMissingNoDefaultInjection(t *testing.T) {
	// The KEY invariant: selecting [debugging, forensic] and forensic
	// disappearing must NOT inject `default` alongside debugging.
	ws := setupTestWorkspace(t, []string{"default"},
		[]string{"debugging", "default"}, nil)
	dm, _ := NewDatabaseManager(ws)
	defer dm.Close()

	sel := []string{"debugging", "forensic"} // forensic file doesn't exist
	res := ResolveActiveModes(dm, &sel)
	if res.Source != SourceExplicit {
		t.Errorf("one-stale: Source=%q, want %q", res.Source, SourceExplicit)
	}
	if got := res.Names(); !reflect.DeepEqual(got, []string{"debugging"}) {
		t.Errorf("one-stale: Names=%v, want [debugging] only (no default injection)", got)
	}
	if !reflect.DeepEqual(res.Missing, []string{"forensic"}) {
		t.Errorf("one-stale: Missing=%v, want [forensic]", res.Missing)
	}
}

func TestResolveActiveModes_AllStale_FallsBackToDefault(t *testing.T) {
	// If ALL selected modes are stale, established recovery policy is
	// to fall back to [default] with SourceFallback.
	ws := setupTestWorkspace(t, []string{"default"}, []string{"default"}, nil)
	dm, _ := NewDatabaseManager(ws)
	defer dm.Close()

	sel := []string{"ghost1", "ghost2"} // neither exists
	res := ResolveActiveModes(dm, &sel)
	if res.Source != SourceFallback {
		t.Errorf("all-stale: Source=%q, want %q", res.Source, SourceFallback)
	}
	if got := res.Names(); !reflect.DeepEqual(got, []string{"default"}) {
		t.Errorf("all-stale: Names=%v, want [default]", got)
	}
	if !reflect.DeepEqual(res.Missing, []string{"ghost1", "ghost2"}) {
		t.Errorf("all-stale: Missing=%v, want [ghost1 ghost2]", res.Missing)
	}
}

func TestResolveActiveModes_AllStaleNoDefault_ReturnsEmpty(t *testing.T) {
	ws := setupTestWorkspace(t, []string{"default"}, []string{"debugging"}, nil) // no default mode
	dm, _ := NewDatabaseManager(ws)
	defer dm.Close()

	sel := []string{"ghost1", "ghost2"}
	res := ResolveActiveModes(dm, &sel)
	if res.Source != SourceEmpty {
		t.Errorf("all-stale-no-default: Source=%q, want %q", res.Source, SourceEmpty)
	}
	if len(res.Modes) != 0 {
		t.Errorf("all-stale-no-default: Modes=%v, want []", res.Modes)
	}
}

// ── ActiveState pointer intent model ──────────────────────────────────────

func TestActiveState_IntentDistinction(t *testing.T) {
	cases := []struct {
		name              string
		persona           *string
		modes             *[]string
		wantPersonaAbsent bool
		wantPersonaClear  bool
		wantModesAbsent   bool
		wantModesClear    bool
	}{
		{"absent fields", nil, nil, true, false, true, false},
		{"explicit clear persona", ptrStr(""), ptrStrs([]string{"a"}), false, true, false, false},
		{"explicit clear modes", ptrStr("x"), ptrStrs([]string{}), false, false, false, true},
		{"explicit selection", ptrStr("x"), ptrStrs([]string{"a"}), false, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &ActiveState{Persona: tc.persona, Modes: tc.modes}
			if got := s.IsPersonaAbsent(); got != tc.wantPersonaAbsent {
				t.Errorf("IsPersonaAbsent=%v, want %v", got, tc.wantPersonaAbsent)
			}
			if got := s.IsPersonaExplicitClear(); got != tc.wantPersonaClear {
				t.Errorf("IsPersonaExplicitClear=%v, want %v", got, tc.wantPersonaClear)
			}
			if got := s.IsModesAbsent(); got != tc.wantModesAbsent {
				t.Errorf("IsModesAbsent=%v, want %v", got, tc.wantModesAbsent)
			}
			if got := s.IsModesExplicitClear(); got != tc.wantModesClear {
				t.Errorf("IsModesExplicitClear=%v, want %v", got, tc.wantModesClear)
			}
		})
	}
}

func ptrStr(s string) *string       { return &s }
func ptrStrs(s []string) *[]string { return &s }

// ── JSON wire form preserves intent ───────────────────────────────────────

func TestActiveState_JSONPreservesIntent(t *testing.T) {
	cases := []struct {
		name    string
		state   ActiveState
		wantKey map[string]bool // which keys should appear in JSON
	}{
		{"absent", ActiveState{}, map[string]bool{}},
		{"explicit clear", ActiveState{Persona: ptrStr(""), Modes: ptrStrs([]string{})},
			map[string]bool{"persona": true, "modes": true}},
		{"explicit selection", ActiveState{Persona: ptrStr("critic"), Modes: ptrStrs([]string{"debugging"})},
			map[string]bool{"persona": true, "modes": true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.state)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var m map[string]interface{}
			if err := json.Unmarshal(data, &m); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			for key, present := range tc.wantKey {
				if _, has := m[key]; has != present {
					t.Errorf("key %q present=%v, want %v (json=%s)", key, has, present, data)
				}
			}
		})
	}
}