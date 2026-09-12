// handlers_epistemology_resolve_note_test.go — pins the --note flag
// contract for `mpm theory resolve`. The help example
// `mpm theory resolve <id> refuted --note "see lesson #abc"` advertised
// --note but the handler only filtered --winner; --note leaked into the
// conclusion slot and the parser rejected it with
// "conclusion must be one of: confirmed, proven, disproven, refuted, invalidated".
//
// The fix lets --note=VALUE or `--note VALUE` through, captures the value
// in the metadata patch's `note` field, and still rejects empty/garbage
// conclusions. This test exercises the flag-stripping logic against the
// production helper splitResolveTheoryArgs (which handleResolveTheory
// delegates to).
package main

import "testing"

// TestResolveTheoryArgs_StripsNote exercises the --note flag extraction
// from the args slice before the conclusion parser runs. The fix is
// observable via the post-fix args shape: id and conclusion remain
// clean, --note value is captured.
func TestResolveTheoryArgs_StripsNote(t *testing.T) {
	cases := []struct {
		name           string
		args           []string
		wantID         string
		wantConclusion string
		wantNote       string
	}{
		{
			name:           "note_equals_form",
			args:           []string{"my-id", "refuted", "--note=latency regression"},
			wantID:         "my-id",
			wantConclusion: "refuted",
			wantNote:       "latency regression",
		},
		{
			name:           "note_space_form",
			args:           []string{"my-id", "refuted", "--note", "latency regression"},
			wantID:         "my-id",
			wantConclusion: "refuted",
			wantNote:       "latency regression",
		},
		{
			name:           "note_with_winner",
			args:           []string{"my-id", "invalidated", "--winner=abc123", "--note=supersedes #abc"},
			wantID:         "my-id",
			wantConclusion: "invalidated",
			wantNote:       "supersedes #abc",
		},
		{
			name:           "no_note",
			args:           []string{"my-id", "refuted"},
			wantID:         "my-id",
			wantConclusion: "refuted",
			wantNote:       "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, conclusion, _, note, err := splitResolveTheoryArgs(tc.args)
			if err != nil {
				t.Fatalf("splitResolveTheoryArgs returned error: %v", err)
			}
			if id != tc.wantID {
				t.Errorf("id = %q, want %q", id, tc.wantID)
			}
			if conclusion != tc.wantConclusion {
				t.Errorf("conclusion = %q, want %q", conclusion, tc.wantConclusion)
			}
			if note != tc.wantNote {
				t.Errorf("note = %q, want %q", note, tc.wantNote)
			}
		})
	}
}