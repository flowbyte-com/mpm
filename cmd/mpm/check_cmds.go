// check_cmds.go — `mpm check`: the read-only engineering check.
//
// ONE COMMAND, ONE JOB: tell an operator (or a reviewer, or another
// session) what the engineering queue is, and whether the evidence behind
// it still holds.
//
// # IT IS THE READ-ONLY PATH
//
// `check` reads the PUBLISHED evidence and reads GitHub. It does not read
// the assignment store, it does not write to anything, and it does not
// start an agent. A fresh reviewer with a URL and a network can run it on
// a machine that has never had MPM installed.
//
// # IT DOES NOT REPORT STALE AS CURRENT
//
// When the published index cannot be retrieved, the command says so and
// exits non-zero. It never falls back to a local copy and calls it
// current: a reader who cannot reach the evidence must be told that it
// does not know.
//
// # IT NEEDS NO PUBLICATION CREDENTIALS
//
// The only credential it can use is an OPTIONAL read token, and a public
// repository needs none at all. The publisher's write credential is not
// reachable from here.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/flowbyte-com/mpm-core/evidence"
	"github.com/flowbyte-com/mpm/cmd/mpm/render"
)

// checkRepository is the repository the check reads by default.
//
// It is this project's own repository, and it is a DEFAULT and not a
// constant of the evidence format: --repo points the check elsewhere, and
// the published format says nothing about which repository it describes.
const checkRepository = "flowbyte-com/mpm"

// checkTokenEnv is the OPTIONAL read credential.
//
// A public repository needs no credential at all, and this variable is
// never required. It is a READ token: the check has no write path at
// all, so a token that could write is not needed and is not asked for.
const checkTokenEnv = "MPM_GITHUB_TOKEN"

// checkAPIEnv overrides the API's root.
//
// It is an ENTERPRISE and TEST seam: a GitHub Enterprise instance has its
// own API root, and a test needs a local one. It changes WHERE the
// reader asks, never WHAT it accepts.
const checkAPIEnv = "MPM_GITHUB_API"

// printCheckHelp prints the check's help page.
//
// It is INERT: it reads nothing, writes nothing, and needs no database.
func printCheckHelp() int {
	var b strings.Builder
	render.Heading(&b, "Check")
	render.BlankLine(&b)
	b.WriteString("Show the engineering queue and verify its evidence against GitHub.\n\n")
	b.WriteString("The check reads the published evidence and GitHub. It writes nothing, starts\n")
	b.WriteString("nothing, and needs no local database and no publication credential.\n\n")
	b.WriteString("  --json          machine-readable output\n")
	b.WriteString("  --repo          the repository to read (default " + checkRepository + ")\n")
	b.WriteString("  --branch        the evidence branch (default " + evidence.EvidenceBranch + ")\n")
	b.WriteString("  --project       only this project's records\n")
	b.WriteString("  --role          only this role's records\n")
	b.WriteString("  --disposition   only this disposition's records\n")
	return respond(b.String(), "", 0)
}

// handleCheck runs the read-only engineering check.
func handleCheck(args []string) int {
	jsonOutput, rest := ExtractJSONFlag(args)
	repo := checkRepository
	branch := evidence.EvidenceBranch
	var filters evidence.Filters

	cleaned := make([]string, 0, len(rest))
	for i := 0; i < len(rest); i++ {
		arg := rest[i]
		switch {
		case arg == "--repo" || arg == "--branch" || arg == "--project" ||
			arg == "--role" || arg == "--disposition":
			if i+1 >= len(rest) {
				return respond("", fmt.Sprintf("Error: %s needs a value\n", arg), 1)
			}
			value := rest[i+1]
			i++
			switch arg {
			case "--repo":
				repo = value
			case "--branch":
				branch = value
			case "--project":
				filters.Project = value
			case "--role":
				filters.Role = value
			case "--disposition":
				filters.Disposition = value
			}
		default:
			cleaned = append(cleaned, arg)
		}
	}
	if len(cleaned) > 0 {
		return respond("", fmt.Sprintf(
			"Error: check takes no positional arguments (%s)\n", strings.Join(cleaned, " ")), 1)
	}

	client := evidence.NewClient(os.Getenv(checkTokenEnv))
	if base := strings.TrimSpace(os.Getenv(checkAPIEnv)); base != "" {
		client.BaseURL = base
	}
	report, err := evidence.RunCheck(context.Background(), client, repo, branch, filters)
	if err != nil {
		// The index could not be retrieved or read. That is an ERROR,
		// not an empty queue: a reader who cannot reach the evidence
		// must be told that, and must not be shown the last thing the
		// tool saw as if it were current.
		return respond("", fmt.Sprintf("Error: %v\n", err), 1)
	}

	if jsonOutput {
		raw, merr := json.MarshalIndent(report, "", "  ")
		if merr != nil {
			return respond("", fmt.Sprintf("Error: encode the report: %v\n", merr), 1)
		}
		return respond(string(raw)+"\n", "", 0)
	}
	return respond(renderCheckReport(report), "", 0)
}

// renderCheckReport renders the check for a human reader.
//
// It renders the SAME report the machine mode emits — one set of
// records, two renderings — and it never prints raw JSON.
func renderCheckReport(report *evidence.CheckReport) string {
	var b strings.Builder
	render.Heading(&b, "Check")
	render.BlankLine(&b)

	status := "✓"
	if !report.Verified {
		status = "⚠"
	}
	fmt.Fprintf(&b, "%s  evidence   %s @ %s\n", status, report.Source.Repository, report.Source.Branch)
	fmt.Fprintf(&b, "   snapshot   %s\n", report.Source.SnapshotID)
	fmt.Fprintf(&b, "   published  %s\n", report.Source.PublishedAt)
	fmt.Fprintf(&b, "   records    %d · %s\n",
		len(report.Records), verifiedSummary(report.Records))
	render.BlankLine(&b)

	if len(report.Sections) == 0 {
		b.WriteString("The queue is empty.\n")
		return b.String()
	}
	for _, section := range report.Sections {
		fmt.Fprintf(&b, "%s · %d\n", section.Disposition, len(section.Records))
		for _, rec := range section.Records {
			fmt.Fprintf(&b, "   %s  %s  %s  %s\n",
				recordSymbol(rec), rec.TaskID, rec.Role, rec.Disposition)
			fmt.Fprintf(&b, "      %s · head %s\n", rec.PRRef, rec.HeadSHA)
			if detail := firstProblem(rec); detail != "" {
				fmt.Fprintf(&b, "      %s %s\n", recordSymbol(rec), detail)
			}
		}
		render.BlankLine(&b)
	}
	if len(report.StaleRecords()) > 0 {
		// The stale section repeats the records, so it carries ONLY
		// the problem. A reader who has just read the queue does not
		// need the pointers again.
		fmt.Fprintf(&b, "stale or contradictory · %d\n", len(report.StaleRecords()))
		for _, rec := range report.StaleRecords() {
			fmt.Fprintf(&b, "   %s  %s  %s\n", recordSymbol(rec), rec.TaskID,
				firstProblem(rec))
		}
		render.BlankLine(&b)
	}
	return b.String()
}

// recordSymbol is the status symbol for one verified record.
func recordSymbol(rec *evidence.VerifiedRecord) string {
	switch rec.Verdict {
	case evidence.CheckVerified:
		return "✓"
	case evidence.CheckStale, evidence.CheckFailed:
		return "✗"
	case evidence.CheckUnverified:
		return "⚠"
	default:
		return "○"
	}
}

// firstProblem returns the first check that is not a verification.
//
// It is the ONE thing a reader needs first: what is wrong, in one line.
func firstProblem(rec *evidence.VerifiedRecord) string {
	if rec.Verification == nil {
		return ""
	}
	for _, check := range rec.Verification.Checks {
		if check.Status != evidence.CheckVerified {
			return fmt.Sprintf("%s: %s", check.Name, check.Detail)
		}
	}
	return ""
}

// verifiedSummary counts the records by their verification state.
func verifiedSummary(records []*evidence.VerifiedRecord) string {
	counts := map[string]int{}
	for _, rec := range records {
		counts[rec.Verdict]++
	}
	parts := make([]string, 0, len(counts))
	for _, status := range []string{
		evidence.CheckVerified, evidence.CheckStale, evidence.CheckFailed,
		evidence.CheckMissing, evidence.CheckUnverified,
	} {
		if counts[status] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[status], status))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, " · ")
}
