package evidence

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

// The CHECK: read the published index, verify it against GitHub.
//
// # IT WORKS WITHOUT THE LOCAL DATABASE
//
// The check reads the PUBLISHED evidence and GitHub. It does not read
// the assignment store, and it does not need one: a fresh reviewer with
// a URL and a network can run it. That is the whole point of publishing
// the index at all.
//
// # IT IS STRICTLY READ-ONLY
//
// Nothing here writes: not to the substrate, not to GitHub, not to the
// filesystem. It reads an index, reads the records the index names, and
// reads GitHub.
//
// # IT DOES NOT REPORT STALE AS CURRENT
//
// When the index cannot be retrieved, the check says so and returns
// nothing. A reader that cannot reach the evidence must be told that it
// does not know — not shown the last thing it saw.

// Source describes where the checked evidence came from.
type Source struct {
	// Repository is where it lives.
	Repository string `json:"repository"`
	// Branch is the branch it lives on.
	Branch string `json:"branch"`
	// SnapshotID identifies the set of records.
	SnapshotID string `json:"snapshot_id"`
	// PublishedAt is when the publication happened.
	PublishedAt string `json:"published_at"`
	// Retrieved says the index was actually retrieved.
	//
	// It is false when the index could not be read, and the report says
	// so rather than showing the last state it saw.
	Retrieved bool `json:"retrieved"`
}

// Filters narrow what the check reports.
//
// They are the project's own dimensions — the repository, the project,
// the role, the disposition — and nothing else. There is no dashboard
// here and no new taxonomy.
type Filters struct {
	// Project keeps only the records of this project.
	Project string
	// Role keeps only the records with this role.
	Role string
	// Disposition keeps only the records with this disposition.
	Disposition string
}

// VerifiedRecord is one record and the result of verifying it.
type VerifiedRecord struct {
	*Record
	// Verification is what was checked.
	Verification *Verification `json:"verification"`
	// Verdict is the record's overall verification state.
	Verdict string `json:"verdict"`
	// DigestVerified says the record's bytes hashed to the identifier
	// the index gave them.
	//
	// It is a separate fact from the checks, because it is about the
	// RECORD and not about the world.
	DigestVerified bool `json:"digest_verified"`
}

// Section is one section of the queue.
type Section struct {
	// Disposition is the section's name.
	Disposition string `json:"disposition"`
	// Records are the records in it.
	Records []*VerifiedRecord `json:"records"`
}

// CheckReport is the whole result of a check.
type CheckReport struct {
	// Source is where the evidence came from.
	Source Source `json:"source"`
	// Verified says every record in the PUBLISHED CORPUS verified.
	//
	// It is computed over the WHOLE corpus and not over the filtered
	// view: a reader who filtered to one project must not be told
	// "verified" while another project's records are stale. It is
	// false when anything is stale, failed, missing or unverified, and
	// it is false for an empty corpus — an empty publication is not a
	// verified one.
	Verified bool `json:"verified"`
	// RecordCount is how many records the publication holds.
	//
	// It is the count of the CORPUS, so a reader can tell a filtered
	// view from the whole thing.
	RecordCount int `json:"record_count"`
	// ShownCount is how many records the filters left.
	ShownCount int `json:"shown_count"`
	// Sections are the queue's sections, in report order.
	Sections []Section `json:"sections"`
	// Records are every record, in report order.
	Records []*VerifiedRecord `json:"records"`
}

// RunCheck reads the published evidence and verifies it against GitHub.
//
// It returns an error when the index cannot be retrieved: a reader that
// cannot reach the evidence must be told that, and must not be handed
// the last state it saw as if it were current.
func RunCheck(ctx context.Context, gh GitHub, repository, branch string, filters Filters) (*CheckReport, error) {
	if gh == nil {
		return nil, fmt.Errorf("evidence: there is no GitHub reader to check against")
	}
	if repository == "" {
		return nil, fmt.Errorf("evidence: the check names no repository")
	}
	if branch == "" {
		branch = EvidenceBranch
	}
	report := &CheckReport{
		Source: Source{Repository: repository, Branch: branch},
	}
	raw, err := gh.Content(ctx, repository, branch, IndexPath)
	if err != nil {
		return nil, fmt.Errorf("evidence: the published index could not be retrieved from %s@%s: %w",
			repository, branch, err)
	}
	index, err := ParseIndex(raw)
	if err != nil {
		return nil, fmt.Errorf("evidence: the published index could not be read: %w", err)
	}
	report.Source.SnapshotID = index.SnapshotID
	report.Source.PublishedAt = index.PublishedAt
	report.Source.Retrieved = true

	verifier := &Verifier{GitHub: gh, Branch: branch}
	for _, entry := range index.Records {
		recordRaw, err := gh.Content(ctx, repository, branch, RecordsDirectory+"/"+entry.ID+".json")
		if err != nil {
			return nil, fmt.Errorf("evidence: the published record %s could not be retrieved: %w",
				entry.ID, err)
		}
		// THE DIGEST FIRST. A record whose bytes do not hash to the
		// identifier the index gave it is not the record the index
		// names, and everything said about it afterwards would be about
		// something else.
		digestOK := SameDigest(DigestBytes(recordRaw), entry.ID)
		var record Record
		if err := json.Unmarshal(recordRaw, &record); err != nil {
			return nil, fmt.Errorf("evidence: the published record %s could not be read: %w",
				entry.ID, err)
		}
		if !digestOK {
			verification := &Verification{}
			verification.add(CheckRepository, CheckFailed,
				"the record's bytes do not hash to the identifier the index gave them")
			report.Records = append(report.Records, &VerifiedRecord{
				Record:         &record,
				Verification:   verification,
				Verdict:        verification.Verdict(),
				DigestVerified: false,
			})
			continue
		}
		verification, err := verifier.Verify(ctx, &record)
		if err != nil {
			return nil, err
		}
		report.Records = append(report.Records, &VerifiedRecord{
			Record:         &record,
			Verification:   verification,
			Verdict:        verification.Verdict(),
			DigestVerified: true,
		})
	}

	// THE VERDICT IS OVER THE WHOLE CORPUS.
	//
	// It is computed BEFORE the filters, because a reader who filtered
	// to one project must not be told "verified" while another
	// project's records are stale.
	report.RecordCount = len(report.Records)
	report.Verified = report.RecordCount > 0
	for _, r := range report.Records {
		if r.Verdict != CheckVerified {
			report.Verified = false
			break
		}
	}
	report.Records = filterRecords(report.Records, filters)
	report.ShownCount = len(report.Records)
	report.Sections = sectionize(report.Records)
	return report, nil
}

// filterRecords applies the filters.
//
// The digest check is NOT a filter: a record whose bytes do not match
// their identifier is always reported, because a reader who filtered it
// away would never learn that the evidence was tampered with.
func filterRecords(records []*VerifiedRecord, filters Filters) []*VerifiedRecord {
	out := make([]*VerifiedRecord, 0, len(records))
	for _, r := range records {
		if filters.Project != "" && r.Project != filters.Project {
			continue
		}
		if filters.Role != "" && r.Role != filters.Role {
			continue
		}
		if filters.Disposition != "" && r.Disposition != filters.Disposition {
			continue
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].RootID != out[j].RootID {
			return out[i].RootID < out[j].RootID
		}
		return out[i].TaskID < out[j].TaskID
	})
	return out
}

// sectionize groups the records into the queue's sections.
//
// The STALE section is not a disposition: it is what the VERIFICATION
// found, and it is reported separately so that a reader sees both where
// a task sits and whether its evidence still holds.
//
// # WHY IT USES THE PER-CHECK PREDICATE
//
// The AGGREGATE verdict is the WORST check, and "unverified" outranks
// "stale". So a record whose review is stale and whose CI could not be
// read would aggregate to "unverified" — and a reader would never learn
// that the review is stale. The stale section therefore asks the
// CHECKS, not the aggregate: one stale check is enough.
func sectionize(records []*VerifiedRecord) []Section {
	sections := make([]Section, 0, len(Dispositions())+1)
	for _, disposition := range Dispositions() {
		group := make([]*VerifiedRecord, 0)
		for _, r := range records {
			if r.Disposition == disposition {
				group = append(group, r)
			}
		}
		if len(group) == 0 {
			continue
		}
		sections = append(sections, Section{Disposition: disposition, Records: group})
	}
	stale := StaleRecords(records)
	if len(stale) > 0 {
		sections = append(sections, Section{Disposition: StaleSection, Records: stale})
	}
	return sections
}

// StaleSection is the name of the section that holds the records whose
// evidence no longer holds.
//
// It is a CONSTANT and not a literal at the call sites, because the
// section is named in the report and a name that changed in one place
// and not another would make the report lie about itself.
const StaleSection = "stale_or_contradictory"

// StaleRecords returns the records whose evidence no longer holds.
//
// A record qualifies when ANY of its checks is stale or failed — not
// when its aggregate verdict is, because the aggregate lets an
// unverifiable check hide a stale one.
func StaleRecords(records []*VerifiedRecord) []*VerifiedRecord {
	var out []*VerifiedRecord
	for _, rec := range records {
		if rec.Verification != nil && rec.Verification.Stale() {
			out = append(out, rec)
			continue
		}
		if rec.Verdict == CheckStale || rec.Verdict == CheckFailed {
			out = append(out, rec)
		}
	}
	return out
}

// StaleRecords returns the records whose evidence no longer holds.
func (r *CheckReport) StaleRecords() []*VerifiedRecord {
	return StaleRecords(r.Records)
}
