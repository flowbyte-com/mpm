// reference_batch.go — manifest-driven ingestion.
//
// A documentation corpus is not imported one page at a time. The
// bootstrap that motivated this work called `reference add` dozens of
// times and reconciled the results by hand, which is both slow and easy
// to get wrong: a failure in the middle of the list was indistinguishable
// from a page that had already been imported.
//
// A manifest makes the list itself the unit of work:
//
//	{
//	  "documents": [
//	    {"url": "https://example.org/a", "tags": ["docs"], "reason": "..."},
//	    {"path": "./local/spec.md", "tags": ["spec"]}
//	  ]
//	}
//
// Semantics that matter for retries:
//
//   - each document is ingested through the same IngestReference pipeline
//     as a single add, so identity, diffing and embedding behave
//     identically;
//   - processing is sequential and bounded — no worker pool, no background
//     system, no new resource class;
//   - one document failing does not stop the rest, and the batch reports
//     each outcome separately;
//   - the batch as a whole is NOT reported as success when any document
//     failed or was ambiguous;
//   - re-running the same manifest is idempotent: unchanged documents
//     report `unchanged` and are not re-embedded.
package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ReferenceManifest is the on-disk / on-wire batch description.
type ReferenceManifest struct {
	Documents []ReferenceManifestEntry `json:"documents"`
}

// ReferenceManifestEntry is one document in a manifest.
//
// Path and URL are mutually exclusive, and supplying both is an error —
// the same rule as the single-document surface. FilePath is accepted as
// an alias for Path because the MCP surface historically used that
// spelling and operators copy it from there.
type ReferenceManifestEntry struct {
	Path      string   `json:"path,omitempty"`
	FilePath  string   `json:"filepath,omitempty"`
	URL       string   `json:"url,omitempty"`
	Title     string   `json:"title,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	Reason    string   `json:"reason,omitempty"`
	ChunkSize int      `json:"chunk_size,omitempty"`
}

// source returns the single source string for the entry.
func (e ReferenceManifestEntry) source() (string, error) {
	candidates := []string{
		strings.TrimSpace(e.Path),
		strings.TrimSpace(e.FilePath),
		strings.TrimSpace(e.URL),
	}
	var set []string
	seen := map[string]bool{}
	for _, c := range candidates {
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		set = append(set, c)
	}
	switch len(set) {
	case 0:
		return "", fmt.Errorf("entry has no source: give either a \"path\" or a \"url\"")
	case 1:
		return set[0], nil
	default:
		return "", fmt.Errorf("entry names more than one source (%q and %q); give exactly one of \"path\" or \"url\"", set[0], set[1])
	}
}

// ReferenceBatchItem is one document's outcome inside a batch.
type ReferenceBatchItem struct {
	// Index is the entry's position in the manifest (0-based), so a caller
	// can map a failure back to the file it wrote.
	Index  int    `json:"index"`
	Status string `json:"status"`
	// Source echoes what was requested, even when ingestion failed before
	// identity resolution.
	Source string `json:"source,omitempty"`
	// Error is the failure reason for status="failed".
	Error string `json:"error,omitempty"`
	// Result is the full ingestion outcome, present for every status
	// except "failed".
	Result *ReferenceIngestResult `json:"result,omitempty"`
}

// ReferenceBatchResult is the batch outcome.
type ReferenceBatchResult struct {
	Total     int `json:"total"`
	Imported  int `json:"imported"`
	Updated   int `json:"updated"`
	Unchanged int `json:"unchanged"`
	Duplicate int `json:"duplicate"`
	Failed    int `json:"failed"`
	// OK is true only when every document was imported, updated or
	// unchanged. Duplicates and failures both make it false: a duplicate
	// means the caller did not get one canonical document, and reporting
	// the batch as successful would hide that.
	OK        bool                 `json:"ok"`
	Documents []ReferenceBatchItem `json:"documents"`
}

// ParseReferenceManifest decodes and validates a manifest.
//
// Validation is up-front and per-entry: a manifest with a malformed entry
// is rejected before any document is fetched, so a typo on line 30 does
// not leave a half-imported corpus behind. The exception is a source that
// is well-formed but unreachable — that is only discoverable at fetch
// time and is reported per document.
func ParseReferenceManifest(data []byte) (*ReferenceManifest, error) {
	var manifest ReferenceManifest
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil, fmt.Errorf("manifest is empty")
	}
	if err := json.Unmarshal([]byte(trimmed), &manifest); err != nil {
		// Accept a bare array as well: it is the obvious first guess, and
		// rejecting it teaches nothing.
		var bare []ReferenceManifestEntry
		if err2 := json.Unmarshal([]byte(trimmed), &bare); err2 == nil {
			manifest.Documents = bare
		} else {
			return nil, fmt.Errorf("manifest is not valid JSON (expected {\"documents\": [...]}): %w", err)
		}
	}
	if len(manifest.Documents) == 0 {
		return nil, fmt.Errorf("manifest contains no documents")
	}
	for i, entry := range manifest.Documents {
		if _, err := entry.source(); err != nil {
			return nil, fmt.Errorf("documents[%d]: %w", i, err)
		}
		if entry.ChunkSize != 0 && (entry.ChunkSize < 64 || entry.ChunkSize > 2048) {
			return nil, fmt.Errorf("documents[%d]: chunk_size must be between 64 and 2048 (got %d)", i, entry.ChunkSize)
		}
	}
	return &manifest, nil
}

// IngestReferenceManifest ingests every document in a manifest,
// sequentially.
//
// Sequential on purpose: it bounds concurrency to one fetch and one write
// at a time, which keeps memory use and provider rate predictable for a
// corpus of a few dozen pages. The cost is latency, and accepting a
// manifest is already the "I am willing to wait" gesture.
func (dm *DatabaseManager) IngestReferenceManifest(ctx context.Context, manifest *ReferenceManifest) (*ReferenceBatchResult, error) {
	if manifest == nil {
		return nil, fmt.Errorf("IngestReferenceManifest: nil manifest")
	}
	if len(manifest.Documents) == 0 {
		return nil, fmt.Errorf("IngestReferenceManifest: manifest contains no documents")
	}
	out := &ReferenceBatchResult{
		Total:     len(manifest.Documents),
		Documents: make([]ReferenceBatchItem, 0, len(manifest.Documents)),
	}
	for i, entry := range manifest.Documents {
		source, err := entry.source()
		item := ReferenceBatchItem{Index: i, Source: source}
		if err != nil {
			item.Status = "failed"
			item.Error = err.Error()
			out.Failed++
			out.Documents = append(out.Documents, item)
			continue
		}
		// A cancelled context stops the batch rather than sprinting through
		// the remaining entries producing "failed" for each.
		if ctx.Err() != nil {
			item.Status = "failed"
			item.Error = ctx.Err().Error()
			out.Failed++
			out.Documents = append(out.Documents, item)
			continue
		}
		res, err := dm.IngestReference(ctx, ReferenceIngestRequest{
			Source:    source,
			Title:     entry.Title,
			Tags:      entry.Tags,
			Reason:    entry.Reason,
			ChunkSize: entry.ChunkSize,
		})
		if err != nil {
			item.Status = "failed"
			item.Error = err.Error()
			out.Failed++
			out.Documents = append(out.Documents, item)
			continue
		}
		item.Status = res.Status
		item.Result = res
		switch res.Status {
		case ReferenceStatusImported:
			out.Imported++
		case ReferenceStatusUpdated:
			out.Updated++
		case ReferenceStatusUnchanged:
			out.Unchanged++
		case ReferenceStatusDuplicate:
			out.Duplicate++
		default:
			out.Failed++
		}
		out.Documents = append(out.Documents, item)
	}
	out.OK = out.Failed == 0 && out.Duplicate == 0
	return out, nil
}

// FailedSources lists the sources that failed, for an error message.
func (b *ReferenceBatchResult) FailedSources() []string {
	if b == nil {
		return nil
	}
	var out []string
	for _, d := range b.Documents {
		if d.Status == "failed" || d.Status == ReferenceStatusDuplicate {
			out = append(out, d.Source)
		}
	}
	return out
}
