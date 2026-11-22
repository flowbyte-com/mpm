package evidence

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// The FIXTURE: a named corpus of engineering work.
//
// # WHY A FIXTURE EXISTS
//
// The assignment store is the natural source of a publication, and it is
// the right one. But a corpus that only ever comes from the store cannot
// be published until the store has been populated, and the Continuity 001
// work itself PREDATES the store being used for it — the four pull
// requests were reviewed and merged before the substrate was running the
// work.
//
// So a fixture is a corpus of claims, written down, that the publisher
// publishes exactly the way it publishes the store's claims: the claims
// come from the fixture, and the OBSERVED facts are read from GitHub at
// publication time.
//
// # WHAT A FIXTURE MAY NOT DO
//
// It may not carry the observed facts. A fixture that recorded "the pull
// request is merged" would be a claim about GitHub written by hand, and
// the publisher would publish it as a fact. The fixture names the pull
// request; the publisher asks GitHub what became of it.
//
// It may not carry a review verdict that the service recorded. A verdict
// in a fixture is a REPORTED verdict, and it is published as one.

// Fixture is a named corpus of claims.
type Fixture struct {
	// Name is the fixture's name.
	Name string `json:"name"`
	// Description says what the corpus is, in one bounded line.
	Description string `json:"description"`
	// Repository is "owner/name" — where the work is visible.
	Repository string `json:"repository"`
	// Project is the project the work belongs to.
	Project string `json:"project"`
	// RootID is the root assignment the corpus hangs from.
	RootID string `json:"root_id"`
	// Tasks are the corpus's tasks.
	Tasks []FixtureTask `json:"tasks"`
}

// FixtureTask is one task of a fixture.
type FixtureTask struct {
	// TaskID identifies the task within the corpus.
	TaskID string `json:"task_id"`
	// Title is the task's title.
	Title string `json:"title"`
	// Objective is the task's one-line objective.
	Objective string `json:"objective"`
	// Recipient is who the work was addressed to.
	//
	// It is Unknown when the corpus was reconstructed from a record that
	// does not name one. Guessing would be worse than saying so.
	Recipient string `json:"recipient"`
	// Role is the task's role.
	Role string `json:"role"`
	// State is the task's state.
	State string `json:"state"`
	// IssueRef is the issue the work answers.
	IssueRef string `json:"issue_ref,omitempty"`
	// PRRef is the pull request the work produced.
	PRRef string `json:"pr_ref,omitempty"`
	// BaseSHA is the implementation's base.
	BaseSHA string `json:"base_sha"`
	// HeadSHA is the implementation's head.
	HeadSHA string `json:"head_sha"`
	// ReviewedSHA is the revision that was reviewed.
	ReviewedSHA string `json:"reviewed_sha"`
	// ReviewDisposition is what is known about the review.
	ReviewDisposition string `json:"review_disposition"`
	// ReviewVerdict is the REPORTED verdict, when the review happened
	// outside the substrate.
	ReviewVerdict string `json:"review_verdict,omitempty"`
	// ReviewEvidenceRef points at the review's bytes.
	ReviewEvidenceRef string `json:"review_evidence_ref,omitempty"`
	// ReviewEvidenceSHA256 is the digest of those bytes.
	//
	// It is Unknown when the bytes were never captured durably. It is
	// NEVER the digest of something else.
	ReviewEvidenceSHA256 string `json:"review_evidence_sha256,omitempty"`
	// PendingOperatorAction is what the operator is being asked for.
	PendingOperatorAction string `json:"pending_operator_action,omitempty"`
}

// The fixtures are EMBEDDED.
//
// A fixture is a corpus that ships with the binary, so the corpus an
// installed MPM publishes is the corpus its source says it is — and a
// path that could point somewhere else is a path that eventually will.
//
//go:embed fixtures/*.json
var fixtureFS embed.FS

// LoadFixture reads a named fixture from the embedded corpus.
//
// It refuses an unknown field and a trailing document: a fixture that
// carries something the loader does not understand is a fixture whose
// meaning nobody can vouch for.
func LoadFixture(name string) (*Fixture, error) {
	path, err := FixturePath(name)
	if err != nil {
		return nil, err
	}
	raw, err := fixtureFS.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("evidence: read the fixture %q: %w", name, err)
	}
	var f Fixture
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("evidence: the fixture is not valid: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("evidence: the fixture carries content after the object")
	}
	if f.Name == "" {
		return nil, fmt.Errorf("evidence: the fixture names itself nothing")
	}
	if _, _, err := ParseRepository(f.Repository); err != nil {
		return nil, fmt.Errorf("evidence: the fixture: %w", err)
	}
	if f.RootID == "" {
		return nil, fmt.Errorf("evidence: the fixture names no root assignment")
	}
	if len(f.Tasks) == 0 {
		return nil, fmt.Errorf("evidence: the fixture holds no tasks")
	}
	return &f, nil
}

// Tree turns the fixture into a tree of claims.
//
// A fixture task becomes an assignment in one tree, and the review of a
// task is the review of that task — a fixture holds the review's facts,
// not a separate reviewer assignment.
func (f *Fixture) Tree() Tree {
	tree := Tree{RootID: f.RootID}
	for _, t := range f.Tasks {
		tree.Tasks = append(tree.Tasks, Claims{
			ID:             t.TaskID,
			RootID:         f.RootID,
			ProjectID:      f.Project,
			Title:          t.Title,
			Objective:      t.Objective,
			Recipient:      t.Recipient,
			Role:           t.Role,
			State:          t.State,
			IssueRef:       t.IssueRef,
			PRRef:          t.PRRef,
			BaseSHA:        t.BaseSHA,
			HeadSHA:        t.HeadSHA,
			ReviewedSHA:    t.ReviewedSHA,
			FindingsSHA256: t.ReviewEvidenceSHA256,
			EvidenceRef:    t.ReviewEvidenceRef,
			AwaitingAction: t.PendingOperatorAction,
		})
	}
	return tree
}

// Reviews returns the fixture's review facts, keyed by task id.
//
// A fixture's review is a REPORTED review: it happened in a session, not
// in the substrate's ledger. It is published as a claim, and the record
// says so.
//
// The map holds CANDIDATES, and the package picks the newest of them —
// so the rule that decides which review is current is the same rule for
// the fixture and for the store.
func (f *Fixture) Reviews() map[string][]ReviewInfo {
	out := map[string][]ReviewInfo{}
	for _, t := range f.Tasks {
		if t.ReviewDisposition == "" {
			continue
		}
		out[t.TaskID] = append(out[t.TaskID], ReviewInfo{
			Exists:          true,
			ReviewedSHA:     t.ReviewedSHA,
			FindingsSHA256:  t.ReviewEvidenceSHA256,
			EvidenceRef:     t.ReviewEvidenceRef,
			Verdict:         t.ReviewVerdict,
			LocallyReported: t.ReviewDisposition == ReviewDispositionLocallyReported,
		})
	}
	return out
}

// Corpus is a publication's input: the claims and the observed facts.
type Corpus struct {
	// Trees are the trees being published.
	Trees []Tree
	// Reviews are the review facts, keyed by task id.
	//
	// Each task's entry holds the CANDIDATES — every review anybody
	// recorded of it — and the newest one is the one that is published.
	Reviews map[string][]ReviewInfo
	// Facts are the observed facts, keyed by task id.
	Facts map[string]Facts
	// Truncated says the corpus holds only PART of the work.
	//
	// It is published, because a reader that took a partial queue for
	// the whole one would be misled — and a silent truncation is worse
	// than a visible one.
	Truncated bool
	// Scanned is how many assignments the corpus was read from.
	Scanned int
	// ScanLimit is the bound the corpus was read under.
	ScanLimit int
}

// BuildCorpusFromFixture reads a fixture and asks GitHub what became of
// it.
//
// The FIXTURE supplies the claims. GITHUB supplies the facts: the pull
// request's state, its head, and the workflow runs for that head. A
// fixture cannot state a fact about GitHub, and this function never
// believes one if it tries.
func BuildCorpusFromFixture(ctx context.Context, gh GitHub, f *Fixture) (*Corpus, error) {
	if gh == nil {
		return nil, fmt.Errorf("evidence: there is no GitHub reader to read the fixture's facts")
	}
	corpus := &Corpus{
		Trees:   []Tree{f.Tree()},
		Reviews: f.Reviews(),
		Facts:   map[string]Facts{},
	}
	for _, t := range f.Tasks {
		if t.PRRef == "" || t.PRRef == Unknown {
			continue
		}
		number, err := PRNumber(t.PRRef)
		if err != nil {
			return nil, fmt.Errorf("evidence: the fixture's task %s: %w", t.TaskID, err)
		}
		pr, err := gh.PullRequest(ctx, f.Repository, number)
		if err != nil {
			return nil, fmt.Errorf("evidence: read the pull request for %s: %w", t.TaskID, err)
		}
		facts := Facts{GitHub: pr}
		if t.HeadSHA != "" && t.HeadSHA != Unknown {
			runs, err := gh.WorkflowRuns(ctx, f.Repository, t.HeadSHA)
			if err != nil {
				// A missing CI is not a fatal problem: the record
				// publishes what IS known, and the verification says
				// what could not be read.
				runs = nil
			}
			facts.CI = runs
		}
		corpus.Facts[t.TaskID] = facts
	}
	return corpus, nil
}

// BuildCorpusFromTrees builds a corpus from the assignment store's trees
// and the facts read from GitHub.
func BuildCorpusFromTrees(trees []Tree, reviews map[string][]ReviewInfo,
	facts map[string]Facts) *Corpus {
	return &Corpus{Trees: trees, Reviews: reviews, Facts: facts}
}

// Records builds the corpus's records.
//
// The corpus's review facts are merged with the tree's own correlation,
// and the NEWEST review of a task wins — the rule lives in
// BuildWithReviews, so it is the same rule for every publisher.
func (c *Corpus) Records() ([]*Record, error) {
	var out []*Record
	for _, tree := range c.Trees {
		records, err := BuildWithReviews(tree, c.Facts, c.Reviews)
		if err != nil {
			return nil, err
		}
		out = append(out, records...)
	}
	return out, nil
}

// FixturePath returns the path of a named fixture inside the embedded
// corpus.
//
// The name is checked: a fixture name that names a PATH would let a
// caller reach a file outside the corpus, and the corpus is the only
// thing the publisher may publish from.
func FixturePath(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("evidence: the fixture names itself nothing")
	}
	if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return "", fmt.Errorf("evidence: the fixture name %q names a path", name)
	}
	return filepath.Join("fixtures", name+".json"), nil
}

// FixtureNames lists the embedded fixtures.
//
// It exists so that a reader can see what the corpus holds without
// reading the source.
func FixtureNames() ([]string, error) {
	entries, err := fixtureFS.ReadDir("fixtures")
	if err != nil {
		return nil, fmt.Errorf("evidence: read the fixture corpus: %w", err)
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, strings.TrimSuffix(entry.Name(), ".json"))
	}
	return out, nil
}
