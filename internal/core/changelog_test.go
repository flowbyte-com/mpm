package internal

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseCommitLog_Basic(t *testing.T) {
	raw := "abc1234\x1fAlice\x1ffeat(reference): chunk-hash diff ingest\x1fThis is the body.\nMore body.\x1e" +
		"def5678\x1fBob\x1ffix(ingest): route MCP add_reference through chunked path\x1f\x1e" +
		"ghi9012\x1fCarol\x1fchore: bump deps\x1f\x1e"

	entries := ParseCommitLog(raw)
	if got, want := len(entries), 3; got != want {
		t.Fatalf("len(entries) = %d, want %d", got, want)
	}

	e0 := entries[0]
	if e0.CommitHash != "abc1234" {
		t.Errorf("entry[0].CommitHash = %q, want %q", e0.CommitHash, "abc1234")
	}
	if e0.Author != "Alice" {
		t.Errorf("entry[0].Author = %q, want %q", e0.Author, "Alice")
	}
	if e0.Type != "feat" {
		t.Errorf("entry[0].Type = %q, want %q", e0.Type, "feat")
	}
	if e0.Scope != "reference" {
		t.Errorf("entry[0].Scope = %q, want %q", e0.Scope, "reference")
	}
	if e0.Summary != "chunk-hash diff ingest" {
		t.Errorf("entry[0].Summary = %q, want %q", e0.Summary, "chunk-hash diff ingest")
	}
	if !strings.Contains(e0.Body, "This is the body.") {
		t.Errorf("entry[0].Body missing body content: %q", e0.Body)
	}
	if e0.MPMMemoryIDs == nil {
		t.Error("entry[0].MPMMemoryIDs should be initialised to empty slice, not nil")
	}
	if len(e0.MPMMemoryIDs) != 0 {
		t.Errorf("entry[0].MPMMemoryIDs should be empty, got %v", e0.MPMMemoryIDs)
	}

	if entries[1].Type != "fix" || entries[1].Scope != "ingest" {
		t.Errorf("entry[1] parse wrong: type=%q scope=%q", entries[1].Type, entries[1].Scope)
	}
	if entries[2].Type != "chore" || entries[2].Scope != "" {
		t.Errorf("entry[2] parse wrong: type=%q scope=%q", entries[2].Type, entries[2].Scope)
	}
}

// TestParseCommitLog_NonConventional: a subject that does not match
// the conventional-commit shape still produces an entry, with Type
// empty and Summary holding the entire subject. Without this,
// "non-conventional" commits silently disappear from the changelog.
func TestParseCommitLog_NonConventional(t *testing.T) {
	raw := "deadbee\x1fAlice\x1fWIP: tweak some stuff\x1f\x1e"
	entries := ParseCommitLog(raw)
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.Type != "" {
		t.Errorf("Type = %q, want empty", e.Type)
	}
	if e.Summary != "WIP: tweak some stuff" {
		t.Errorf("Summary = %q, want %q", e.Summary, "WIP: tweak some stuff")
	}
}

// TestParseCommitLog_EmptyBody: single-line commits with no body.
func TestParseCommitLog_EmptyBody(t *testing.T) {
	raw := "f00d\x1fAlice\x1ffix: thing\x1f\x1e"
	entries := ParseCommitLog(raw)
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}
	if entries[0].Body != "" {
		t.Errorf("Body = %q, want empty", entries[0].Body)
	}
}

func TestGroupBySection(t *testing.T) {
	entries := []ChangelogEntry{
		{Type: "feat", Summary: "a"},
		{Type: "feat", Summary: "b"},
		{Type: "fix", Summary: "c"},
		{Type: "refactor", Summary: "d"},
		{Type: "chore", Summary: "e"},
		{Type: "", Summary: "f"}, // non-conventional -> Other
	}
	groups := GroupBySection(entries)
	if got := len(groups["Added"]); got != 2 {
		t.Errorf("Added: got %d, want 2", got)
	}
	if got := len(groups["Fixed"]); got != 1 {
		t.Errorf("Fixed: got %d, want 1", got)
	}
	if got := len(groups["Changed"]); got != 1 {
		t.Errorf("Changed: got %d, want 1 (refactor lands here)", got)
	}
	if got := len(groups["Other"]); got != 2 {
		t.Errorf("Other: got %d, want 2 (chore + non-conventional)", got)
	}
}

func TestRenderMarkdown_Structure(t *testing.T) {
	doc := &ChangelogDocument{
		Project: "Test",
		Releases: []ChangelogRelease{
			{
				Version: "1.1.0",
				Date:    "2026-06-19",
				Entries: []ChangelogEntry{
					{Type: "feat", Scope: "reference", Summary: "embed chunks", CommitHash: "abc1234567", MPMMemoryIDs: []string{}},
					{Type: "fix", Summary: "router typo", CommitHash: "def4567", MPMMemoryIDs: []string{}},
				},
			},
		},
	}
	md := doc.RenderMarkdown()

	// Header.
	if !strings.Contains(md, "# Test Changelog") {
		t.Error("missing project header")
	}
	// Version heading.
	if !strings.Contains(md, "## [1.1.0] - 2026-06-19") {
		t.Error("missing version heading")
	}
	// Sections in canonical order: Added first, then Fixed.
	addedIdx := strings.Index(md, "### Added")
	fixedIdx := strings.Index(md, "### Fixed")
	if addedIdx == -1 || fixedIdx == -1 {
		t.Fatal("missing Added or Fixed section")
	}
	if addedIdx > fixedIdx {
		t.Error("Added section should appear before Fixed")
	}
	// Scope is bolded.
	if !strings.Contains(md, "**reference:** embed chunks") {
		t.Error("scope not rendered as bold prefix")
	}
	// Short SHA links to the full commit.
	if !strings.Contains(md, "[abc1234](https://github.com/example/mpm/commit/abc1234567)") {
		t.Error("commit link not rendered with short SHA + full URL")
	}
}

func TestRenderMarkdown_Highlight(t *testing.T) {
	doc := &ChangelogDocument{
		Project: "T",
		Releases: []ChangelogRelease{
			{
				Version: "1.1.0",
				Date:    "2026-06-19",
				Entries: []ChangelogEntry{
					{Highlight: true, Body: "Reference arc complete.\nShelf-vs-Mind architecture confirmed."},
					{Type: "feat", Summary: "embed chunks"},
				},
			},
		},
	}
	md := doc.RenderMarkdown()

	// Highlight prose rendered as blockquote.
	if !strings.Contains(md, "> Reference arc complete.") {
		t.Error("highlight not rendered as blockquote")
	}
	if !strings.Contains(md, "> Shelf-vs-Mind architecture confirmed.") {
		t.Error("highlight second line not rendered")
	}
	// Highlight entry should NOT appear as a bullet.
	if strings.Contains(md, "- Reference arc complete.") {
		t.Error("highlight leaked into bullet list")
	}
}

func TestRenderMarkdown_Legacy(t *testing.T) {
	doc := &ChangelogDocument{
		Project: "T",
		Releases: []ChangelogRelease{
			{Version: "1.1.0", Date: "2026-06-19", Entries: []ChangelogEntry{
				{Type: "feat", Summary: "new"},
			}},
		},
		Legacy: &ChangelogRelease{
			Version: "1.0.1-legacy",
			Date:    "2026-06-19",
			Entries: []ChangelogEntry{
				{CommitHash: "ccc", Summary: "old thing 1"},
				{CommitHash: "aaa", Summary: "old thing 2"},
			},
		},
	}
	md := doc.RenderMarkdown()

	if !strings.Contains(md, "## [1.0.1-legacy] - 2026-06-19") {
		t.Error("legacy section heading missing")
	}
	if !strings.Contains(md, "**Legacy backfill.**") {
		t.Error("legacy backfill banner missing")
	}
	// Legacy entries sorted by commit hash (stable proxy for time).
	idxAAA := strings.Index(md, "- old thing 2")
	idxCCC := strings.Index(md, "- old thing 1")
	if idxAAA == -1 || idxCCC == -1 {
		t.Fatal("legacy entries missing")
	}
	if idxAAA > idxCCC {
		t.Error("legacy entries should be sorted by commit hash (aaa before ccc)")
	}
}

func TestFetchGitLog_Integration(t *testing.T) {
	// Hermetic: build a fixture repo with a tag + 60 commits so the test
	// is deterministic and independent of the real MPM git history
	// (which has been rewritten and no longer has a "v1.0.0-hardened"
	// tag). The original test asserted >= 50 entries since that tag
	// to catch silent breakage of the git log walker; this fixture
	// version reproduces that guarantee with synthetic data.
	repo := t.TempDir()

	// Initialize a fresh git repo. `git config` below requires .git to exist;
	// -b main avoids the "default branch name" warning on modern git.
	runGit(t, repo, "init", "-b", "main")

	// Seed commits require a user identity for git author/committer.
	for _, kv := range [][2]string{
		{"user.name", "Stranger Test"},
		{"user.email", "stranger@example.com"},
	} {
		cmd := exec.Command("git", "-C", repo, "config", kv[0], kv[1])
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git config %s: %v: %s", kv[0], err, out)
		}
	}

	// Three seed files before the tag — these will be excluded by --since.
	for i := 0; i < 3; i++ {
		writeGitFixtureFile(t, repo, "seed.txt", fmt.Sprintf("seed %d", i))
		runGit(t, repo, "add", "seed.txt")
		runGit(t, repo, "commit", "-m", fmt.Sprintf("seed commit %d", i))
	}
	runGit(t, repo, "tag", "v0.0.1")

	// 60 commits after the tag — these will be included by --since.
	for i := 0; i < 60; i++ {
		writeGitFixtureFile(t, repo, "post.txt", fmt.Sprintf("post %d", i))
		runGit(t, repo, "add", "post.txt")
		runGit(t, repo, "commit", "-m", fmt.Sprintf("post commit %d", i))
	}

	out, err := FetchGitLog(GitLogOptions{RepoDir: repo, Since: "v0.0.1"})
	if err != nil {
		t.Fatalf("FetchGitLog: %v", err)
	}
	entries := ParseCommitLog(out)
	if len(entries) < 50 {
		t.Errorf("expected at least 50 entries since v0.0.1, got %d", len(entries))
	}
	// Every entry must have a non-empty commit hash and a non-empty
	// author — git log %H and %an are non-optional fields. If these
	// are empty, the pretty-format string has been corrupted.
	for i, e := range entries {
		if e.CommitHash == "" {
			t.Errorf("entry[%d] missing CommitHash", i)
		}
		if e.Author == "" {
			t.Errorf("entry[%d] missing Author", i)
		}
	}
}

func writeGitFixtureFile(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-C", dir}, args...)
	cmd := exec.Command("git", full...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}

func TestChangelogEntry_ShapeForSynthesis(t *testing.T) {
	// This is the load-bearing assertion that the JSON schema is
	// shaped for the future synthesis engine. If anyone changes
	// ChangelogEntry to drop CommitHash or MPMMemoryIDs, this test
	// fails and forces a conscious decision.
	e := ChangelogEntry{
		Version:      "1.1.0",
		Date:         "2026-06-19",
		CommitHash:   "abc1234",
		Author:       "v",
		Type:         "feat",
		Scope:        "reference",
		Summary:      "embed chunks",
		MPMMemoryIDs: []string{"mem-c937c319d85f4231"},
	}
	if e.CommitHash == "" {
		t.Error("CommitHash required for synthesis join")
	}
	if e.MPMMemoryIDs == nil {
		t.Error("MPMMemoryIDs must be a slice (possibly empty), not nil — agents joining by index need a stable shape")
	}
}

func TestValidateGitRef(t *testing.T) {
	cases := []struct {
		name    string
		ref     string
		wantErr bool
	}{
		// Empty is allowed (treats as "no bound").
		{"empty", "", false},
		// Normal refs.
		{"semver", "v1.2.3", false},
		{"branch", "main", false},
		{"branch-with-slash", "feature/foo", false},
		{"commit-hash", "abc1234", false},
		{"underscore-dot", "release_1.0.0", false},
		// Injection vectors — must reject.
		{"semicolon", "v1.0.0; rm -rf /", true},
		{"command-substitution", "$(whoami)", true},
		{"backticks", "`whoami`", true},
		{"newline", "v1.0.0\nrm -rf /", true},
		{"ampersand", "v1.0.0 && echo pwned", true},
		{"pipe", "v1.0.0 | cat /etc/passwd", true},
		{"space-injection", "v1.0.0 ;id", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateGitRef(tc.ref)
			if tc.wantErr && err == nil {
				t.Errorf("validateGitRef(%q) = nil, want error", tc.ref)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateGitRef(%q) = %v, want nil", tc.ref, err)
			}
		})
	}
}
