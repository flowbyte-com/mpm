package internal

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// LintIssue represents a single problem found in a router component file.
// Issues are categorized so the linter can report scope and let callers
// decide exit code (warn vs fail).
type LintIssue struct {
	File    string // absolute path
	Field   string // "patterns", "domain_out", "voice_guards", "yaml", or "" for whole-file issues
	Pattern string // the specific bad string, if applicable
	Message string
}

// LintReport is the structured output of LintRouterDirectories.
type LintReport struct {
	FilesScanned int
	Issues       []LintIssue
}

// OK returns true if no issues were found.
func (r *LintReport) OK() bool {
	return len(r.Issues) == 0
}

// LintRouterDirectories scans every .md file under each of the given
// directories and reports (a) YAML parse errors in frontmatter,
// (b) regex compile errors in the `patterns` field,
// (c) regex compile errors in the `domain_out` field.
// Voice guards are NOT compiled (they are descriptive prose for LLM
// context), so they are not checked at the regex level — but the YAML
// parse of the frontmatter catches malformed voice_guards strings.
//
// This is the proactive defense against the YAML escape footgun family
// discovered 2026-06-26: \b in double-quoted YAML silently becomes a
// backspace character; \' inside single-quoted YAML breaks the parser;
// misaligned quotes or commas break compilation. All three fail
// silently at load time (the persona/mode just doesn't match anything).
// Catching them at lint time means a CI/pre-commit run fails loudly
// before the broken file is ever loaded by the runtime router.
func LintRouterDirectories(dirs ...string) LintReport {
	report := LintReport{}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			report.Issues = append(report.Issues, LintIssue{
				File:    dir,
				Message: fmt.Sprintf("cannot read directory: %v", err),
			})
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			report.FilesScanned++
			report.Issues = append(report.Issues, lintFile(path)...)
		}
	}
	return report
}

// lintFile reads one .md file and returns any lint issues. The file must
// have YAML frontmatter delimited by --- on its own lines. Both shapes
// of patterns/domain_out are supported: comma-separated single-quoted
// string, comma-separated double-quoted string, YAML list.
func lintFile(path string) []LintIssue {
	data, err := os.ReadFile(path)
	if err != nil {
		return []LintIssue{{File: path, Message: fmt.Sprintf("cannot read: %v", err)}}
	}

	content := string(data)
	if !strings.HasPrefix(content, "---") {
		// No frontmatter — that's allowed (mode/standard.md historically
		// has empty frontmatter). Nothing to lint.
		return nil
	}
	parts := strings.SplitN(content[3:], "---", 2)
	if len(parts) < 2 {
		return []LintIssue{{File: path, Message: "frontmatter not closed (missing second ---)"}}
	}
	frontmatter := strings.TrimSpace(parts[0])

	var fm frontmatterSchema
	if err := yaml.Unmarshal([]byte(frontmatter), &fm); err != nil {
		return []LintIssue{{
			File:    path,
			Field:   "yaml",
			Message: fmt.Sprintf("YAML parse error: %v", err),
		}}
	}

	var issues []LintIssue

	// Check each pattern field. The fields are stored in the schema as
	// `any` because YAML can decode either string or []string. We pull
	// them out by re-decoding from the raw frontmatter to avoid the
	// type-coercion the schema does. But for the lint, parseStringList
	// is good enough — it returns the canonical []string form.

	for _, field := range []string{"patterns", "domain_out"} {
		// We re-fetch from the raw frontmatter to keep field names
		// accurate in error messages.
		patterns := parseStringList(getFieldRaw(frontmatter, field))
		// If parseStringList returned nil because the field isn't
		// present, fall back to the schema's parsed value (handles
		// the case where YAML decoded it as []string directly).
		if patterns == nil {
			switch field {
			case "patterns":
				patterns = parseStringList(fm.Patterns)
			case "domain_out":
				patterns = parseStringList(fm.DomainOut)
			}
		}
		for _, p := range patterns {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			// Patterns and domain_out are compiled with (?i) prefix
			// by the router. domain_out additionally gets \b...\b
			// wrapping (word boundaries) AND regexp.QuoteMeta on the
			// raw string. The linter must mirror the EXACT compilation
			// shape the router uses, otherwise it would accept strings
			// the runtime would reject (or reject strings the runtime
			// would accept).
			var fullPattern string
			if field == "domain_out" {
				fullPattern = `(?i)\b` + regexp.QuoteMeta(p) + `\b`
			} else {
				fullPattern = `(?i)` + p
			}
			if _, err := regexp.Compile(fullPattern); err != nil {
				issues = append(issues, LintIssue{
					File:    path,
					Field:   field,
					Pattern: p,
					Message: fmt.Sprintf("regex compile error: %v (compiled as %q)", err, fullPattern),
				})
			}

			// SEMANTIC CHECK (stricter than the router): also compile
			// the raw string the user wrote, with NO QuoteMeta. The
			// router uses QuoteMeta which silently fixes unbalanced
			// brackets (e.g. "broken[unclosed" compiles to
			// "broken\[unclosed" which never matches anything). This
			// catches the bug class where QuoteMeta masks a typo.
			//
			// False positive risk: users writing intentional regex
			// syntax like [Rr]ust or [0-9]+ will FAIL this check too.
			// That's acceptable — those users should add a comment in
			// their frontmatter documenting intent, OR move the
			// pattern to `patterns:` (which doesn't get QuoteMeta).
			//
			// domain_out is meant for prompt-vocabulary phrases
			// (prose), so the raw-form compile catches the typo class
			// without false positives for legitimate regex users.
			if field == "domain_out" {
				rawFull := `(?i)\b` + p + `\b`
				if _, err := regexp.Compile(rawFull); err != nil {
					issues = append(issues, LintIssue{
						File:    path,
						Field:   field,
						Pattern: p,
						Message: fmt.Sprintf("raw-form compile error (QuoteMeta would mask this at runtime): %v (compiled as %q)", err, rawFull),
					})
				}
			}
		}
	}

	return issues
}

// getFieldRaw looks up a top-level key in a YAML frontmatter string and
// returns its raw value (could be string, list, scalar, etc.). We use a
// fresh yaml.Node decode to preserve the original shape — parseStringList
// handles the rest. Returns nil if the field is not present.
func getFieldRaw(frontmatter, key string) any {
	var node map[string]yaml.Node
	if err := yaml.Unmarshal([]byte(frontmatter), &node); err != nil {
		return nil
	}
	n, ok := node[key]
	if !ok {
		return nil
	}
	// Decode the node back to any so parseStringList can switch on type.
	var v any
	if err := n.Decode(&v); err != nil {
		return nil
	}
	return v
}
