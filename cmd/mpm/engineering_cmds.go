// engineering_cmds.go — `mpm engineering publish`: the publication path.
//
// ONE VERB, and it is the operator's:
//
//	mpm engineering publish [--dry-run] [--fixture NAME]
//
// # WHY IT IS A SEPARATE FAMILY
//
// The substrate already has an `mpm evidence` family, and it is about
// something else: the confidence foundation's own evidence types. The
// engineering evidence is a different thing with a different reader, so
// it gets its own door rather than a leaf under a name that means
// something else.
//
// # WHY IT IS CLI-ONLY
//
// Publication WRITES to a repository. An agent-facing action that writes
// to a repository is a way for an agent to gain a privilege, and the
// publication's whole authority argument rests on the operator being the
// one who holds it. So there is no MCP action and no registry entry: the
// operator's shell is the door, exactly as PR 3's `execute` and PR 4's
// `dispatch` are.
//
// # WHAT IT MAY BE POINTED AT
//
// The repository and the branch come from the OPERATOR'S CONFIGURATION
// file. A payload cannot select them: a model that could name the
// target could publish the evidence anywhere.
//
// # THE DRY RUN IS THE SAME PLAN
//
// A dry run builds the publication and stops. It is not a second
// rendering that might differ from the one that gets published: it is the
// same plan, with the writing left out.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/flowbyte-com/mpm-core/assignment"
	"github.com/flowbyte-com/mpm-core/config"
	"github.com/flowbyte-com/mpm-core/evidence"
)

// evidenceConfigFileName is the publication configuration's name inside
// the MPM directory.
//
// It is a FILE, because the publication target is the OPERATOR'S choice
// and an installed binary must never publish on its own.
const evidenceConfigFileName = "evidence.json"

// handleEngineering dispatches the engineering family's leaves.
func handleEngineering(args []string) int {
	if len(args) == 0 {
		return printEngineeringHelp()
	}
	switch args[0] {
	case "publish":
		return handleEngineeringPublish(args[1:])
	case "help", "--help", "-h":
		return printEngineeringHelp()
	default:
		return respond("", fmt.Sprintf(
			"Error: unknown engineering command %q — try `mpm engineering help`\n", args[0]), 1)
	}
}

// printEngineeringHelp prints the family's help page.
func printEngineeringHelp() int {
	fmt.Println("Usage: mpm engineering publish [--dry-run] [--fixture NAME] [--json]")
	fmt.Println()
	fmt.Println("Publish the engineering evidence to the repository the operator configured.")
	fmt.Println()
	fmt.Println("  --dry-run     build the publication and print it; write nothing")
	fmt.Println("  --fixture     publish a named fixture corpus instead of the assignment store")
	fmt.Println("  --json        machine-readable output")
	fmt.Println()
	fmt.Println("The repository and the branch come from " + filepath.Join("~", ".mpm", evidenceConfigFileName) + ".")
	return 0
}

// handleEngineeringPublish publishes the engineering evidence.
func handleEngineeringPublish(args []string) int {
	jsonOutput, rest := ExtractJSONFlag(args)
	dryRun := false
	fixtureName := ""
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--dry-run":
			dryRun = true
		case "--fixture":
			if i+1 >= len(rest) {
				return respond("", "Error: --fixture needs a name\n", 1)
			}
			i++
			fixtureName = rest[i]
		default:
			return respond("", fmt.Sprintf(
				"Error: unknown option %q for `mpm engineering publish`\n", rest[i]), 1)
		}
	}

	target, err := evidence.LoadTarget(filepath.Join(config.GetMPMDir(), evidenceConfigFileName))
	if err != nil {
		// A MISSING CONFIGURATION IS NOT A DEFAULT. The publication
		// target is the operator's choice, and a publisher that invented
		// one would publish the evidence somewhere nobody chose.
		return respond("", fmt.Sprintf("Error: %v\n", err), 1)
	}

	ctx := context.Background()
	client := evidence.NewClient(os.Getenv(checkTokenEnv))
	if base := strings.TrimSpace(os.Getenv(checkAPIEnv)); base != "" {
		client.BaseURL = base
	}

	corpus, err := buildPublicationCorpus(ctx, client, fixtureName)
	if err != nil {
		return respond("", fmt.Sprintf("Error: %v\n", err), 1)
	}
	records, err := corpus.Records()
	if err != nil {
		return respond("", fmt.Sprintf("Error: %v\n", err), 1)
	}
	plan, err := evidence.NewPlan(target.Repository, target.Branch, records, time.Now())
	if err != nil {
		return respond("", fmt.Sprintf("Error: %v\n", err), 1)
	}
	writer, err := evidence.NewGHWriter(target.Repository, target.Branch)
	if err != nil {
		return respond("", fmt.Sprintf("Error: %v\n", err), 1)
	}
	publisher := &evidence.Publisher{Writer: writer, DryRun: dryRun}
	pub, err := publisher.Publish(ctx, plan)
	if err != nil {
		return respond("", fmt.Sprintf("Error: %v\n", err), 1)
	}
	if jsonOutput {
		raw, merr := json.MarshalIndent(pub, "", "  ")
		if merr != nil {
			return respond("", fmt.Sprintf("Error: encode the publication: %v\n", merr), 1)
		}
		return respond(string(raw)+"\n", "", 0)
	}
	return respond(renderPublication(pub, plan), "", 0)
}

// buildPublicationCorpus builds the corpus to publish.
//
// It is either a NAMED FIXTURE or the assignment store. The fixture is
// the corpus for work that predates the store's use; the store is the
// corpus for everything after it.
//
// The OBSERVED facts come from GitHub either way: a fixture states which
// pull request a task produced, and the publisher asks GitHub what became
// of it.
func buildPublicationCorpus(ctx context.Context, client *evidence.Client,
	fixtureName string) (*evidence.Corpus, error) {
	if fixtureName != "" {
		path, err := evidence.FixturePath(fixtureName)
		if err != nil {
			return nil, err
		}
		fixture, err := evidence.LoadFixture(resolveFixturePath(path))
		if err != nil {
			return nil, err
		}
		return evidence.BuildCorpusFromFixture(ctx, client, fixture)
	}
	store, err := openAssignmentStore()
	if err != nil {
		return nil, err
	}
	return corpusFromStore(store)
}

// openAssignmentStore opens the assignment store.
//
// It is the READ side of the publication: the store says what it claims,
// and the publisher says what GitHub says.
func openAssignmentStore() (*assignment.Store, error) {
	dm := getDBConcrete()
	if dm == nil {
		return nil, fmt.Errorf("the database is unavailable")
	}
	return assignment.NewStore(dm), nil
}

// resolveFixturePath finds a fixture's file.
//
// The fixtures are versioned with the package that reads them, and the
// binary is built beside them.
func resolveFixturePath(relative string) string {
	if _, err := os.Stat(relative); err == nil {
		return relative
	}
	return filepath.Join("internal", "core", "evidence", relative)
}

// corpusFromStore builds a corpus from the assignment store.
//
// The trees come from the store's own rows, and the VERDICTS come from
// the ledger: a verdict is a recorded decision, and the ledger is where
// decisions are recorded.
func corpusFromStore(store *assignment.Store) (*evidence.Corpus, error) {
	rows, err := store.Inbox(assignment.InboxQuery{Limit: 500})
	if err != nil {
		return nil, fmt.Errorf("read the assignments: %w", err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("the assignment store holds no assignments")
	}
	trees := map[string]evidence.Tree{}
	order := make([]string, 0, len(rows))
	reviews := map[string]evidence.ReviewInfo{}
	for _, a := range rows {
		tree, ok := trees[a.RootID]
		if !ok {
			tree = evidence.Tree{RootID: a.RootID}
			trees[a.RootID] = tree
			order = append(order, a.RootID)
		}
		claim := assignmentClaims(a)
		tree.Tasks = append(tree.Tasks, claim)
		trees[a.RootID] = tree
		if a.Role == assignment.RoleReviewer {
			verdict, subject, verr := verdictFromLedger(store, a)
			if verr != nil {
				return nil, verr
			}
			if verdict != "" && subject != "" {
				reviews[subject] = evidence.ReviewInfo{
					Exists:         true,
					ReviewerID:     a.ID,
					ReviewedSHA:    a.ReviewedSHA,
					FindingsSHA256: a.FindingsSHA256,
					EvidenceRef:    a.EvidenceRef,
					Verdict:        verdict,
				}
			}
		}
	}
	ordered := make([]evidence.Tree, 0, len(order))
	for _, id := range order {
		ordered = append(ordered, trees[id])
	}
	return evidence.BuildCorpusFromTrees(ordered, reviews, map[string]evidence.Facts{}), nil
}

// assignmentClaims turns an assignment into the publisher's claims.
func assignmentClaims(a *assignment.Assignment) evidence.Claims {
	claim := evidence.Claims{
		ID:             a.ID,
		RootID:         a.RootID,
		ProjectID:      a.ProjectID,
		Title:          a.Title,
		Objective:      a.Objective,
		Recipient:      a.Recipient,
		Role:           a.Role,
		State:          a.State,
		IssueRef:       a.IssueRef,
		PRRef:          a.PRRef,
		BaseSHA:        a.BaseSHA,
		HeadSHA:        a.HeadSHA,
		ReviewedSHA:    a.ReviewedSHA,
		FindingsSHA256: a.FindingsSHA256,
		EvidenceRef:    a.EvidenceRef,
		AwaitingAction: a.AwaitingAction,
		UpdatedAt:      a.UpdatedAt,
	}
	if a.Payload != nil {
		if target, ok := a.Payload[assignment.PayloadKeyReviewTarget].(string); ok {
			claim.ReviewTarget = target
		}
	}
	if a.ApprovedAt != nil {
		claim.Approval = &evidence.Approval{
			Action:     a.ApprovalAction,
			Revision:   a.ApprovalRevision,
			Source:     a.ApprovalSource,
			Evidence:   a.ApprovalEvidence,
			ApprovedAt: *a.ApprovedAt,
		}
	}
	return claim
}

// verdictFromLedger reads a reviewer's recorded verdict.
//
// The verdict is the event that carries one, and it names the subject it
// was about. Reading it from the ledger is what keeps a STATE from being
// mistaken for a DECISION.
func verdictFromLedger(store *assignment.Store, reviewer *assignment.Assignment) (string, string, error) {
	history, err := store.History(reviewer.ID)
	if err != nil {
		return "", "", fmt.Errorf("read the reviewer's ledger: %w", err)
	}
	for _, event := range history {
		if event.Payload == nil {
			continue
		}
		verdict, ok := event.Payload["verdict"].(string)
		if !ok || verdict == "" {
			continue
		}
		subject, _ := event.Payload["subject_id"].(string)
		return verdict, subject, nil
	}
	return "", "", nil
}

// renderPublication renders a publication for a human reader.
func renderPublication(pub *evidence.Publication, plan *evidence.Plan) string {
	var b strings.Builder
	if pub.DryRun {
		b.WriteString("dry run — nothing was written\n\n")
	}
	fmt.Fprintf(&b, "snapshot   %s\n", pub.SnapshotID)
	fmt.Fprintf(&b, "records    %d\n", pub.Records)
	fmt.Fprintf(&b, "branch     %s\n", plan.Branch)
	if pub.CommitSHA != "" {
		fmt.Fprintf(&b, "commit     %s\n", pub.CommitSHA)
	}
	b.WriteString("\nfiles\n")
	for _, f := range plan.Files {
		fmt.Fprintf(&b, "  %s\n", f.Path)
	}
	return b.String()
}
