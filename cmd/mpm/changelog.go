package main

// changelog.go implements `mpm ops changelog build` — the
// scaffolding for MPM's release notes. The subcommand reads the git
// log since the previous tag (or a custom boundary), groups commits
// by conventional-commit prefix, and emits both CHANGELOG.md (human)
// and changelog.json (machine, input to the --with-synthesis join).
//
// Flags:
//   --since <ref>      Lower bound (tag, sha, or date). Default: latest tag.
//   --until <ref>      Upper bound. Default: HEAD.
//   --version <v>      Version string for the new release. Default: derived
//                      from the previous tag via semver bump rules.
//   --date <YYYY-MM-DD> Release date. Default: today (UTC).
//   --output <path>    Where to write CHANGELOG.md. Default: ./CHANGELOG.md
//   --json <path>      Where to write changelog.json. Default: ./changelog.json
//   --legacy           Emit the commits before --since as a single
//                      unedited ## [1.0.1] - Legacy Backfill section.
//                      Idempotent: re-running with the same --since
//                      reproduces the same backfill.
//   --dry-run          Print the generated Markdown to stdout instead
//                      of writing files.
//
// Design note: this tool does NOT delete or rewrite any existing
// CHANGELOG.md content. It rebuilds the file from scratch every time,
// so the operator is responsible for hand-editing the 1.1.0 entry
// after a release (e.g., adding release-highlight prose). To preserve
// hand-written notes across runs, edit the file in place and the
// next build will still respect the structure; the Highlight flag
// in the entry type tells the renderer where prose lives.

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/usererror"
)

// handleOpsChangelog is the entry point for `mpm ops changelog build`.
// It parses flags, fetches the git log, groups commits, and writes
// CHANGELOG.md + changelog.json. Returns 0 on success, 1 on error.
func handleOpsChangelogRoute(args []string) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Println("Usage: mpm ops changelog <subcommand>")
		fmt.Println()
		fmt.Println("Subcommands:")
		fmt.Println("  build [flags]    Generate CHANGELOG.md + changelog.json from git log")
		return 0
	}
	switch args[0] {
	case "build":
		return handleOpsChangelog(args[1:])
	default:
		usererror.Warn("changelog: unknown subcommand %q", args[0])
		fmt.Fprintln(os.Stderr, "Run `mpm ops changelog help` for usage.")
		return 1
	}
}

func handleOpsChangelog(args []string) int {
	// Global parseFlags rewrites --help -> "help" before we see it, so
	// check the first arg for that alias too. If the user asked for
	// help (or supplied nothing), show the build flag help and exit.
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		// We have to set up the flag set first because the help
		// text uses fs.PrintDefaults() so adding a new flag auto-
		// appears in the help without manually updating this list.
		helpFS := flag.NewFlagSet("ops changelog build help", flag.ContinueOnError)
		helpFS.String("since", "", "Lower bound: tag, sha, or date (default: latest tag)")
		helpFS.String("until", "", "Upper bound: tag, sha, or date (default: HEAD)")
		helpFS.String("release-version", "", "Version string (default: minor bump of --since)")
		helpFS.String("date", "", "Release date (default: today UTC)")
		helpFS.String("output", "CHANGELOG.md", "Markdown output path (default: CHANGELOG.md)")
		helpFS.String("json", "changelog.json", "JSON sibling output path (default: changelog.json)")
		helpFS.String("project", "MPM", "Project name in changelog header (default: MPM)")
		helpFS.Bool("legacy", false, "Emit pre-since commits as a Legacy Backfill section")
		helpFS.String("repo", "", "Git repo directory (default: cwd)")
		helpFS.Bool("dry-run", false, "Print Markdown to stdout; do not write files")
		helpFS.String("release-notes", "", "Path to a file of hand-written highlights (blockquote at top of release)")
		helpFS.Bool("with-synthesis", false, "Join git-sourced entries with agent-written #changelog memories; merged prose renders as blockquote under each commit bullet; orphans surface in a dedicated section + terminal warning")
		fmt.Println("Usage: mpm ops changelog build [flags]")
		fmt.Println()
		fmt.Println("Generates CHANGELOG.md + changelog.json from the git log.")
		fmt.Println()
		fmt.Println("Flags:")
		helpFS.PrintDefaults()
		return 0
	}

	fs := flag.NewFlagSet("ops changelog build", flag.ContinueOnError)
	since := fs.String("since", "", "Lower bound: tag, sha, or date (default: latest tag)")
	until := fs.String("until", "", "Upper bound: tag, sha, or date (default: HEAD)")
	version := fs.String("release-version", "", "Version string (default: auto-bump from previous tag)")
	date := fs.String("date", "", "Release date YYYY-MM-DD (default: today UTC)")
	output := fs.String("output", "CHANGELOG.md", "Markdown output path")
	jsonOutput := fs.String("json", "changelog.json", "JSON sibling output path")
	projectName := fs.String("project", "MPM", "Project name in changelog header")
	legacy := fs.Bool("legacy", false, "Emit pre-since commits as a Legacy Backfill section")
	repoDir := fs.String("repo", "", "Git repo directory (default: cwd)")
	dryRun := fs.Bool("dry-run", false, "Print Markdown to stdout; do not write files")
	notesFile := fs.String("release-notes", "", "Path to a file containing hand-written release highlights (rendered as blockquote at the top of the release)")
	withSynthesis := fs.Bool("with-synthesis", false, "Join git-sourced entries with agent-written #changelog memories; merged prose renders as blockquote under each commit bullet, orphans surface in a dedicated section + terminal warning")

	fs.Usage = func() {
		fmt.Println("Usage: mpm ops changelog build [flags]")
		fmt.Println()
		fmt.Println("Generates CHANGELOG.md + changelog.json from the git log.")
		fmt.Println()
		fmt.Println("Flags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 1
	}

	// Resolve defaults.
	if *since == "" {
		tag, err := mpminternal.LatestTag(*repoDir)
		if err != nil {
			usererror.Warn("changelog: resolve latest tag: %v", err)
			return 1
		}
		*since = tag
	}
	if *version == "" {
		*version = bumpVersion(*since)
	}
	if *date == "" {
		*date = mpminternal.Today()
	}

	// Fetch the main release range.
	mainLog, err := mpminternal.FetchGitLog(mpminternal.GitLogOptions{
		RepoDir: *repoDir,
		Since:   *since,
		Until:   *until,
	})
	if err != nil {
		usererror.Warn("changelog: git log (%s..%s): %v", *since, *until, err)
		return 1
	}
	mainEntries := mpminternal.ParseCommitLog(mainLog)

	// If --release-notes was supplied, prepend a Highlight entry so the
	// renderer puts the prose at the top of the release section. The
	// file is read as-is; newlines and Markdown are preserved.
	if *notesFile != "" {
		body, readErr := os.ReadFile(*notesFile)
		if readErr != nil {
			usererror.Warn("changelog: read release-notes: %v", readErr)
			return 1
		}
		mainEntries = append([]mpminternal.ChangelogEntry{
			{
				Version:      *version,
				Date:         *date,
				Highlight:    true,
				Body:         strings.TrimSpace(string(body)),
				MPMMemoryIDs: []string{},
			},
		}, mainEntries...)
	}

	doc := &mpminternal.ChangelogDocument{
		Project: *projectName,
		Releases: []mpminternal.ChangelogRelease{
			{
				Version: *version,
				Date:    *date,
				Entries: mainEntries,
			},
		},
	}

	// Legacy backfill: walk from the start of the repo up to (but not
	// including) the main release range, dump as an unedited section.
	if *legacy {
		legacyLog, err := mpminternal.FetchGitLog(mpminternal.GitLogOptions{
			RepoDir: *repoDir,
			Until:   *since, // exclusive: walk up to the lower bound
			Reverse: true,   // oldest-first so the section reads chronologically
		})
		if err != nil {
			usererror.Warn("changelog: git log (legacy): %v", err)
			return 1
		}
		legacyEntries := mpminternal.ParseCommitLog(legacyLog)
		if len(legacyEntries) > 0 {
			doc.Legacy = &mpminternal.ChangelogRelease{
				Version: legacyVersion(*version),
				Date:    *date,
				Entries: legacyEntries,
			}
		}
	}

	markdown := doc.RenderMarkdown()

	// Optional synthesis pass: join agent-written #changelog
	// memories with the git-sourced entries. When the flag is set
	// we open the MPM database, run the synthesis, and re-render
	// the document. The output is the same shape as the un-
	// synthesized form, with Body fields populated from the
	// memories and orphan memories surfaced in a dedicated
	// section at the bottom of the file.
	if *withSynthesis {
		synResult, synErr := runSynthesis(doc)
		if synErr != nil {
			usererror.Warn("changelog: synthesis: %v", synErr)
			return 1
		}
		doc = synResult.Document
		markdown = doc.RenderMarkdown()
		// Terminal warning so the operator sees orphan count
		// immediately, even when the changelog is generated
		// non-interactively (e.g., in CI). The orphan section
		// inside the file is the persistent signal.
		if len(synResult.Orphans) > 0 {
			usererror.Warn("Found %d changelog memories with unmatched commit hashes.\n"+
				"      These are surfaced in the 'Orphan Changelog Memories' section at the end of %s.\n"+
				"      Common causes: agent hallucinated a hash, commit was squashed, or branch was abandoned.",
				len(synResult.Orphans), *output)
		}
		fmt.Printf("changelog: synthesis: %d matched, %d unmatched, %d orphans\n",
			synResult.Matched, synResult.Unmatched, len(synResult.Orphans))
	}

	if *dryRun {
		fmt.Print(markdown)
		return 0
	}

	// Write Markdown.
	if err := os.WriteFile(*output, []byte(markdown), 0o644); err != nil {
		usererror.Warn("changelog: write %s: %v", *output, err)
		return 1
	}
	absMd, _ := filepath.Abs(*output)
	fmt.Printf("changelog: wrote %s\n", absMd)

	// Write JSON sibling.
	jsonBytes, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		usererror.Warn("changelog: marshal json: %v", err)
		return 1
	}
	if err := os.WriteFile(*jsonOutput, jsonBytes, 0o644); err != nil {
		usererror.Warn("changelog: write %s: %v", *jsonOutput, err)
		return 1
	}
	absJSON, _ := filepath.Abs(*jsonOutput)
	fmt.Printf("changelog: wrote %s\n", absJSON)

	// Summary.
	release := doc.Releases[0]
	fmt.Printf("changelog: %d entries in %s (%s..%s)\n",
		len(release.Entries), release.Version, *since, headOrDefault(*until, "HEAD"))
	if doc.Legacy != nil {
		fmt.Printf("changelog: %d legacy entries in %s\n", len(doc.Legacy.Entries), doc.Legacy.Version)
	}
	return 0
}

// bumpVersion applies a simple semver patch bump when no --version
// is supplied. The result is informational — the operator is expected
// to confirm or override with --version on a real release. For our
// purposes (1.0.0 -> 1.1.0) we bump the minor component, since the
// reference arc is a feature-level change.
func bumpVersion(prev string) string {
	if prev == "" {
		return "0.1.0"
	}
	v := strings.TrimPrefix(prev, "v")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 3 {
		return v + ".0"
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return "0.1.0"
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return fmt.Sprintf("%d.1.0", major)
	}
	return fmt.Sprintf("%d.%d.0", major, minor+1)
}

// legacyVersion produces a section version for the legacy backfill
// block. We use "<main>-legacy" so the heading is distinct from any
// real version and operators can spot it at a glance. The renderer
// will format this as "## [1.1.0-legacy] - 2026-06-19" which is
// visibly a backfill marker.
func legacyVersion(mainVersion string) string {
	return mainVersion + "-legacy"
}

func headOrDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// runSynthesis opens the MPM database and joins the document with
// agent-written changelog memories. The database is opened with
// the same path resolution as other CLI commands (workspace DB
// from mpm_config.json, defaulting to ~/.mpm/).
//
// Orphan memories are attached to the SYNTHESIZED document (the
// working copy returned by SynthesizeChangelog, not the input)
// as a synthetic release so the renderer can emit them at the
// bottom of the file in their own ## [orphans] block. We use a
// separate release-like struct for the orphan section rather
// than reusing the Legacy field because the orphans are not
// 'legacy backfill' in any sense — they are present-day memories
// whose join failed.
func runSynthesis(doc *mpminternal.ChangelogDocument) (*mpminternal.ChangelogSynthesisResult, error) {
	dm := getDB()
	if dm == nil {
		return nil, fmt.Errorf("db unavailable: getDB returned nil (singleton init failed)")
	}
	res, err := mpminternal.SynthesizeChangelog(doc, dm.SQLDB())
	if err != nil {
		return nil, err
	}
	// Attach orphans as a synthetic release on the synthesized
	// document (not the input). This is the working copy that
	// the caller will use from here on.
	if len(res.Orphans) > 0 {
		orphanEntries := make([]mpminternal.ChangelogEntry, 0, len(res.Orphans))
		for _, m := range res.Orphans {
			e := mpminternal.ChangelogEntry{
				Version:      "orphans",
				Date:         mpminternal.Today(),
				Author:       "agent (unmatched commit)",
				Summary:      fmt.Sprintf("orphan memory id=%s", m.ID),
				Body:         fmt.Sprintf("commit hash claimed: %s\n\n%s", m.CommitHash, m.Content),
				MPMMemoryIDs: []string{m.ID},
			}
			orphanEntries = append(orphanEntries, e)
		}
		res.Document.Releases = append(res.Document.Releases, mpminternal.ChangelogRelease{
			Version: "orphans",
			Date:    mpminternal.Today(),
			Entries: orphanEntries,
		})
	}
	return res, nil
}
