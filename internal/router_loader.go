package internal

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// frontmatterSchema handles both comma-separated string and []string shapes
// for patterns/anti_patterns fields. YAML.v3 unmarshals either into any.
type frontmatterSchema struct {
	Name         string `yaml:"name"`
	Patterns     any    `yaml:"patterns,omitempty"`
	AntiPatterns any    `yaml:"anti_patterns,omitempty"`
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
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
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

	// 3. Anti-patterns from frontmatter.
	antiList := parseStringList(fm.AntiPatterns)
	var antiCompiled []*regexp.Regexp
	for _, ap := range antiList {
		ap = strings.TrimSpace(ap)
		if ap == "" {
			continue
		}
		// Anti-patterns use word-boundary matching.
		re, err := regexp.Compile(`(?i)\b` + regexp.QuoteMeta(ap) + `\b`)
		if err != nil {
			continue
		}
		antiCompiled = append(antiCompiled, re)
	}

	return &Component{
		Name:             name,
		Kind:             kind,
		Patterns:         compiled,
		AntiPatterns:     antiCompiled,
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