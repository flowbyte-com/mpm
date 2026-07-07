package internal

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestFormatEphemeralPersonaAsMarkdown_YAMLInjection is the regression guard
// for the YAML frontmatter injection bug. The old FormatEphemeralPersonaAsMarkdown
// emitted the frontmatter via fmt.Sprintf with raw %s interpolation, so any
// persona field containing `:` or `\n` corrupted the frontmatter block:
//   - A `name` of "evil: hack" would parse as a single-line scalar with a
//     `hack:` subkey, silently injecting a new frontmatter key.
//   - A `title` of "a\n---\ninjected: yes" would terminate the frontmatter
//     block early and inject a second frontmatter section.
//
// The fix routes the frontmatter through yaml.v3 marshalling. This test
// verifies that hostile inputs round-trip losslessly: the output's
// frontmatter block parses back to the same values that went in.
//
// If this test ever fails, a downstream parser (router.go frontmatter
// reader, persona-validator, LLM prompt loader) is at risk of executing
// attacker-controlled YAML keys/values.
//
// NOTE: this test deliberately avoids putting `---` mid-line in any value
// because the frontmatter closer `---` would split a string-SplitN on
// the wrong boundary. `---` mid-line in a yaml scalar is legal but
// creates ambiguity in this naive delimiter scheme — that's a downstream
// parser concern, not what this regression guard covers.
func TestFormatEphemeralPersonaAsMarkdown_YAMLInjection(t *testing.T) {
	hostile := &EphemeralPersona{
		Name:         "evil: hack",                       // `:` would inject subkey
		Title:        "multi\nline\ntitle",              // `\n` would break block
		Creature:     "creature: with colon",            // another `:`
		Vibe:         `backtick and "quote" and 'apos'`, // quote chars
		Voice:        "voice with colons: in it",        // colon stress
		VoiceGuards:  "guards: {nested: yes}, [list], &anchor, *ref, |literal\nmultiline",
	}

	md := FormatEphemeralPersonaAsMarkdown(hostile)

	// Strip the opening `---\n` and split at the closing `---` (which
	// sits on its own line, preceded by `\n`). Take everything between.
	const opener = "---\n"
	if !strings.HasPrefix(md, opener) {
		t.Fatalf("output missing opening frontmatter delimiter:\n%s", md)
	}
	rest := strings.TrimPrefix(md, opener)
	closeIdx := strings.Index(rest, "\n---")
	if closeIdx < 0 {
		t.Fatalf("output missing closing frontmatter delimiter:\n%s", md)
	}
	fmText := rest[:closeIdx]
	body := rest[closeIdx+4:] // skip past "\n---"

	// Parse the frontmatter. yaml.Unmarshal MUST succeed and MUST
	// recover the original values exactly. If it doesn't, the frontmatter
	// is broken / hostile / injected.
	var got fmPersona
	if err := yaml.Unmarshal([]byte(fmText), &got); err != nil {
		t.Fatalf("frontmatter did not parse as YAML:\n--- frontmatter ---\n%s\n--- error ---\n%v", fmText, err)
	}
	if got.Name != hostile.Name {
		t.Errorf("Name: got %q, want %q (injection / corruption)", got.Name, hostile.Name)
	}
	if got.Title != hostile.Title {
		t.Errorf("Title: got %q, want %q (newline corruption)", got.Title, hostile.Title)
	}
	if got.Creature != hostile.Creature {
		t.Errorf("Creature: got %q, want %q", got.Creature, hostile.Creature)
	}
	if got.Vibe != hostile.Vibe {
		t.Errorf("Vibe: got %q, want %q (quote corruption)", got.Vibe, hostile.Vibe)
	}
	if got.Voice != hostile.Voice {
		t.Errorf("Voice: got %q, want %q", got.Voice, hostile.Voice)
	}
	if got.VoiceGuards != hostile.VoiceGuards {
		t.Errorf("VoiceGuards: got %q, want %q (YAML-special chars)", got.VoiceGuards, hostile.VoiceGuards)
	}

	// The body must follow the closing `---`. Sanity check that the
	// markdown structure survived: title heading + 4 H2 sections.
	if !strings.Contains(body, "# multi") {
		t.Errorf("expected body H1 to start with title prefix '# multi', got:\n%s", body)
	}
	for _, header := range []string{"## Creature", "## Vibe", "## Voice", "## Voice Guards"} {
		if !strings.Contains(body, header) {
			t.Errorf("body missing section header %q", header)
		}
	}
}

// TestFormatEphemeralPersonaAsFrontmatter_YAMLInjection is the same
// regression guard for the frontmatter-only renderer. FormatEphemeralPersonaAsFrontmatter
// was already using yaml.v3 before this fix, so this test pins the
// contract: if anyone "simplifies" it back to Sprintf, this fails.
func TestFormatEphemeralPersonaAsFrontmatter_YAMLInjection(t *testing.T) {
	hostile := &EphemeralPersona{
		Name:        "evil: hack",
		Title:       "multi\nline",
		Creature:    "x",
		Vibe:        "y",
		Voice:       "z",
		VoiceGuards: "guards: {nested}, &anchor",
	}
	out, err := FormatEphemeralPersonaAsFrontmatter(hostile)
	if err != nil {
		t.Fatalf("FormatEphemeralPersonaAsFrontmatter: %v", err)
	}

	// Strip the opening/closing `---` markers and parse the body.
	trimmed := strings.TrimPrefix(out, "---\n")
	trimmed = strings.TrimSuffix(trimmed, "---")
	var got fmPersona
	if err := yaml.Unmarshal([]byte(trimmed), &got); err != nil {
		t.Fatalf("frontmatter did not parse as YAML:\n%s\n--- error ---\n%v", out, err)
	}
	if got.Name != hostile.Name || got.Title != hostile.Title || got.VoiceGuards != hostile.VoiceGuards {
		t.Errorf("frontmatter corrupted: got %+v, want key fields from %+v", got, hostile)
	}
}

// TestFormatEphemeralPersonaAsMarkdown_LegacyFallback verifies the
// AntiPatterns → VoiceGuards fallback path. JIT personas generated
// before the rename (2026-06-26) only have AntiPatterns populated;
// they must still render correctly until migration is complete.
func TestFormatEphemeralPersonaAsMarkdown_LegacyFallback(t *testing.T) {
	legacy := &EphemeralPersona{
		Name:         "legacy-persona",
		Title:        "Legacy",
		Creature:     "x",
		Vibe:         "y",
		Voice:        "z",
		AntiPatterns: "don't do this", // pre-rename field
		VoiceGuards:  "",              // empty — fallback should kick in
	}
	md := FormatEphemeralPersonaAsMarkdown(legacy)

	if !strings.Contains(md, "voice_guards: don't do this") {
		t.Errorf("legacy AntiPatterns did not flow into voice_guards:\n%s", md)
	}
}

// TestFormatEphemeralPersonaAsMarkdown_VoiceGuardsPriority verifies the
// new field wins when both are populated (the post-rename path).
func TestFormatEphemeralPersonaAsMarkdown_VoiceGuardsPriority(t *testing.T) {
	both := &EphemeralPersona{
		Name:         "n",
		Title:        "t",
		Creature:     "c",
		Vibe:         "v",
		Voice:        "o",
		AntiPatterns: "old content",
		VoiceGuards:  "new content",
	}
	md := FormatEphemeralPersonaAsMarkdown(both)
	if !strings.Contains(md, "voice_guards: new content") {
		t.Errorf("VoiceGuards did not take priority:\n%s", md)
	}
	if strings.Contains(md, "voice_guards: old content") {
		t.Errorf("legacy AntiPatterns leaked into output despite VoiceGuards being set:\n%s", md)
	}
}