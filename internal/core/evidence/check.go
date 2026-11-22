package evidence

import (
	"bytes"
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

// RenderingVerification is one human-readable artefact and whether its
// bytes are the ones the index named.
//
// The index names every artefact with a digest, so a reader can check
// the Markdown a person reads as exactly as they can check the record a
// machine reads. A digest nobody checks is a claim, so the CHECK
// checks them.
type RenderingVerification struct {
	// Path is the artefact's path on the evidence branch.
	Path string `json:"path"`
	// SHA256 is the digest the index gave it.
	SHA256 string `json:"sha256"`
	// OK says whether the artefact's bytes hash to that digest.
	OK bool `json:"ok"`
	// Detail says what could not be checked.
	Detail string `json:"detail,omitempty"`
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
	// SnapshotVerified says the index's snapshot id is the digest of
	// the records it lists.
	//
	// The report prints the id, so a reader may record it as the
	// corpus's identity; an index that renamed the snapshot would
	// otherwise be verified.
	SnapshotVerified bool `json:"snapshot_verified"`
	// SnapshotDetail explains a snapshot id that does not match.
	SnapshotDetail string `json:"snapshot_detail,omitempty"`
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
	// Renderings are the human-readable artefacts the index named, and
	// whether their bytes are the ones it named.
	//
	// They are checked because the index gives them digests: a digest
	// nobody checks is a claim, and a tampered queue summary would be
	// invisible to a reader who trusted it.
	Renderings []RenderingVerification `json:"renderings,omitempty"`
	// Truncated says the PUBLICATION holds only part of the work.
	//
	// It is the reader's answer to the question the index answered for
	// the publisher: a queue that was cut short is not the queue, and a
	// reader who cannot see that would take a partial publication for
	// the whole one.
	Truncated bool `json:"truncated,omitempty"`
	// Scanned is how many assignments the publication was read from.
	Scanned int `json:"scanned,omitempty"`
	// ScanLimit is the bound the read was under.
	ScanLimit int `json:"scan_limit,omitempty"`
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

	// THE READ IS CARRIED THROUGH TO THE READER.
	//
	// The index says whether the publication holds all of the work, and
	// the reader is the one who has to know: a queue that was cut
	// short is not the queue, and the flag in a machine file that the
	// reader has to go and fetch is not the same as the reader knowing.
	report.Truncated = index.Truncated
	report.Scanned = index.Scanned
	report.ScanLimit = index.ScanLimit

	verifier := &Verifier{GitHub: gh, Branch: branch, Repository: repository}
	// THE RECORDS, AND WHAT THE READER CAN CHECK ABOUT THEM.
	//
	// Every decoded record is kept, with its index identifier and
	// whether its bytes are the ones the index named. The artefacts are
	// re-rendered FROM the records that verified, so this list is what
	// the rendering checks stand on.
	loaded := make([]*Record, 0, len(index.Records))
	loadedIDs := make([]string, 0, len(index.Records))
	clean := true
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
			// REPORT IT, DO NOT ABORT.
			//
			// A record that cannot be read is a record the reader must
			// be told about. Aborting would hide the queue behind an
			// error, and the index entry still says WHICH record it is.
			verification := &Verification{}
			if !digestOK {
				verification.add(CheckRepository, CheckFailed,
					"the record's bytes do not hash to the identifier the index gave them")
			}
			verification.add(CheckRecord, CheckFailed,
				"the record's bytes are not a record: "+err.Error())
			report.Records = append(report.Records, &VerifiedRecord{
				Record: &Record{
					TaskID: entry.TaskID, RootID: entry.RootID,
					Title: entry.Title, PRRef: entry.PRRef,
					Disposition: entry.Disposition,
				},
				Verification:   verification,
				Verdict:        verification.Verdict(),
				DigestVerified: digestOK,
			})
			clean = false
			continue
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
			clean = false
			continue
		}
		// THE PACKAGE'S OWN GATES RUN HERE TOO.
		//
		// A record that is published but wrong is worse than a missing
		// one, because it is believed. The reader applies the same
		// validity and redaction gates the publisher applies before it
		// publishes — and the same derivation for the queue's filing,
		// so a record cannot file itself where its fields do not put
		// it.
		verification := &Verification{}
		if err := Validate(&record); err != nil {
			verification.add(CheckRecord, CheckFailed, err.Error())
		}
		if err := AssertPublishable(&record); err != nil {
			verification.add(CheckRecord, CheckFailed, err.Error())
		}
		if derived := DeriveDisposition(&record); derived != record.Disposition {
			verification.add(CheckDisposition, CheckFailed, fmt.Sprintf(
				"the record files itself as %q, and its own fields derive %q",
				record.Disposition, derived))
		}
		live, err := verifier.Verify(ctx, &record)
		if err != nil {
			return nil, err
		}
		verification.Checks = append(verification.Checks, live.Checks...)
		report.Records = append(report.Records, &VerifiedRecord{
			Record:         &record,
			Verification:   verification,
			Verdict:        verification.Verdict(),
			DigestVerified: true,
		})
		loaded = append(loaded, &record)
		loadedIDs = append(loadedIDs, entry.ID)
	}

	// THE INDEX'S SNAPSHOT ID IS A DIGEST OF THE RECORDS IT NAMES.
	//
	// It is printed at the top of the report, so a reader may record it
	// as the corpus's identity. The check has every record and can
	// recompute it; an index that renames the snapshot while the
	// records verify is an index that lies about what the reader is
	// looking at.
	if snapshot, err := SnapshotID(loaded); err != nil {
		report.SnapshotDetail = err.Error()
	} else if SameDigest(snapshot, index.SnapshotID) {
		report.SnapshotVerified = true
	} else {
		report.SnapshotDetail = fmt.Sprintf(
			"the index names the snapshot %s, and the records it lists hash to %s",
			index.SnapshotID, snapshot)
	}

	// THE VERDICT IS OVER THE WHOLE CORPUS.
	//
	// It is computed BEFORE the filters, because a reader who filtered
	// to one project must not be told "verified" while another
	// project's records are stale.
	report.RecordCount = len(report.Records)
	report.Verified = report.RecordCount > 0 && report.SnapshotVerified
	for _, r := range report.Records {
		if r.Verdict != CheckVerified {
			report.Verified = false
			break
		}
	}

	// THE HUMAN ARTEFACTS ARE CHECKED AGAINST THE RECORDS.
	//
	// The index names every Markdown artefact with a digest, and the
	// check re-renders each one FROM the record it belongs to: the
	// rendering is a pure function of the record, so comparing the
	// bytes binds the document a person reads to the record a machine
	// verified. Hashing it against the index alone would only bind it
	// to the mutable pointer.
	report.Renderings = verifyRenderings(ctx, gh, repository, branch, index,
		renderingExpectations(loaded, loadedIDs, index, clean))
	for _, r := range report.Renderings {
		if !r.OK {
			report.Verified = false
			break
		}
	}

	report.Records = filterRecords(report.Records, filters)
	report.ShownCount = len(report.Records)
	report.Sections = sectionize(report.Records)
	return report, nil
}

// renderingExpectation is one artefact the index must name, and the
// bytes it must be the rendering of.
//
// content is nil when the expectation cannot be computed — the record it
// would be rendered from is not the record the index names — and why
// says so. In that case only the index's digest can be checked.
type renderingExpectation struct {
	path    string
	content []byte
	why     string
}

// renderingExpectations is what the publication must hold, from the
// records the reader verified.
//
// The set is not the index's list: the index is data, so an index that
// names nothing would otherwise verify with nothing checked. It is the
// records themselves, plus the queue summary they render.
func renderingExpectations(records []*Record, ids []string, index *Index,
	clean bool) []renderingExpectation {
	out := make([]renderingExpectation, 0, len(records)+1)
	for i, r := range records {
		if i >= len(ids) {
			break
		}
		out = append(out, renderingExpectation{
			path:    RecordsDirectory + "/" + ids[i] + ".md",
			content: []byte(RenderRecordMarkdown(r)),
		})
	}
	corpus := &Corpus{
		Truncated: index.Truncated,
		Scanned:   index.Scanned,
		ScanLimit: index.ScanLimit,
	}
	summary := renderingExpectation{path: QueueSummaryPath}
	if clean {
		summary.content = []byte(RenderQueueMarkdown(records, index.SnapshotID, corpus))
	} else {
		summary.why = "the queue summary cannot be checked against the records while a record is not the one the index names"
	}
	out = append(out, summary)
	return out
}

// verifyRenderings checks the human artefacts the publication must hold.
//
// A rendering that cannot be read is NOT verified: "the artefact could
// not be found" and "the artefact is the one the index named" are
// different facts, and a reader who confuses them will believe a
// tampered summary.
func verifyRenderings(ctx context.Context, gh GitHub, repository, branch string,
	index *Index, expectations []renderingExpectation) []RenderingVerification {
	named := make(map[string]Rendering, len(index.Renderings))
	for _, r := range index.Renderings {
		named[r.Path] = r
	}
	out := make([]RenderingVerification, 0, len(expectations)+len(index.Renderings))
	seen := make(map[string]bool, len(expectations))
	for _, exp := range expectations {
		seen[exp.path] = true
		verification := RenderingVerification{Path: exp.path}
		entry, ok := named[exp.path]
		if !ok {
			verification.Detail = "the index does not name this artefact"
			out = append(out, verification)
			continue
		}
		verification.SHA256 = entry.SHA256
		if entry.SHA256 == "" {
			verification.Detail = "the index named this artefact with no digest"
			out = append(out, verification)
			continue
		}
		if !safeEvidencePath(exp.path) {
			verification.Detail = "the index names a path outside the evidence repository"
			out = append(out, verification)
			continue
		}
		raw, err := gh.Content(ctx, repository, branch, exp.path)
		if err != nil {
			verification.Detail = fmt.Sprintf("the artefact could not be read: %v", err)
			out = append(out, verification)
			continue
		}
		if exp.content != nil {
			if !bytes.Equal(raw, exp.content) {
				verification.Detail = "the artefact's bytes are not the rendering of the record it names"
				out = append(out, verification)
				continue
			}
			verification.OK = true
			out = append(out, verification)
			continue
		}
		// THE EXPECTATION COULD NOT BE COMPUTED. The index's digest is
		// all that can be checked, and the detail says so.
		if !SameDigest(DigestBytes(raw), entry.SHA256) {
			verification.Detail = fmt.Sprintf("the artefact hashes to %s and the index says %s",
				DigestBytes(raw), entry.SHA256)
			out = append(out, verification)
			continue
		}
		verification.OK = true
		verification.Detail = exp.why
		out = append(out, verification)
	}
	// AN EXTRA ARTEFACT the expected set does not know about. The index
	// still gives it a digest, so the digest is checked; there is no
	// record to re-render it from.
	for _, r := range index.Renderings {
		if seen[r.Path] {
			continue
		}
		verification := RenderingVerification{Path: r.Path, SHA256: r.SHA256}
		if r.Path == "" || r.SHA256 == "" {
			verification.Detail = "the index named an artefact with no path or no digest"
			out = append(out, verification)
			continue
		}
		if !safeEvidencePath(r.Path) {
			verification.Detail = "the index names a path outside the evidence repository"
			out = append(out, verification)
			continue
		}
		raw, err := gh.Content(ctx, repository, branch, r.Path)
		if err != nil {
			verification.Detail = fmt.Sprintf("the artefact could not be read: %v", err)
			out = append(out, verification)
			continue
		}
		got := DigestBytes(raw)
		if !SameDigest(got, r.SHA256) {
			verification.Detail = fmt.Sprintf("the artefact hashes to %s and the index says %s",
				got, r.SHA256)
			out = append(out, verification)
			continue
		}
		verification.OK = true
		out = append(out, verification)
	}
	return out
}

// filterRecords applies the filters.
//
// The digest check is NOT a filter: a record whose bytes do not match
// their identifier is always reported, because a reader who filtered it
// away would never learn that the evidence was tampered with.
//
// And neither is the VERIFICATION: a record whose evidence is stale or
// contradicted is always reported, because it is the cause of the
// report's own ⚠ — a filter that removed it would leave the reader with
// a warning and no reason for it.
//
// A filter narrows what a reader LOOKS at; it may not hide what the
// reader needs to KNOW.
func filterRecords(records []*VerifiedRecord, filters Filters) []*VerifiedRecord {
	out := make([]*VerifiedRecord, 0, len(records))
	for _, r := range records {
		// A record the reader NEEDS TO KNOW ABOUT is never filtered
		// away: one whose bytes are not the ones the index names, and
		// one whose evidence is stale or contradicted. A reader who
		// filtered those away would see a ⚠ with no visible cause.
		if !r.DigestVerified || (r.Verification != nil && r.Verification.Stale()) ||
			r.Verdict == CheckStale || r.Verdict == CheckFailed {
			out = append(out, r)
			continue
		}
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
