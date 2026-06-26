package main

import (
	"fmt"
	"os"
	"path/filepath"

	mpminternal "mpm/internal"
)

// handleLint implements `mpm ops lint` — proactive defense against the
// YAML escape footgun family documented in lesson 9e1b2142bd525c6d.
//
// Checks (per v's spec, 2026-06-26):
//   1. YAML validity — frontmatter must parse without error
//   2. Regex compilation — every string in `patterns` and `domain_out`
//      must compile via regexp.Compile when prefixed with (?i) (and with
//      \b...\b wrapping for domain_out, matching router_loader.go)
//
// Voice guards are NOT regex-compiled (they are LLM-context prose), so
// they are not checked here. But YAML parse errors in voice_guards
// strings are still caught by check #1.
//
// Exit codes:
//   0 — clean (no issues)
//   1 — issues found, lint failed
//   2 — operational error (cannot read directory, etc.)
func handleLint(args []string) int {
	dirs := []string{}
	strict := false
	showClean := false

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--dir", "-d":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "Error: --dir requires a path argument")
				return 2
			}
			i++
			dirs = append(dirs, args[i])
		case "--strict":
			strict = true
		case "--show-clean":
			showClean = true
		case "--help", "-h":
			fmt.Println(`mpm ops lint — validate persona/mode router frontmatter

Usage:
  mpm ops lint [--dir <path>]... [--strict] [--show-clean]

Checks:
  1. YAML frontmatter in every .md file must parse without error
  2. Every entry in "patterns:" and "domain_out:" must compile via
     regexp.Compile with the same (?i) prefix and word-boundary wrapping
     the router uses at load time

Defaults:
  Scans ~/.mpm/persona/ and ~/.mpm/mode/ if no --dir is given

Flags:
  --dir <path>     Add a directory to scan (can be repeated)
  --strict         Exit non-zero on any issue (default: same)
  --show-clean     Print OK summary even if no issues found
  --help           Show this help

Exit codes:
  0  clean (no issues)
  1  issues found — see output above
  2  operational error (cannot read directory, etc.)

See lesson 9e1b2142bd525c6d for the YAML escape footgun family this
linter defends against.`)
			return 0
		default:
			fmt.Fprintf(os.Stderr, "Error: unknown flag %q\n", a)
			return 2
		}
	}

	// Default directories
	if len(dirs) == 0 {
		mpmDir := os.Getenv("MPM_DIR")
		if mpmDir == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: cannot determine home dir: %v\n", err)
				return 2
			}
			mpmDir = filepath.Join(home, ".mpm")
		}
		dirs = []string{
			filepath.Join(mpmDir, "persona"),
			filepath.Join(mpmDir, "mode"),
		}
	}

	// Verify all dirs exist before scanning
	for _, d := range dirs {
		if info, err := os.Stat(d); err != nil {
			fmt.Fprintf(os.Stderr, "Error: cannot stat %q: %v\n", d, err)
			return 2
		} else if !info.IsDir() {
			fmt.Fprintf(os.Stderr, "Error: %q is not a directory\n", d)
			return 2
		}
	}

	report := mpminternal.LintRouterDirectories(dirs...)

	if report.OK() {
		if showClean {
			fmt.Printf("✓ Lint clean: %d files scanned, 0 issues\n", report.FilesScanned)
			for _, d := range dirs {
				fmt.Printf("  %s\n", d)
			}
		}
		return 0
	}

	fmt.Printf("✗ Lint failed: %d issues across %d files scanned\n\n", len(report.Issues), report.FilesScanned)

	// Group issues by file for readable output
	byFile := map[string][]mpminternal.LintIssue{}
	for _, issue := range report.Issues {
		byFile[issue.File] = append(byFile[issue.File], issue)
	}

	for _, file := range sortedKeys(byFile) {
		fmt.Printf("  %s\n", file)
		for _, issue := range byFile[file] {
			field := issue.Field
			if field == "" {
				field = "(file)"
			}
			if issue.Pattern != "" {
				fmt.Printf("    field=%s pattern=%q\n      %s\n", field, issue.Pattern, issue.Message)
			} else {
				fmt.Printf("    field=%s\n      %s\n", field, issue.Message)
			}
		}
	}

	if strict {
		// strict mode is the default; reserved for future stricter checks
	}
	return 1
}

// sortedKeys returns map keys sorted alphabetically. Used for
// deterministic linter output (matches the alphabetical ReadDir order
// the linter already uses internally, but keeps the formatting layer
// independent of the scan layer).
func sortedKeys(m map[string][]mpminternal.LintIssue) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// simple insertion sort — small N (file count), avoids import
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return keys
}
