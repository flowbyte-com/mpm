// reference_tools.go — the legacy entry points for reference ingestion.
//
// These methods predate the unified pipeline and are kept as thin
// adapters so existing callers (the MCP handler, the CoreDB interface,
// and older tests) keep their signatures. All of the actual work —
// identity resolution, parsing, extraction, chunking, the content diff,
// embedding and outcome classification — lives in IngestReference
// (reference_ingest.go). There is exactly one ingestion implementation.
package internal

import (
	"context"
	"fmt"
)

// AddReferenceFromFile ingests a local file into the reference library.
//
// Deprecated: call IngestReference when you need the structured outcome
// (status, chunk diff, embedding state). This adapter returns the legacy
// map shape for callers that have not migrated.
func (dm *DatabaseManager) AddReferenceFromFile(filepath, title string) (map[string]interface{}, error) {
	return dm.AddReferenceFromFileWith(filepath, title, nil, "", 0)
}

// AddReferenceFromFileWith is AddReferenceFromFile with tags, an import
// reason and a chunk size.
//
// Deprecated: call IngestReference. See AddReferenceFromFile.
func (dm *DatabaseManager) AddReferenceFromFileWith(filepath, title string, tags []string, reason string, chunkSize int) (map[string]interface{}, error) {
	res, err := dm.IngestReference(context.Background(), ReferenceIngestRequest{
		FilePath:  filepath,
		Title:     title,
		Tags:      tags,
		Reason:    reason,
		ChunkSize: chunkSize,
	})
	if err != nil {
		return nil, err
	}
	if res.Status == ReferenceStatusDuplicate {
		return nil, fmt.Errorf("%s already exists as %d separate documents; nothing was imported (run `mpm reference duplicates`)",
			res.Source, len(res.DuplicateIDs))
	}
	// Legacy shape. `unchanged` was the only status this surface could
	// express before; it is preserved because callers branch on it.
	out := map[string]interface{}{
		"success":      true,
		"id":           res.ID,
		"title":        res.Title,
		"total_chunks": res.TotalChunks,
		"embedded":     res.Embedded,
		"status":       res.Status,
	}
	if res.Status == ReferenceStatusUnchanged {
		out["unchanged"] = true
	}
	return out, nil
}

// parseReferenceFile is the legacy single-value parser used by older
// tests. New code should use parseReferenceDocument, which also returns
// the document title and extraction notes.
func parseReferenceFile(filepath string) (string, error) {
	content, _, err := parseReferenceDocument(filepath)
	return content, err
}
