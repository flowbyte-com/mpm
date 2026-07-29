package internal

// changelog.go defines the schema and renderer for MPM's CHANGELOG.md
// and the changelog.json sibling. The Markdown file is for humans
// reading releases; the JSON sibling is for agents (and the eventual
// synthesis engine) that need structured access.
//
// The schema is shaped to make the synthesis engine a pure function:
// every entry carries a CommitHash so the future MPM-memory join is a
// mathematically certain (commit_hash, mpm_memory_id) merge. Entries
// from the git log leave MPMMemoryIDs empty; entries that the future
// log_to_changelog MCP tool produces fill it in.
//
// This file deliberately depends on nothing project-specific beyond
// the Go standard library, so it can be unit-tested in isolation and
// imported by the CLI subcommand without dragging in the rest of
// internal/. Future: if the synthesis engine needs to read MPM
// memories directly, that code lives here too, behind a separate
// function that takes a slice of pre-loaded memories.

import (
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ChangelogEntry is one row in a release section. For git-sourced
// entries (the only kind this commit produces) the CommitHash is the
// authoritative identity; MPMMemoryIDs stays empty. Future
// log_to_changelog entries will have CommitHash empty and a populated
// MPMMemoryIDs — the synthesis engine reconciles both kinds.
type ChangelogEntry struct {
	// Version is the release this entry belongs to, e.g. "1.1.0".
	Version string `json:"version"`
	// Date is the release date in YYYY-MM-DD form.
	Date string `json:"date"`
	// CommitHash is the SHA1 of the commit (git-sourced entries) or
	// empty (agent-sourced entries that pre-date commit).
	CommitHash string `json:"commit_hash"`
	// Author is the commit author line (git log %an).
	Author string `json:"author"`
	// Type is the conventional-commit prefix: feat, fix, refactor,
	// chore, docs, perf, test, build, ci, style, revert. Empty for
	// hand-written release-highlight entries.
	Type string `json:"type"`
	// Scope is the parenthesised scope: feat(reference) -> "reference".
	// Empty when absent.
	Scope string `json:"scope,omitempty"`
	// Summary is the commit subject line, conventional prefix stripped.
	Summary string `json:"summary"`
	// Body is the commit body (everything after the subject). Empty
	// for single-line commits. Useful for surfacing "why" in the
	// changelog without re-deriving it from MPM memory.
	Body string `json:"body,omitempty"`
	// MPMMemoryIDs is the join key for the future synthesis engine.
	// Git-sourced entries leave this empty; agent-sourced entries
	// populated via the log_to_changelog MCP tool will fill it.
	// Resolved by the synthesis engine as
	//   for entry in entries:
	//       if entry.CommitHash != "":
	//           join MPMMemoryIDs[entry.CommitHash] into entry.MPMMemoryIDs
	//       for id in entry.MPMMemoryIDs:
	//           join prose from MPM memory id into entry.Body
	MPMMemoryIDs []string `json:"mpm_memory_ids"`
	// Highlight is true for hand-written release-highlight entries
	// (the "this is what matters about 1.1.0" prose under a release
	// header). The renderer uses Highlight to choose prose styling.
	Highlight bool `json:"highlight,omitempty"`
}

// ChangelogDocument is the full file. One ChangelogDocument serializes
// to both CHANGELOG.md (via RenderMarkdown) and changelog.json (via
// the standard library's encoding/json).
type ChangelogDocument struct {
	// Project is the project name in the header line. Defaults to
	// "MPM" when empty.
	Project string `json:"project"`
	// Releases are the versioned release sections, newest first.
	Releases []ChangelogRelease `json:"releases"`
	// Legacy is the unedited backfill section. Renders as a single
	// release with Highlight=false and Type="" entries — bullet
	// points only, no prose interpretation.
	Legacy *ChangelogRelease `json:"legacy,omitempty"`
}

// ChangelogRelease is one versioned block. The entries inside are
// split by Type into Keep-a-Changelog-style subsections (Added,
// Changed, Fixed, Removed) at render time.
type ChangelogRelease struct {
	Version string            `json:"version"`
	Date    string            `json:"date"`
	Notes   string            `json:"notes,omitempty"`
	Entries []ChangelogEntry  `json:"entries"`
}

// conventionalCommitRe matches the conventional commit shape:
//   <type>[(scope)]: <summary>
//   <body lines>
//
// type is one of the canonical prefixes. scope may contain slashes
// (e.g. "reference/ingest"). summary is captured greedily up to the
// end of the first line. Body lines follow on subsequent lines.
var conventionalCommitRe = regexp.MustCompile(
	`^(?P<type>feat|fix|refactor|perf|docs|test|build|ci|style|chore|revert)(?:\((?P<scope>[^)]+)\))?(?P<bang>!)?: (?P<summary>.+)$`,
)

// knownSectionOrder is the keep-a-changelog subsection order. Types
// outside this list are surfaced under "Other" so a new conventional
// prefix never silently disappears.
var knownSectionOrder = []string{
	"Added",     // feat
	"Changed",   // refactor, perf
	"Deprecated",
	"Removed",   // revert
	"Fixed",     // fix
	"Security",
	"Other",     // docs, test, build, ci, style, chore, unknown
}

// typeToSection maps conventional-commit type to the human-facing
// section header. Keep-a-Changelog has no "docs/test/build/ci/style/
// chore" sections natively — these land in "Other" so the human
// reader sees the work but is not misled by the absence of a docs
// section meaning "no documentation changed".
var typeToSection = map[string]string{
	"feat":     "Added",
	"refactor": "Changed",
	"perf":     "Changed",
	"fix":      "Fixed",
	"revert":   "Removed",
	"docs":     "Other",
	"test":     "Other",
	"build":    "Other",
	"ci":       "Other",
	"style":    "Other",
	"chore":    "Other",
}

// ParseCommitLog parses the stdout of `git log` with the
// conventional-commit-friendly format:
//
//   %H%x1f%an%x1f%s%x1f%b%x1e
//
// (hash, author, subject, body, record-separator) where \x1f is the
// ASCII unit separator and \x1e is the ASCII record separator. This
// format is unambiguous for the body because bodies can contain any
// character, including newlines. The function returns one
// ChangelogEntry per commit, in the order the input supplies (newest
// first, since that is what git log produces by default).
//
// Non-conventional commits are kept as entries with Type="" so they
// appear under "Other" and are never silently dropped.
func ParseCommitLog(raw string) []ChangelogEntry {
	var entries []ChangelogEntry
	for _, block := range strings.Split(raw, "\x1e") {
		block = strings.TrimRight(block, "\n")
		if block == "" {
			continue
		}
		parts := strings.SplitN(block, "\x1f", 4)
		if len(parts) < 3 {
			continue
		}
		hash, author, subject := parts[0], parts[1], parts[2]
		body := ""
		if len(parts) == 4 {
			body = strings.TrimSpace(parts[3])
		}

		entry := ChangelogEntry{
			CommitHash:   strings.TrimSpace(hash),
			Author:       strings.TrimSpace(author),
			Body:         body,
			MPMMemoryIDs: []string{},
		}

		// Try to match the conventional-commit shape on the subject
		// line. If it matches, split type/scope/summary. If it
		// doesn't, leave Type empty and use the whole subject as
		// Summary — the entry still appears in the output under
		// "Other".
		if m := conventionalCommitRe.FindStringSubmatch(subject); m != nil {
			entry.Type = m[1]
			entry.Scope = m[2]
			entry.Summary = strings.TrimSpace(m[4])
		} else {
			entry.Summary = strings.TrimSpace(subject)
		}

		entries = append(entries, entry)
	}
	return entries
}

// gitRefPattern validates git ref arguments against injection.
var gitRefPattern = regexp.MustCompile(`^[a-zA-Z0-9._\-/@]+$`)

// validateGitRef returns an error for refs with unsafe characters.
func validateGitRef(ref string) error {
	if ref == "" {
		return nil
	}
	if !gitRefPattern.MatchString(ref) {
		return fmt.Errorf("invalid git ref: %q contains unsafe characters", ref)
	}
	return nil
}

// GitLogOptions controls how the changelog tool invokes git.
type GitLogOptions struct {
	// RepoDir is the working directory for git invocations. Empty
	// means the current process working directory.
	RepoDir string
	// Since is the lower bound for the log range. Format: tag, sha,
	// or date understood by git (e.g. "v1.0.0", "abc1234",
	// "2026-01-01"). Empty means "no lower bound" (used for legacy
	// backfill mode).
	Since string
	// Until is the upper bound. Empty means HEAD.
	Until string
	// Reverse, when true, walks commits oldest-first. Used by legacy
	// backfill so the backfill section reads chronologically.
	Reverse bool
}

// FetchGitLog invokes `git log` with the given options and returns
// the raw output in the format ParseCommitLog expects. Centralising
// the invocation here keeps the rest of the package testable without
// shelling out.
func FetchGitLog(opts GitLogOptions) (string, error) {
	if err := validateGitRef(opts.Since); err != nil {
		return "", err
	}
	if err := validateGitRef(opts.Until); err != nil {
		return "", err
	}

	args := []string{
		"log",
		"--no-merges",
		"--pretty=format:%H%x1f%an%x1f%s%x1f%b%x1e",
	}
	if opts.Reverse {
		args = append(args, "--reverse")
	}
	if opts.Since != "" {
		args = append(args, opts.Since+"..HEAD")
		if opts.Until != "" {
			args[len(args)-1] = opts.Since + ".." + opts.Until
		}
	} else if opts.Until != "" {
		args = append(args, opts.Until)
	}

	cmd := exec.Command("git", args...)
	if opts.RepoDir != "" {
		cmd.Dir = opts.RepoDir
	}
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git log: %w", err)
	}
	return string(out), nil
}

// LatestTag returns the most recent git tag reachable from HEAD, or
// ("", nil) if no tags exist. Used as the default lower bound for
// "mpm ops changelog build" so the tool is zero-arg-usable in the
// common case.
func LatestTag(repoDir string) (string, error) {
	args := []string{"describe", "--tags", "--abbrev=0"}
	cmd := exec.Command("git", args...)
	if repoDir != "" {
		cmd.Dir = repoDir
	}
	out, err := cmd.Output()
	if err != nil {
		// `git describe` returns non-zero when no tags exist. Treat
		// that as "no previous tag" rather than an error.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 128 {
			return "", nil
		}
		return "", fmt.Errorf("git describe: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// GroupBySection partitions entries by Keep-a-Changelog section.
// Returns a map from section name to entries, with sections in the
// canonical order. Empty sections are omitted from the map.
func GroupBySection(entries []ChangelogEntry) map[string][]ChangelogEntry {
	groups := make(map[string][]ChangelogEntry)
	for _, e := range entries {
		section, ok := typeToSection[e.Type]
		if !ok {
			section = "Other"
		}
		groups[section] = append(groups[section], e)
	}
	return groups
}

// RenderMarkdown renders the document as Keep-a-Changelog Markdown.
// Sections within a release are emitted in canonical order. Legacy
// backfill releases render as a flat bullet list with no subsections.
func (d *ChangelogDocument) RenderMarkdown() string {
	var b strings.Builder

	project := d.Project
	if project == "" {
		project = "MPM"
	}
	fmt.Fprintf(&b, "# %s Changelog\n\n", project)
	b.WriteString("All notable changes to this project are documented here. ")
	b.WriteString("The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) ")
	b.WriteString("and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).\n\n")

	// Newest first across all releases.
	for _, rel := range d.Releases {
		renderRelease(&b, rel)
	}
	if d.Legacy != nil {
		renderLegacy(&b, *d.Legacy)
	}
	return b.String()
}

func renderRelease(b *strings.Builder, rel ChangelogRelease) {
	heading := fmt.Sprintf("## [%s] - %s", rel.Version, rel.Date)
	if rel.Date == "" {
		heading = fmt.Sprintf("## [%s]", rel.Version)
	}
	fmt.Fprintln(b, heading)
	fmt.Fprintln(b)

	// Highlight prose (hand-written release summary) lives at the
	// top of the section. Render it as a quote-block.
	for _, e := range rel.Entries {
		if e.Highlight {
			for _, line := range strings.Split(e.Body, "\n") {
				fmt.Fprintf(b, "> %s\n", line)
			}
		}
	}
	if rel.Notes != "" {
		for _, line := range strings.Split(rel.Notes, "\n") {
			fmt.Fprintf(b, "> %s\n", line)
		}
		fmt.Fprintln(b)
	}

	groups := GroupBySection(rel.Entries)
	for _, section := range knownSectionOrder {
		entries, ok := groups[section]
		if !ok || len(entries) == 0 {
			continue
		}
		fmt.Fprintf(b, "### %s\n\n", section)
		for _, e := range entries {
			if e.Highlight {
				continue
			}
			renderEntry(b, e)
		}
		fmt.Fprintln(b)
	}
}

func renderEntry(b *strings.Builder, e ChangelogEntry) {
	scope := ""
	if e.Scope != "" {
		scope = fmt.Sprintf("**%s:** ", e.Scope)
	}
	sha := ""
	if e.CommitHash != "" {
		short := e.CommitHash
		if len(short) > 7 {
			short = short[:7]
		}
		sha = fmt.Sprintf(" ([%s](https://github.com/example/mpm/commit/%s))", short, e.CommitHash)
	}
	fmt.Fprintf(b, "- %s%s%s\n", scope, e.Summary, sha)
	// Body, if present, renders as a blockquote directly beneath
	// the bullet. This is the synthesis engine's prose channel:
	// git-sourced entries have empty Body, so the bullet stands
	// alone; entries that were matched against a #changelog
	// memory get the agent's prose rendered as blockquoted text.
	// Each line of Body is prefixed with "> " so multi-line
	// prose stacks correctly.
	if e.Body != "" {
		for _, line := range strings.Split(e.Body, "\n") {
			fmt.Fprintf(b, "  > %s\n", line)
		}
	}
}

func renderLegacy(b *strings.Builder, rel ChangelogRelease) {
	heading := fmt.Sprintf("## [%s] - %s", rel.Version, rel.Date)
	if rel.Date == "" {
		heading = fmt.Sprintf("## [%s]", rel.Version)
	}
	fmt.Fprintln(b, heading)
	fmt.Fprintln(b)
	b.WriteString("> **Legacy backfill.** Raw commit log dump, unedited. ")
	b.WriteString("Future cleanup passes will group these into the standard sections.\n\n")

	// Sort entries by commit date so the section reads
	// chronologically. We don't have author date in the parsed entry,
	// so we sort by CommitHash as a stable proxy (lexicographic on
	// sha1 is deterministic and roughly correlates with time).
	sorted := append([]ChangelogEntry(nil), rel.Entries...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].CommitHash < sorted[j].CommitHash
	})
	for _, e := range sorted {
		sha := ""
		if e.CommitHash != "" {
			short := e.CommitHash
			if len(short) > 7 {
				short = short[:7]
			}
			sha = fmt.Sprintf(" ([%s](https://github.com/example/mpm/commit/%s))", short, e.CommitHash)
		}
		summary := e.Summary
		if summary == "" {
			summary = "(no subject)"
		}
		fmt.Fprintf(b, "- %s%s\n", summary, sha)
	}
	fmt.Fprintln(b)
}

// Today returns the current date in YYYY-MM-DD. Centralised so tests
// can stub it if needed.
func Today() string {
	return time.Now().UTC().Format("2006-01-02")
}
