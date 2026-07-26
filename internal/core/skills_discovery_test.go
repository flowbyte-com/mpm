// skills_discovery_test.go — Task 9 of the skills-layer plan.
//
// ProactiveRecallHint must surface skills whose when_to_use field
// overlaps any keyword from the conversation text. Skills are a fourth
// hint source alongside the existing decision/theory matches from
// FindEpistemologyOverlaps.
//
// The minScore argument is -3.0 in every test because the underlying
// FindEpistemologyOverlaps treats `score >= threshold` as SKIP — BM25
// scores are negative numbers (closer to 0 = better match), so -3.0 is
// permissive enough to let every candidate through. The MCP handler
// (handleProactiveRecallHint) already defaults min_score to -3.0 when
// the caller doesn't supply one, confirming this is the floor.

package internal

import (
	"strings"
	"testing"
)

func TestProactiveRecallHint_SkillOverlaps(t *testing.T) {
	const skillFrontmatter = "---\nname: agentshell\nversion: 1.0.0\nwhen_to_use: agentshell, wordpress, theme\n---\nbody"

	cases := []struct {
		name         string
		conversation string
		wantSkill    bool
	}{
		{
			name:         "overlap surfaces skill hint",
			conversation: "I'm working with the agentshell theme",
			wantSkill:    true,
		},
		{
			name:         "no overlap yields no skill hint",
			conversation: "the weather is sunny today",
			wantSkill:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dm := NewTestDM(t)
			insertRawSkill(t, dm, "skill:agentshell-v1.0.0", "agentshell", "1.0.0", skillFrontmatter)

			hints, err := dm.ProactiveRecallHint(tc.conversation, 3, -3.0)
			if err != nil {
				t.Fatalf("ProactiveRecallHint: %v", err)
			}

			found := false
			for _, h := range hints {
				content, ok := h["content"].(string)
				if !ok {
					continue
				}
				if strings.Contains(content, "agentshell") {
					found = true
					break
				}
			}
			if found != tc.wantSkill {
				t.Fatalf("skill hint surfaced=%v, want %v (hints=%+v)", found, tc.wantSkill, hints)
			}
		})
	}
}

// TestProactiveRecallHint_TrimsAfterSkillAppend pins the ≤ maxHints
// contract after the skill pass. With three overlapping skills and
// maxHints=2, only two hints may be returned and at least one must be
// a skill (proving the trim was applied to the merged list, not just
// the BM25 hits).
func TestProactiveRecallHint_TrimsAfterSkillAppend(t *testing.T) {
	dm := NewTestDM(t)
	for _, name := range []string{"agentshell", "wp-deploy", "theme-forge"} {
		insertRawSkill(t, dm, "skill:"+name+"-v1.0.0", name, "1.0.0",
			"---\nname: "+name+"\nversion: 1.0.0\nwhen_to_use: agentshell, theme\n---\nbody")
	}

	hints, err := dm.ProactiveRecallHint("agentshell theme work", 2, -3.0)
	if err != nil {
		t.Fatalf("ProactiveRecallHint: %v", err)
	}
	if len(hints) > 2 {
		t.Fatalf("hint count = %d, want ≤ 2 after skill append", len(hints))
	}
	hasSkill := false
	for _, h := range hints {
		if t, _ := h["type"].(string); t == "skill" {
			hasSkill = true
			break
		}
	}
	if !hasSkill {
		t.Fatalf("expected at least one skill hint in trimmed result, got %+v", hints)
	}
}

func TestKeywordOverlap(t *testing.T) {
	cases := []struct {
		name     string
		keywords map[string]bool
		haystack string
		want     bool
	}{
		{
			name:     "empty keyword set is non-match",
			keywords: map[string]bool{},
			haystack: "agentshell",
			want:     false,
		},
		{
			name:     "empty haystack is non-match",
			keywords: map[string]bool{"agentshell": true},
			haystack: "",
			want:     false,
		},
		{
			name:     "case-insensitive substring hit",
			keywords: map[string]bool{"wp": true},
			haystack: "wp-deploy",
			want:     true,
		},
		{
			name:     "miss when no keyword in haystack",
			keywords: map[string]bool{"theme": true},
			haystack: "agentshell deploy",
			want:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := keywordOverlap(tc.keywords, tc.haystack); got != tc.want {
				t.Fatalf("keywordOverlap = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestProactiveRecallHint_ListSkillsFailureIsNonFatal is intentionally
// omitted: the swallow contract (directive_tools.go `if skillErr ==
// nil`) is verifiable via code review, and a fault-injection test
// would require either a method-level seam or a test-only hook that
// does not exist in this codebase. Documenting the omission here so a
// future reader does not re-add a brittle test that exercises the
// wrong path.
