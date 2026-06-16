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

	// 2. Implicit patterns from body text (non-trivial content words).
	//    These capture topical keywords from the actual mode/persona description
	//    without requiring an explicit `patterns:` field in every file.
	bodyPatterns := extractBodyPatterns(body)
	for _, p := range bodyPatterns {
		re, err := regexp.Compile(`(?i)\b` + regexp.QuoteMeta(p) + `\b`)
		if err != nil {
			continue
		}
		compiled = append(compiled, re)
	}

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

// extractBodyPatterns returns significant words (3+ chars, non-stop-word)
// extracted from body text. These serve as implicit positive triggers when
// no explicit patterns: field exists in the frontmatter.
func extractBodyPatterns(body string) []string {
	words := strings.FieldsFunc(body, func(r rune) bool {
		return r == ' ' || r == '\n' || r == '\t' || r == ',' || r == '.' ||
			r == '!' || r == '?' || r == ':' || r == ';' || r == '"' ||
			r == '\'' || r == '(' || r == ')' || r == '[' || r == ']' ||
			r == '{' || r == '}' || r == '#' || r == '*' || r == '-' ||
			r == '/' || r == '\\' || r == '|' || r == '`' || r == '~'
	})

	stop := map[string]bool{
		"the": true, "and": true, "for": true, "that": true, "this": true,
		"with": true, "from": true, "you": true, "are": true, "was": true,
		"were": true, "been": true, "have": true, "has": true, "had": true,
		"will": true, "would": true, "could": true, "should": true, "may": true,
		"might": true, "can": true, "not": true, "but": true, "its": true,
		"also": true, "into": true, "when": true, "then": true, "than": true,
		"what": true, "which": true, "their": true, "there": true, "they": true,
		"them": true, "your": true, "our": true, "all": true, "each": true,
		"every": true, "both": true, "few": true, "more": true, "most": true,
		"other": true, "some": true, "such": true, "only": true, "own": true,
		"same": true, "so": true, "very": true, "just": true, "about": true,
		"after": true, "before": true, "because": true, "being": true, "between": true,
		"even": true, "how": true, "like": true, "make": true, "many": true,
		"now": true, "one": true, "out": true, "said": true, "two": true,
		"up": true, "way": true, "well": true, "who": true, "work": true,
	}

	var out []string
	seen := make(map[string]bool)
	for _, w := range words {
		lower := strings.ToLower(w)
		if len(lower) < 3 || stop[lower] || seen[lower] {
			continue
		}
		seen[lower] = true
		out = append(out, lower)
	}
	return out
}