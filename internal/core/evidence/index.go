package evidence

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// The PUBLISHED LAYOUT.
//
// The evidence lives on a branch of the repository it describes, so a
// reader needs nothing but a URL and a network:
//
//	index.json                 the MUTABLE pointer at the current snapshot
//	records/<id>.json          one IMMUTABLE evidence record
//	records/<id>.md            the same record, human-readable
//	summaries/queue.md         the engineering queue, human-readable
//
// # IMMUTABLE SNAPSHOTS, MUTABLE POINTER
//
// A record is named by the digest of its own bytes, so the same evidence
// always lands on the same file and a republication is invisible rather
// than noisy. The INDEX is the one mutable file: it names the current
// snapshot and says when it was published.
//
// # NO SELF-REFERENCE
//
// Nothing in the published content contains the SHA of the content that
// contains it. The index names the records by their digests, and the
// records do not name the index. A reader can therefore verify every
// digest it is given, and none of them is a claim it cannot check.

// The paths of the published layout.
const (
	IndexPath        = "index.json"
	RecordsDirectory = "records"
	SummariesDir     = "summaries"
	QueueSummaryPath = SummariesDir + "/queue.md"
)

// IndexEntry is one record's line in the index.
//
// It is a POINTER, not a copy: the record itself is the evidence, and the
// index says where to find it.
type IndexEntry struct {
	// ID is the record's digest.
	ID string `json:"id"`
	// RootID is the tree the record belongs to.
	RootID string `json:"root_id"`
	// TaskID is the assignment the record is about.
	TaskID string `json:"task_id"`
	// PRRef is the pull request the work produced.
	PRRef string `json:"pr_ref,omitempty"`
	// Disposition is where the work sits in the queue.
	Disposition string `json:"disposition"`
	// Title is the assignment's title.
	Title string `json:"title,omitempty"`
}

// Rendering is one human-readable artefact of a publication.
//
// It is a POINTER with a digest, like a record: the artefact is what a
// person reads, and the digest is how a reader checks that the bytes
// they are reading are the bytes the publication produced.
//
// Before this, the human artefacts were covered by nothing: the index
// named only the machine-readable records, so a reader who verified a
// record had verified nothing about the Markdown a person actually
// reads. Now every artefact is named and checkable.
type Rendering struct {
	// Path is the artefact's path on the evidence branch.
	Path string `json:"path"`
	// SHA256 is the digest of the artefact's bytes.
	SHA256 string `json:"sha256"`
}

// Index is the published pointer at the current snapshot.
//
// It carries the PUBLICATION METADATA — when it was published, and where
// — and the identity of the snapshot. The records themselves carry
// everything else.
type Index struct {
	// FormatVersion is the evidence format this index conforms to.
	FormatVersion string `json:"format_version"`
	// PublishedAt is when the publication happened.
	//
	// It is METADATA, deliberately kept out of the records: two
	// publications of the same evidence produce the same records, and
	// only this field differs.
	PublishedAt string `json:"published_at"`
	// Repository is where the evidence lives.
	Repository string `json:"repository"`
	// Branch is the branch it lives on.
	Branch string `json:"branch"`
	// SnapshotID identifies the SET of records.
	SnapshotID string `json:"snapshot_id"`
	// RecordCount is how many records the snapshot holds.
	RecordCount int `json:"record_count"`
	// Records are the snapshot's records.
	Records []IndexEntry `json:"records"`
	// Renderings are the human-readable artefacts, with their digests.
	Renderings []Rendering `json:"renderings,omitempty"`
	// Truncated says the publication holds only PART of the work.
	//
	// A reader that took a partial queue for the whole one would be
	// misled, so the truncation is published rather than assumed away.
	Truncated bool `json:"truncated,omitempty"`
	// Scanned is how many assignments the publication was read from.
	//
	// It is the number of rows the scan contributed: when the
	// publication was truncated, this is the bound, and the rows past
	// it were dropped.
	Scanned int `json:"scanned,omitempty"`
	// ScanLimit is the bound the read was under.
	ScanLimit int `json:"scan_limit,omitempty"`
}

// File is one file of a publication.
type File struct {
	// Path is the file's path on the evidence branch.
	Path string `json:"path"`
	// Content is the file's bytes.
	Content []byte `json:"content"`
}

// Plan is a publication, before it is written.
//
// It is a VALUE: the publisher builds it, shows it, and writes it. A
// dry run is the plan without the writing, and it is the SAME plan — not
// a second rendering that could differ from the one that gets published.
type Plan struct {
	// Repository is where the evidence goes.
	Repository string `json:"repository"`
	// Branch is the branch it goes on.
	Branch string `json:"branch"`
	// SnapshotID identifies the set of records.
	SnapshotID string `json:"snapshot_id"`
	// Records are the records, in publication order.
	Records []*Record `json:"-"`
	// Files are the files, in publication order.
	Files []File `json:"files"`
}

// NewPlan builds the publication for a set of records.
//
// The records are checked for DUPLICATE LOGICAL KEYS before anything is
// rendered: two records for one assignment are not two facts about it,
// and publishing both would let a reader believe there were two.
func NewPlan(repository, branch string, records []*Record, publishedAt time.Time) (*Plan, error) {
	return NewPlanFromCorpus(repository, branch, nil, records, publishedAt)
}

// NewPlanFromCorpus builds the publication for a corpus.
//
// The corpus carries what the publisher knows about the READ it made —
// whether it was truncated, and under what bound — and that is
// published, because a reader that took a partial queue for the whole
// one would be misled.
func NewPlanFromCorpus(repository, branch string, corpus *Corpus, records []*Record,
	publishedAt time.Time) (*Plan, error) {
	if repository == "" {
		return nil, fmt.Errorf("evidence: the publication names no repository")
	}
	if branch == "" {
		return nil, fmt.Errorf("evidence: the publication names no branch")
	}
	seen := map[string]string{}
	for _, r := range records {
		key := r.RootID + "/" + r.TaskID
		if prev, ok := seen[key]; ok {
			return nil, fmt.Errorf(
				"evidence: the tree produced two records for %s (%s and %s); "+
					"a task has one record", key, prev, r.TaskID)
		}
		seen[key] = r.TaskID
	}
	snapshot, err := SnapshotID(records)
	if err != nil {
		return nil, err
	}
	plan := &Plan{
		Repository: repository,
		Branch:     branch,
		SnapshotID: snapshot,
		Records:    records,
	}
	// THE RECORDS FIRST, THE POINTER LAST.
	//
	// The pointer is the only mutable file, and it is what a reader
	// follows. If the publication is interrupted, the pointer still
	// names the PREVIOUS snapshot — which is true — rather than a
	// half-written corpus.
	for _, r := range records {
		id, err := RecordID(r)
		if err != nil {
			return nil, err
		}
		raw, err := CanonicalJSON(r)
		if err != nil {
			return nil, err
		}
		plan.Files = append(plan.Files, File{
			Path:    RecordsDirectory + "/" + id + ".json",
			Content: raw,
		})
		plan.Files = append(plan.Files, File{
			Path:    RecordsDirectory + "/" + id + ".md",
			Content: []byte(RenderRecordMarkdown(r)),
		})
	}
	plan.Files = append(plan.Files, File{
		Path:    QueueSummaryPath,
		Content: []byte(RenderQueueMarkdown(records, snapshot)),
	})
	index := Index{
		FormatVersion: FormatVersion,
		PublishedAt:   publishedAt.UTC().Format(time.RFC3339),
		Repository:    repository,
		Branch:        branch,
		SnapshotID:    snapshot,
		RecordCount:   len(records),
	}
	if corpus != nil {
		index.Truncated = corpus.Truncated
		index.Scanned = corpus.Scanned
		index.ScanLimit = corpus.ScanLimit
	}
	// THE HUMAN ARTEFACTS ARE NAMED AND CHECKED.
	//
	// Every file the publication wrote other than the index is named
	// here with its digest, so a reader can check the Markdown a person
	// reads as exactly as they can check the record a machine reads.
	for _, f := range plan.Files {
		if f.Path == IndexPath {
			continue
		}
		if strings.HasSuffix(f.Path, ".json") {
			// The records are named by the index's own entries.
			continue
		}
		index.Renderings = append(index.Renderings, Rendering{
			Path:   f.Path,
			SHA256: DigestBytes(f.Content),
		})
	}
	for _, r := range records {
		id, err := RecordID(r)
		if err != nil {
			return nil, err
		}
		index.Records = append(index.Records, IndexEntry{
			ID:          id,
			RootID:      r.RootID,
			TaskID:      r.TaskID,
			PRRef:       r.PRRef,
			Disposition: r.Disposition,
			Title:       r.Title,
		})
	}
	sort.SliceStable(index.Records, func(i, j int) bool {
		if index.Records[i].RootID != index.Records[j].RootID {
			return index.Records[i].RootID < index.Records[j].RootID
		}
		return index.Records[i].TaskID < index.Records[j].TaskID
	})
	sort.SliceStable(index.Renderings, func(i, j int) bool {
		return index.Renderings[i].Path < index.Renderings[j].Path
	})
	raw, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("evidence: encode the index: %w", err)
	}
	plan.Files = append(plan.Files, File{Path: IndexPath, Content: raw})
	return plan, nil
}

// ParseIndex reads a published index.
//
// It refuses an index it does not recognise, rather than reading the
// fields it happens to know.
func ParseIndex(raw []byte) (*Index, error) {
	var idx Index
	if err := json.Unmarshal(raw, &idx); err != nil {
		return nil, fmt.Errorf("evidence: decode the index: %w", err)
	}
	if idx.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("evidence: the index's format version is %q, want %q",
			idx.FormatVersion, FormatVersion)
	}
	if idx.SnapshotID == "" {
		return nil, fmt.Errorf("evidence: the index names no snapshot")
	}
	if idx.RecordCount != len(idx.Records) {
		return nil, fmt.Errorf("evidence: the index claims %d records and lists %d",
			idx.RecordCount, len(idx.Records))
	}
	for _, r := range idx.Renderings {
		if r.Path == "" || r.SHA256 == "" {
			return nil, fmt.Errorf("evidence: the index names a rendering with no path or no digest")
		}
	}
	return &idx, nil
}

// RenderRecordMarkdown renders one record for a human reader.
//
// It says what is VERIFIED and what is merely CLAIMED, because a human
// reading a table has no way to tell those apart otherwise.
func RenderRecordMarkdown(r *Record) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", r.Title)
	fmt.Fprintf(&b, "- **task**: `%s` (root `%s`)\n", r.TaskID, r.RootID)
	if r.Objective != "" && r.Objective != Unknown {
		fmt.Fprintf(&b, "- **objective**: %s\n", r.Objective)
	}
	fmt.Fprintf(&b, "- **recipient**: %s · **role**: %s\n", r.Recipient, r.Role)
	fmt.Fprintf(&b, "- **state**: %s · **disposition**: %s\n", r.State, r.Disposition)
	if r.PRRef != "" && r.PRRef != Unknown {
		fmt.Fprintf(&b, "- **pull request**: %s\n", r.PRRef)
	}
	if r.IssueRef != "" && r.IssueRef != Unknown {
		fmt.Fprintf(&b, "- **issue**: %s\n", r.IssueRef)
	}
	fmt.Fprintf(&b, "- **base**: %s\n", r.BaseSHA)
	fmt.Fprintf(&b, "- **head**: %s\n", r.HeadSHA)
	fmt.Fprintf(&b, "- **reviewed**: %s\n", r.ReviewedSHA)
	fmt.Fprintf(&b, "- **review**: %s", r.ReviewDisposition)
	if r.ReviewVerdict != "" {
		fmt.Fprintf(&b, " (`%s`)", r.ReviewVerdict)
	}
	b.WriteString("\n")
	if r.ReviewEvidence != nil {
		fmt.Fprintf(&b, "- **review evidence**: %s · digest `%s`\n",
			r.ReviewEvidence.Ref, r.ReviewEvidence.SHA256)
	}
	if r.PendingOperatorAction != "" && r.PendingOperatorAction != Unknown {
		fmt.Fprintf(&b, "- **awaiting the operator**: %s\n", r.PendingOperatorAction)
	}
	if r.OperatorApproval != nil {
		fmt.Fprintf(&b, "- **approved**: %s (`%s`)\n",
			r.OperatorApproval.Action, r.OperatorApproval.Revision)
	}
	if r.GitHub != nil {
		fmt.Fprintf(&b, "- **github**: %s", prState(r.GitHub))
		if r.GitHub.HeadSHA != "" {
			fmt.Fprintf(&b, " at `%s`", r.GitHub.HeadSHA)
		}
		b.WriteString("\n")
	}
	if len(r.CI) > 0 {
		b.WriteString("- **ci**:\n")
		for _, run := range r.CI {
			conclusion := run.Conclusion
			if conclusion == "" {
				conclusion = run.Status
			}
			fmt.Fprintf(&b, "  - run %d · `%s` · %s · %s\n", run.ID, run.HeadSHA, conclusion, run.URL)
		}
	}
	b.WriteString("\n## Provenance\n\n")
	b.WriteString("| category | fields |\n|---|---|\n")
	fmt.Fprintf(&b, "| observed | %s |\n", strings.Join(r.Provenance.Observed, ", "))
	fmt.Fprintf(&b, "| recorded | %s |\n", strings.Join(r.Provenance.Recorded, ", "))
	fmt.Fprintf(&b, "| decisions | %s |\n", strings.Join(r.Provenance.Decisions, ", "))
	if len(r.Redactions) > 0 {
		fmt.Fprintf(&b, "\n**Withheld**: %s\n", strings.Join(r.Redactions, ", "))
	}
	return b.String()
}

// RenderQueueMarkdown renders the engineering queue for a human reader.
//
// It is the summary a person reads first, and it says the SAME things
// the machine report says — both render the same records.
func RenderQueueMarkdown(records []*Record, snapshot string) string {
	var b strings.Builder
	b.WriteString("# The engineering queue\n\n")
	fmt.Fprintf(&b, "Snapshot `%s` · format `%s`\n\n", snapshot, FormatVersion)
	if len(records) == 0 {
		b.WriteString("The queue is empty.\n")
		return b.String()
	}
	for _, disposition := range Dispositions() {
		group := make([]*Record, 0, len(records))
		for _, r := range records {
			if r.Disposition == disposition {
				group = append(group, r)
			}
		}
		if len(group) == 0 {
			continue
		}
		fmt.Fprintf(&b, "## %s (%d)\n\n", disposition, len(group))
		b.WriteString("| task | role | state | pull request | head | review |\n")
		b.WriteString("|---|---|---|---|---|---|\n")
		for _, r := range group {
			review := r.ReviewDisposition
			if r.ReviewVerdict != "" {
				review += " (" + r.ReviewVerdict + ")"
			}
			fmt.Fprintf(&b, "| `%s` | %s | %s | %s | `%s` | %s |\n",
				r.TaskID, r.Role, r.State, dash(r.PRRef), r.HeadSHA, review)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
