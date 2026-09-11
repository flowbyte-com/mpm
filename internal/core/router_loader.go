package internal

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// frontmatterSchema handles both comma-separated string and []string shapes
// for the three list fields. YAML.v3 unmarshals either into any.
//
// ANTI-PATTERNS RENAME (2026-06-26):
// The original `anti_patterns:` field was misconfigured across all 16
// components as voice guards (output constraints like 'bikeshedding',
// 'premature optimization') rather than as input filters. They almost
// never matched prompts, so the -1 penalty mechanism was dormant.
// Split into two semantically explicit fields:
//   - voice_guards:  output constraints (descriptive prose; not compiled)
//                    kept for LLM context — what the persona should NOT say
//   - domain_out:    input filters (regex fragments; compiled; -1 per match)
//                    prompt-vocabulary phrases that should reduce the persona's
//                    routing score and let a better-fit specialist win
type frontmatterSchema struct {
	Name        string `yaml:"name"`
	Patterns    any    `yaml:"patterns,omitempty"`
	VoiceGuards any    `yaml:"voice_guards,omitempty"`
	DomainOut   any    `yaml:"domain_out,omitempty"`
}

// parseStringList accepts a YAML value that may be a string, []string, or nil,
// and returns a clean []string. Commas in a plain string are split.
func parseStringList(v any) []string {
	if v == nil {
		return nil
	}
	switch t := v.(type) {
	case string:
		if t == "" {
			return nil
		}
		var out []string
		for _, part := range strings.Split(t, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				out = append(out, part)
			}
		}
		return out
	case []string:
		return t
	case []interface{}:
		out := make([]string, 0, len(t))
		for _, x := range t {
			if s, ok := x.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// loadComponents reads all .md files from dir, parses their frontmatter,
// extracts and compiles pattern regexes, and returns a slice of Component.
// Files that fail to parse are skipped silently.
func loadComponents(dir string, kind ComponentKind) ([]*Component, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var components []*Component
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		// Eligibility gate: must be a Markdown definition file. README.md
		// and other documentation entries are rejected unconditionally —
		// matches the manager loaders so the router and CLI see the same
		// vocabulary. A README carrying valid-looking frontmatter (e.g.
		// `name: README`) must never become a routing target.
		if !IsDefinitionFile(entry.Name()) {
			continue
		}

		comp, err := parseComponentFile(filepath.Join(dir, entry.Name()), kind)
		if err != nil {
			continue // skip malformed files silently
		}
		if comp == nil {
			continue
		}
		components = append(components, comp)
	}
	return components, nil
}

// parseComponentFile reads one .md file, extracts frontmatter patterns and
// anti-patterns, adds implicit patterns from body text, compiles regexes,
// and returns a Component. Returns nil if the file has no usable content.
func parseComponentFile(path string, kind ComponentKind) (*Component, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	content := string(data)
	var frontmatter string
	var body string

	if strings.HasPrefix(content, "---") {
		parts := strings.SplitN(content[3:], "---", 2)
		if len(parts) >= 2 {
			frontmatter = strings.TrimSpace(parts[0])
			body = strings.TrimSpace(parts[1])
		}
	}
	_ = body // body parsed for schema symmetry; not used for routing (deprecated 2026-06-26, decision be61de1c4ef2ff4a)

	var fm frontmatterSchema
	explicitCount := 0
	if frontmatter != "" {
		if err := yaml.Unmarshal([]byte(frontmatter), &fm); err != nil {
			return nil, err
		}
	}

	name := fm.Name
	if name == "" {
		// Fall back to filename stem (mode/write.md → name="write")
		name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}

	// 1. Explicit patterns from frontmatter (weight boosted in scoreComponent).
	patternList := parseStringList(fm.Patterns)
	var compiled []*regexp.Regexp
	for _, p := range patternList {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		re, err := regexp.Compile(`(?i)` + p)
		if err != nil {
			continue
		}
		compiled = append(compiled, re)
	}
	explicitCount = len(compiled)

	// 2. Body text is voice only. Deprecated 2026-06-26 (decision be61de1c4ef2ff4a):
	//    body-word inference was treated as implicit routing signal, which caused
	//    persona/mode over-firing on common prose words. Patterns: frontmatter is
	//    now the only routing signal. A component without `patterns:` is invisible
	//    to the auto-router. See lesson 853d678719f905d7 for the failure mode.

	// 3. domain_out from frontmatter (renamed from anti_patterns 2026-06-26).
	//    These ARE input filters — compiled to regex, applied as -1 per match
	//    in scoreComponent. Voice guards (the old anti_patterns content) are
	//    NOT compiled and NOT used for routing; they live in the frontmatter
	//    for LLM context only.
	domainOutList := parseStringList(fm.DomainOut)
	var domainOutCompiled []*regexp.Regexp
	for _, d := range domainOutList {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		// domain_out uses word-boundary matching, just like anti_patterns did.
		re, err := regexp.Compile(`(?i)\b` + regexp.QuoteMeta(d) + `\b`)
		if err != nil {
			continue
		}
		domainOutCompiled = append(domainOutCompiled, re)
	}

	// 4. voice_guards from frontmatter. NOT compiled. Stored as raw string
	//    for the LLM's context window (so the persona can self-check at
	//    generation time). Routing layer ignores this field entirely.
	var voiceGuards string
	switch t := fm.VoiceGuards.(type) {
	case string:
		voiceGuards = t
	case []interface{}:
		parts := make([]string, 0, len(t))
		for _, x := range t {
			if s, ok := x.(string); ok && s != "" {
				parts = append(parts, s)
			}
		}
		voiceGuards = strings.Join(parts, ", ")
	}

	return &Component{
		Name:             name,
		Kind:             kind,
		Patterns:         compiled,
		DomainOut:        domainOutCompiled,
		VoiceGuards:      voiceGuards,
		explicitPatterns: explicitCount,
	}, nil
}

// extractBodyPatterns — REMOVED 2026-06-26 (decision be61de1c4ef2ff4a).
// Body-word inference caused false-positive over-firing (e.g. artisan scoring
// on every prompt containing "does", "code", "function", "fix", "works" —
// common prose words that happened to appear in the persona body).
// Patterns: frontmatter is now the only routing signal. A persona/mode file
// without `patterns:` is invisible to the auto-router and can only be invoked
// manually via `mpm ops switch` or `mpm ops stance assume`.