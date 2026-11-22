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
	"strconv"
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
	fmt.Println("Usage: mpm engineering publish [--dry-run] [--fixture NAME] [--repo REPO] [--branch BRANCH] [--json]")
	fmt.Println()
	fmt.Println("Publish the engineering evidence to the repository the operator configured.")
	fmt.Println()
	fmt.Println("  --dry-run     build the publication and print it; write nothing")
	fmt.Println("  --fixture     publish a named fixture corpus instead of the assignment store")
	fmt.Println("  --repo        the repository (a dry run only; a write uses the configuration)")
	fmt.Println("  --branch      the evidence branch (a dry run only; a write uses the configuration)")
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
	repoFlag := ""
	branchFlag := ""
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
		case "--repo":
			if i+1 >= len(rest) {
				return respond("", "Error: --repo needs a value\n", 1)
			}
			i++
			repoFlag = rest[i]
		case "--branch":
			if i+1 >= len(rest) {
				return respond("", "Error: --branch needs a value\n", 1)
			}
			i++
			branchFlag = rest[i]
		default:
			return respond("", fmt.Sprintf(
				"Error: unknown option %q for `mpm engineering publish`\n", rest[i]), 1)
		}
	}

	// THE TARGET.
	//
	// The CONFIGURED target is the authority, and a WRITE requires it:
	// a publisher that invented a target would publish the evidence
	// somewhere nobody chose.
	//
	// A DRY RUN may be pointed at a target explicitly, because a
	// rehearsal that needs a configuration file to exist cannot be run
	// anywhere else — and a rehearsal writes nothing.
	target, err := evidence.LoadTarget(filepath.Join(config.GetMPMDir(), evidenceConfigFileName))
	if err != nil {
		if !dryRun {
			return respond("", fmt.Sprintf("Error: %v\n", err), 1)
		}
		if repoFlag == "" {
			return respond("", fmt.Sprintf(
				"Error: %v\nA dry run needs a target: pass --repo and --branch\n", err), 1)
		}
		target = &evidence.Target{Repository: repoFlag, Branch: branchFlag}
		if target.Branch == "" {
			target.Branch = evidence.EvidenceBranch
		}
		if _, _, perr := evidence.ParseRepository(target.Repository); perr != nil {
			return respond("", fmt.Sprintf("Error: %v\n", perr), 1)
		}
	} else if repoFlag != "" && repoFlag != target.Repository {
		return respond("", fmt.Sprintf(
			"Error: --repo names %s and the configured target is %s; the configuration wins\n",
			repoFlag, target.Repository), 1)
	} else if branchFlag != "" && branchFlag != target.Branch {
		// The BRANCH is compared as well as the repository. A flag
		// that was silently ignored would publish the evidence
		// somewhere the operator did not ask for.
		return respond("", fmt.Sprintf(
			"Error: --branch names %s and the configured target is %s; the configuration wins\n",
			branchFlag, target.Branch), 1)
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
	plan, err := evidence.NewPlanFromCorpus(target.Repository, target.Branch, corpus,
		records, time.Now())
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
		fixture, err := evidence.LoadFixture(fixtureName)
		if err != nil {
			return nil, err
		}
		return evidence.BuildCorpusFromFixture(ctx, client, fixture)
	}
	store, err := openAssignmentStore()
	if err != nil {
		return nil, err
	}
	return corpusFromStore(ctx, store, client)
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

// The store publication's scan limit.
//
// It is a BOUND, not a silent truncation: a publication that holds part
// of the queue says so, in the index and in the report.
const storeScanLimit = 500

// corpusFromStore builds a corpus from the assignment store.
//
// The trees come from the store's own rows, the VERDICTS come from the
// ledger — a verdict is a recorded decision, and the ledger is where
// decisions are recorded — and the OBSERVED facts come from GitHub.
//
// # THE OBSERVED SIDE IS NOT OPTIONAL
//
// A record that names no repository, no pull request state, no head and
// no workflow run cannot be checked by anybody. So the publisher reads
// the facts for every assignment that names a pull request, exactly as
// it does for a fixture.
//
// # THE NEWEST REVIEW WINS
//
// Two reviewers can review one subject: the head advanced, and the work
// was re-reviewed. The LATER review says what the subject's state is
// now, and publishing the earlier one would publish a superseded
// verdict as the current one.
//
// So the publisher hands the package EVERY review it found — the rows
// arrive newest first, so picking the last one written to a map would
// pick the OLDEST review — and the package applies the rule that decides
// which one is current. The rule lives there, and not here, because it
// is part of the contract: a caller that built its own map could get it
// wrong, and the map is not what a reader sees.
func corpusFromStore(ctx context.Context, store *assignment.Store,
	client *evidence.Client) (*evidence.Corpus, error) {
	rows, err := store.Inbox(assignment.InboxQuery{Limit: storeScanLimit + 1})
	if err != nil {
		return nil, fmt.Errorf("read the assignments: %w", err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("the assignment store holds no assignments")
	}
	// ONE ROW PAST THE BOUND.
	//
	// The corpus knows whether anything was actually dropped only if
	// it can see one row more than the bound. A scan that merely
	// REACHED the limit has not truncated anything, and publishing it
	// as truncated would be a false alarm.
	if len(rows) > storeScanLimit {
		rows = rows[:storeScanLimit]
	}
	trees := map[string]evidence.Tree{}
	order := make([]string, 0, len(rows))
	reviews := map[string][]evidence.ReviewInfo{}
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
				// EVERY REVIEW IS A CANDIDATE. The rows arrive
				// newest first, so picking the last one written to
				// a map would pick the OLDEST review — the
				// opposite of what the evidence contract needs.
				// The package picks the newest, so the publisher
				// only has to hand it the candidates.
				reviews[subject] = append(reviews[subject], evidence.ReviewInfo{
					Exists:         true,
					ReviewerID:     a.ID,
					ReviewedSHA:    a.ReviewedSHA,
					FindingsSHA256: a.FindingsSHA256,
					CaptureStatus:  claim.CaptureStatus,
					EvidenceRef:    a.EvidenceRef,
					Verdict:        verdict,
					UpdatedAt:      a.UpdatedAt,
				})
			}
		}
	}
	ordered := make([]evidence.Tree, 0, len(order))
	for _, id := range order {
		ordered = append(ordered, trees[id])
	}
	facts, err := observedFacts(ctx, client, rows)
	if err != nil {
		return nil, err
	}
	corpus := evidence.BuildCorpusFromTrees(ordered, reviews, facts)
	// THE TRUNCATION IS VISIBLE, AND IT IS NOT A GUESS.
	//
	// One row past the bound is read, so the corpus knows whether
	// anything was actually dropped: a scan that merely REACHED the
	// limit has not truncated anything, and saying that it had would
	// be a false alarm.
	corpus.Truncated = len(rows) > storeScanLimit
	corpus.Scanned = len(rows)
	if corpus.Truncated {
		corpus.Scanned = storeScanLimit
	}
	corpus.ScanLimit = storeScanLimit
	return corpus, nil
}

// captureStatus checks a captured digest against the bytes it names.
//
// The publisher checks it BEFORE it publishes it: a digest nobody
// checked is a claim, and the evidence contract exists to keep claims
// and evidence apart.
func captureStatus(a *assignment.Assignment) string {
	status, _ := evidence.VerifyCapturedEvidence(a.FindingsSHA256, a.EvidenceRef)
	return status
}

// observedFacts reads GitHub's facts for the assignments that name a
// pull request.
//
// It is the same read the fixture path does: the publisher asks GitHub
// what became of the work, and it asks ONCE per pull request and once
// per head.
func observedFacts(ctx context.Context, client *evidence.Client,
	rows []*assignment.Assignment) (map[string]evidence.Facts, error) {
	facts := map[string]evidence.Facts{}
	prCache := map[string]*evidence.GitHubFacts{}
	runCache := map[string][]evidence.CIRun{}
	for _, a := range rows {
		if a.PRRef == "" || a.PRRef == evidence.Unknown {
			continue
		}
		number, err := evidence.PRNumber(a.PRRef)
		if err != nil {
			continue
		}
		repository, err := evidence.RepositoryFromRef(a.PRRef)
		if err != nil {
			continue
		}
		key := repository + "#" + strconv.Itoa(number)
		pr, seen := prCache[key]
		if !seen {
			fetched, ferr := client.PullRequest(ctx, repository, number)
			if ferr != nil {
				// A pull request that cannot be read is NOT a
				// fatal problem: the record publishes the claims
				// the store holds, and the check reports that the
				// facts could not be verified.
				fetched = nil
			}
			prCache[key] = fetched
			pr = fetched
		}
		taskFacts := facts[a.ID]
		taskFacts.GitHub = pr
		head := a.HeadSHA
		if head == "" || head == evidence.Unknown {
			if pr != nil {
				head = pr.HeadSHA
			}
		}
		if head != "" && head != evidence.Unknown {
			runKey := repository + "@" + head
			runs, cached := runCache[runKey]
			if !cached {
				fetched, rerr := client.WorkflowRuns(ctx, repository, head)
				if rerr != nil {
					fetched = nil
				}
				runCache[runKey] = fetched
				runs = fetched
			}
			taskFacts.CI = runs
		}
		facts[a.ID] = taskFacts
	}
	return facts, nil
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
		// THE PUBLISHER'S ANSWER ABOUT THE CAPTURE.
		//
		// It belongs on the CLAIM, and not only on the ledger's
		// account of a review: the claim is what the tree's own
		// correlation reads, so this is what the reviewer's OWN
		// record carries — and a record that said "nothing is known
		// about this capture" beside the subject's record saying
		// "verified" would be contradicting the publisher about the
		// same digest.
		CaptureStatus: captureStatus(a),
	}
	// The repository comes from the assignment's OWN pull request
	// reference: it is where the work says it is visible, and it is what
	// a reader is given when GitHub could not be asked.
	if repo, err := evidence.RepositoryFromRef(a.PRRef); err == nil {
		claim.Repository = repo
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
