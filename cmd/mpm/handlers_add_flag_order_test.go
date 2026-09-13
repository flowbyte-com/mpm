package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core/config"
	_ "github.com/flowbyte-com/mpm-core"
)

// buildMpmForFlagTest compiles the mpm binary into a per-test temp dir.
func buildMpmForFlagTest(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	binPath := binDir + "/mpm"
	cmd := exec.Command("go", "build", "-tags", "fts5", "-o", binPath, ".")
	cmd.Dir = "."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mpm: %v\n%s", err, out)
	}
	return binPath
}

// TestHandleAdd_FlagAfterContent pins the launch fix that prevents
// `mpm add "hello" --weight 5` from silently absorbing `--weight 5`
// as content. The fix reorders known flags before positional content
// so Go's flag.Parse sees them in the expected order.
func TestHandleAdd_FlagAfterContent(t *testing.T) {
	bin := buildMpmForFlagTest(t)
	workspace := t.TempDir()
	cfgPath := workspace + "/mpm_config.json"
	if err := os.WriteFile(cfgPath, []byte(`{
  "profiles": {"default": {"provider": "minimax", "model": "M2.7", "api_key": "sk"}}
}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	tests := []struct {
		name string
		args []string
		// expected fields in saved memory row
		wantContent string
		wantWeight  float64
		wantTags    string
	}{
		{
			name:        "--weight after content",
			args:        []string{"add", "weighted test", "--weight", "5"},
			wantContent: "weighted test",
			wantWeight:  5,
		},
		{
			name:        "--weight before content",
			args:        []string{"add", "--weight", "7", "weighted early"},
			wantContent: "weighted early",
			wantWeight:  7,
		},
		{
			name:        "--tags after content",
			args:        []string{"add", "tagged test", "--tags", "foo,bar"},
			wantContent: "tagged test",
			// 2026-09-10 cleanup: the canonical save default is 0.5
			// (legacy float scale), which normalizeWeightToColumn
			// resolves to a column value of 5. Pre-fix the CLI
			// defaulted to 1.0 (column=1) which diverged from the
			// tool path's 0.5 (column=5). Both surfaces now agree at
			// 5. Tests that exercised the divergent CLI default
			// (this one) are updated to the canonical value.
			//
			// 2026-09-13 acceptance: --tags (plural) is the canonical
			// flag name; the pre-fix --tag (singular) is replaced so
			// the help text matches the parser and the documented
			// muscle-memory form actually works.
			wantWeight:  5,
			wantTags:    `["foo","bar"]`,
		},
		{
			name:        "remember alias: --weight after content",
			args:        []string{"remember", "remember test", "--weight", "9"},
			wantContent: "remember test",
			wantWeight:  9,
		},
		{
			name:        "remember alias: --weight before content",
			args:        []string{"remember", "--weight", "3", "remembered early"},
			wantContent: "remembered early",
			wantWeight:  3,
		},
		{
			name:        "no flag, plain content",
			args:        []string{"add", "plain test"},
			wantContent: "plain test",
			// 2026-09-10 cleanup: see the note on `--tag after
			// content` above — the canonical CLI default is now
			// column=5 (via 0.5 float scale), matching the tool
			// surface.
			wantWeight:  5,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(bin, tc.args...)
			cmd.Env = append([]string{}, "MPM_WORKSPACE="+workspace)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("mpm %v: %v\n%s", tc.args, err, out)
			}

			// Verify the saved row's fields via direct DB read.
			cfg, err := config.LoadConfig()
			if err != nil {
				t.Fatalf("load config: %v", err)
			}
			_ = cfg // config.LoadConfig reads mpm_config.json; we don't need the value here
			// Read the most recent memory row from the workspace DB.
			dbPath := workspace + "/src/db/mpm.db"
			row := readMostRecentMemory(t, dbPath)
			if got := memoryContent(t, row); got != tc.wantContent {
				t.Errorf("content: got %q, want %q", got, tc.wantContent)
			}
			if got := memoryWeight(t, row); got != tc.wantWeight {
				t.Errorf("weight: got %v, want %v", got, tc.wantWeight)
			}
			if tc.wantTags != "" {
				if got := memoryTags(t, row); got != tc.wantTags {
					t.Errorf("tags: got %s, want %s", got, tc.wantTags)
				}
			}
		})
	}
}

// readMostRecentMemory shells out to sqlite3 CLI to find the most
// recently created memory row. Returns the row as a map.
func readMostRecentMemory(t *testing.T, dbPath string) map[string]interface{} {
	t.Helper()
	out, err := exec.Command("sqlite3", "-json", dbPath,
		"SELECT id, content, weight, tags FROM memories WHERE collection='memories' AND deleted_at IS NULL ORDER BY created_at DESC LIMIT 1",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("sqlite3: %v\n%s", err, out)
	}
	outStr := strings.TrimSpace(string(out))
	if outStr == "" {
		t.Fatalf("no memory row found")
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal([]byte(outStr), &rows); err != nil {
		t.Fatalf("unmarshal sqlite3 output: %v\n%s", err, outStr)
	}
	if len(rows) == 0 {
		t.Fatalf("empty result set")
	}
	return rows[0]
}

func memoryContent(t *testing.T, row map[string]interface{}) string {
	t.Helper()
	v, ok := row["content"].(string)
	if !ok {
		t.Fatalf("content not a string: %v", row["content"])
	}
	return v
}

func memoryWeight(t *testing.T, row map[string]interface{}) float64 {
	t.Helper()
	v, ok := row["weight"].(float64)
	if !ok {
		t.Fatalf("weight not a float64: %v", row["weight"])
	}
	return v
}

func memoryTags(t *testing.T, row map[string]interface{}) string {
	t.Helper()
	v, ok := row["tags"].(string)
	if !ok {
		t.Fatalf("tags not a string: %v", row["tags"])
	}
	return v
}
