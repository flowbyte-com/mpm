// reference_ingest.go — the single ingestion pipeline for reference
// documents.
//
// Before this file there were two implementations of "add a reference":
// the CLI handler in cmd/mpm/simple_cmds.go (tags, reason, chunk size,
// its own parse/chunk/lookup sequence) and the MCP handler in
// internal/core/tools/handlers.go (filepath + title only, routed through
// AddReferenceFromFile). They drifted exactly as duplicated ingestion
// logic does: the agent-facing surface could not attach tags or an
// import reason, and neither surface could say what it had actually done
// beyond "success".
//
// IngestReference is now the one implementation. Every surface — CLI
// `mpm reference add`, `mpm reference import`, MCP `mpm_references
// action=add|import` — resolves a source to its identity and calls this.
// A surface cannot diverge from the others without changing this file.
//
// What the result reports, and why each field exists:
//
//	status          imported | updated | unchanged | duplicate
//	id              the document id (stable across re-imports)
//	source          the canonical identity written to file_path
//	chunks_*        applied diff counts, from the transaction that
//	                actually wrote them
//	embedded_*      storage success and embedding success are separate
//	                facts, and the caller must be able to tell them apart
//	warnings        extraction limits and metadata merges the caller
//	                should know about but that are not failures
package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Reference ingest statuses. Callers switch on these strings; the values
// are part of the tool contract.
const (
	ReferenceStatusImported  = "imported"
	ReferenceStatusUpdated   = "updated"
	ReferenceStatusUnchanged = "unchanged"
	ReferenceStatusDuplicate = "duplicate"
)

// ReferenceIngestRequest is one document to ingest.
//
// Source is the only required field. It is a local path or an http(s)
// URL; the two are never both supplied, because there is exactly one
// source parameter. Mixed intent is rejected rather than resolved by
// precedence.
type ReferenceIngestRequest struct {
	// Source is a local file path or an http(s) URL.
	Source string
	// FilePath / URL are the explicit split forms. Supplying both (or a
	// Source plus one of them, disagreeing) is an error. Supplying
	// exactly one is equivalent to Source.
	FilePath string
	URL      string
	// Title overrides the extracted/filename title.
	Title string
	// Tags are unioned with any tags already on the document.
	Tags []string
	// Reason replaces the stored import reason when non-empty.
	Reason string
	// ChunkSize is the target token count per chunk (64–2048; 0 = 512).
	ChunkSize int
}

// ReferenceIngestResult is the structured outcome of one ingestion.
type ReferenceIngestResult struct {
	Status string `json:"status"`
	ID     string `json:"id,omitempty"`
	Title  string `json:"title,omitempty"`
	// Source is the canonical identity: absolute symlink-resolved path,
	// or effective URL.
	Source string `json:"source,omitempty"`
	// SourceKind is "file" or "url".
	SourceKind string `json:"source_kind,omitempty"`
	// SourceType is the parsed document format ("markdown", "html", …).
	SourceType string `json:"source_type,omitempty"`
	// RequestedSource is what the caller supplied, kept so a redirect or
	// a relative-path rewrite is visible.
	RequestedSource string `json:"requested_source,omitempty"`

	TotalChunks   int `json:"total_chunks"`
	ChunksAdded   int `json:"chunks_added"`
	ChunksUpdated int `json:"chunks_updated"`
	ChunksReused  int `json:"chunks_reused"`
	ChunksRemoved int `json:"chunks_removed"`

	// Embedded counts chunks newly given a vector by this call.
	Embedded int `json:"embedded"`
	// PendingEmbeddings counts chunks still without a vector after this
	// call. Non-zero means semantic retrieval cannot see them; lexical
	// retrieval still can.
	PendingEmbeddings int `json:"pending_embeddings"`
	// LexicalSearchable / SemanticSearchable are the honest retrieval
	// claims for this document right now.
	LexicalSearchable  bool `json:"lexical_searchable"`
	SemanticSearchable bool `json:"semantic_searchable"`

	// DuplicateIDs is populated for status=duplicate: several existing
	// documents already resolve to this identity. Nothing was written.
	DuplicateIDs []string `json:"duplicate_ids,omitempty"`
	// Warnings are non-fatal observations (extraction limits, metadata
	// merges, embedding unavailability).
	Warnings []string `json:"warnings,omitempty"`
	// ExtractionNotes describe how the source bytes were turned into
	// text, including anything that could not be preserved.
	ExtractionNotes []string `json:"extraction_notes,omitempty"`
}

// ErrReferenceSourceRequired is returned when a request names no source.
var ErrReferenceSourceRequired = fmt.Errorf("a source is required: pass a local file path or an http(s) URL")

// resolveRequestSource applies the mutually-exclusive source rules and
// returns the single source string plus its explicit kind, if any.
//
// Supplying two different sources is a caller bug that must not be
// resolved silently: choosing one would import the wrong document and
// the caller would never learn why.
func (r ReferenceIngestRequest) resolveRequestSource() (string, error) {
	source := strings.TrimSpace(r.Source)
	filePath := strings.TrimSpace(r.FilePath)
	rawURL := strings.TrimSpace(r.URL)

	set := 0
	for _, s := range []string{source, filePath, rawURL} {
		if s != "" {
			set++
		}
	}
	switch {
	case set == 0:
		return "", ErrReferenceSourceRequired
	case set > 1:
		// Equal duplicates ("source" plus the explicit form naming the
		// same thing) are redundant but not contradictory.
		nonEmpty := map[string]bool{}
		for _, s := range []string{source, filePath, rawURL} {
			if s != "" {
				nonEmpty[s] = true
			}
		}
		if len(nonEmpty) > 1 {
			return "", fmt.Errorf("conflicting sources: supply exactly one of a local path or a URL (got %q and %q)", firstKey(nonEmpty, source, filePath), otherKey(nonEmpty, source, filePath))
		}
	}
	if filePath != "" {
		if IsURLSource(filePath) {
			return filePath, nil
		}
		return filePath, nil
	}
	if rawURL != "" {
		if !IsURLSource(rawURL) {
			return "", fmt.Errorf("--url requires an http(s) URL, got %q", rawURL)
		}
		return rawURL, nil
	}
	return source, nil
}

func firstKey(set map[string]bool, order ...string) string {
	for _, s := range order {
		if s != "" && set[s] {
			return s
		}
	}
	for s := range set {
		return s
	}
	return ""
}

func otherKey(set map[string]bool, order ...string) string {
	first := firstKey(set, order...)
	for _, s := range order {
		if s != "" && set[s] && s != first {
			return s
		}
	}
	for s := range set {
		if s != first {
			return s
		}
	}
	return ""
}

// IngestReference resolves, parses, chunks and stores one document.
//
// Idempotency contract:
//   - same identity + same content → status=unchanged, no chunk writes,
//     embeddings preserved;
//   - same identity + changed content → status=updated, unchanged chunks
//     reused (ids and vectors preserved), changed chunks re-inserted with
//     their embedding cleared, orphan chunks deleted;
//   - unknown identity → status=imported;
//   - identity resolves to more than one existing document →
//     status=duplicate with the ids and no write.
func (dm *DatabaseManager) IngestReference(ctx context.Context, req ReferenceIngestRequest) (*ReferenceIngestResult, error) {
	if dm == nil || dm.db == nil {
		return nil, fmt.Errorf("IngestReference: database not initialized")
	}
	source, err := req.resolveRequestSource()
	if err != nil {
		return nil, err
	}

	identity, err := ResolveReferenceIdentity(source)
	if err != nil {
		return nil, err
	}

	if req.ChunkSize != 0 && (req.ChunkSize < 64 || req.ChunkSize > 2048) {
		return nil, fmt.Errorf("chunk size must be between 64 and 2048 tokens (got %d)", req.ChunkSize)
	}

	// Resolve the identity before fetching. Two reasons: a URL re-import
	// can then send If-None-Match / If-Modified-Since from the stored
	// validators and get a 304 instead of re-downloading the document, and
	// a fetch failure cannot leave a document row behind because no write
	// has happened yet.
	// No content hash yet, so only decisive evidence is used: canonical,
	// absolute-path or URL rows. A weak (relative-path) legacy candidate
	// must not supply HTTP validators — sending an unrelated document's
	// ETag could produce a 304 for content this row never held.
	preMatch, err := resolveReferenceIdentity(dm.db, identity, "")
	if err != nil {
		return nil, fmt.Errorf("resolve existing reference: %w", err)
	}
	acquired, err := dm.acquireReferenceContent(ctx, identity, preMatch)
	if err != nil {
		return nil, err
	}

	// The write phase can lose a race with a concurrent ingest of the same
	// source: both resolve to "no existing document", both build the same
	// deterministic id, and one of them then fails with SQLITE_BUSY /
	// SQLITE_BUSY_SNAPSHOT. WithTx deliberately does not retry callbacks
	// (see its contract), and its per-statement retry cannot help once a
	// read snapshot is stale, so the retry lives here — re-resolving the
	// identity each attempt so the loser of the race sees the winner's row
	// and applies an update to it instead of failing.
	const attempts = 5
	backoff := 20 * time.Millisecond
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			time.Sleep(backoff)
			if backoff < 400*time.Millisecond {
				backoff *= 2
			}
		}
		res, err := dm.ingestAcquiredReference(ctx, identity, acquired, req, source)
		if err == nil {
			return res, nil
		}
		if !IsBusyError(err) {
			return nil, err
		}
		lastErr = err
	}
	return nil, fmt.Errorf("store reference: database stayed locked after %d attempts: %w", attempts, lastErr)
}

// ingestAcquiredReference performs one resolve-and-write pass for a source
// whose text has already been obtained.
func (dm *DatabaseManager) ingestAcquiredReference(ctx context.Context, identity SourceIdentity, acquired *acquiredReferenceContent, req ReferenceIngestRequest, source string) (*ReferenceIngestResult, error) {
	content := acquired.Content
	contentHash := HashContent(content)

	match, err := resolveReferenceIdentity(dm.db, identity, contentHash)
	if err != nil {
		return nil, fmt.Errorf("resolve existing reference: %w", err)
	}
	if len(match.AmbiguousIDs) > 0 {
		return &ReferenceIngestResult{
			Status:          ReferenceStatusDuplicate,
			Source:          identity.Canonical,
			SourceKind:      identity.Kind,
			RequestedSource: source,
			DuplicateIDs:    match.AmbiguousIDs,
			Warnings: []string{
				fmt.Sprintf("%d existing documents resolve to this source; nothing was imported. "+
					"Run `mpm reference duplicates` for the repair plan, then shred the copies you do not want and re-import.",
					len(match.AmbiguousIDs)),
			},
		}, nil
	}

	existing := match.Doc
	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = acquired.Title
	}
	if title == "" {
		title = defaultReferenceTitle(identity)
	}

	result := &ReferenceIngestResult{
		Source:          identity.Canonical,
		SourceKind:      identity.Kind,
		RequestedSource: source,
		SourceType:      acquired.SourceType,
		Title:           title,
		ExtractionNotes: acquired.Notes,
		Warnings:        acquired.Warnings,
	}

	if len(match.RejectedAliasIDs) > 0 {
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("%d legacy document(s) store a relative path that could also name this source, but their content differs (%s); "+
				"they were left untouched and this source was imported separately. Run `mpm reference duplicates` to reconcile.",
				len(match.RejectedAliasIDs), strings.Join(match.RejectedAliasIDs, ", ")))
	}

	// Unchanged content: refresh metadata only. Chunk rows (and therefore
	// chunk ids and embeddings) are not touched — that is the whole point
	// of the diff-based ingest.
	// A 304 means the publisher confirms the stored document is current.
	// Keep it byte-for-byte: no chunk writes, no re-embedding, and the
	// stored content hash stays valid.
	if acquired.NotModified && existing != nil {
		mergedTags := mergeReferenceTags(existing.Tags, req.Tags)
		reason := existing.ImportReason
		if strings.TrimSpace(req.Reason) != "" {
			reason = strings.TrimSpace(req.Reason)
		}
		title := existing.Title
		if strings.TrimSpace(req.Title) != "" {
			title = strings.TrimSpace(req.Title)
		}
		if err := dm.updateReferenceMetadata(existing.ID, title, mergedTags, reason, identity, acquired); err != nil {
			return nil, err
		}
		res := &ReferenceIngestResult{
			Status:            ReferenceStatusUnchanged,
			ID:                existing.ID,
			Title:             title,
			Source:            identity.Canonical,
			SourceKind:        identity.Kind,
			RequestedSource:   source,
			SourceType:        existing.SourceType,
			TotalChunks:       existing.TotalChunks,
			ChunksReused:      existing.TotalChunks,
			LexicalSearchable: existing.TotalChunks > 0,
			Warnings:          []string{"the publisher reported no change (HTTP 304); the stored document and its embeddings were kept as-is"},
		}
		dm.fillReferenceRetrievalState(ctx, existing.ID, res)
		return res, nil
	}
	if acquired.NotModified && existing == nil {
		return nil, fmt.Errorf("fetch %s: server replied 304 Not Modified but no stored copy exists; retry without conditional headers", identity.Canonical)
	}

	if existing != nil && existing.ContentHash == contentHash {
		mergedTags := mergeReferenceTags(existing.Tags, req.Tags)
		reason := existing.ImportReason
		if strings.TrimSpace(req.Reason) != "" {
			reason = strings.TrimSpace(req.Reason)
		}
		if err := dm.updateReferenceMetadata(existing.ID, title, mergedTags, reason, identity, acquired); err != nil {
			return nil, err
		}
		result.Status = ReferenceStatusUnchanged
		result.ID = existing.ID
		result.TotalChunks = existing.TotalChunks
		result.ChunksReused = existing.TotalChunks
		dm.fillReferenceRetrievalState(ctx, existing.ID, result)
		if tagsDiffer(existing.Tags, req.Tags) {
			result.Warnings = append(result.Warnings, "content unchanged; tags were merged onto the existing document")
		}
		return result, nil
	}

	chunks, err := chunkReferenceDocument(content, acquired.SourceType, req.ChunkSize)
	if err != nil {
		return nil, fmt.Errorf("chunk reference: %w", err)
	}
	if len(chunks) == 0 {
		return nil, fmt.Errorf("no extractable text in %s (nothing was stored)", source)
	}

	now := strconv.FormatInt(time.Now().Unix(), 10)
	docID := ReferenceDocIDForIdentity(identity)
	if existing != nil {
		// Reuse the existing id so the chunk diff replaces chunks in
		// place rather than creating a second document.
		docID = existing.ID
	}
	doc := &ReferenceDoc{
		ID:               docID,
		Title:            title,
		SourcePath:       identity.Canonical,
		SourceType:       acquired.SourceType,
		Tags:             mergeReferenceTags(nil, req.Tags),
		ImportReason:     strings.TrimSpace(req.Reason),
		Content:          content,
		ContentHash:      contentHash,
		TotalChunks:      len(chunks),
		LastIndexed:      now,
		Created:          now,
		SourceKind:       identity.Kind,
		SourceURL:        acquired.EffectiveURL,
		RequestedURL:     acquired.RequestedURL,
		HTTPETag:         acquired.ETag,
		HTTPLastModified: acquired.LastModified,
		Attribution:      referenceAttributionJSON(acquired),
	}
	if !acquired.FetchedAt.IsZero() {
		doc.FetchedAt = acquired.FetchedAt.Unix()
	}

	refChunks := make([]ReferenceChunk, len(chunks))
	for i, c := range chunks {
		refChunks[i] = ReferenceChunk{
			ID:         ComputeChunkID(docID, c.Index, contentHash),
			DocID:      docID,
			ChunkIndex: c.Index,
			Section:    c.Section,
			Content:    c.Content,
			SourcePath: identity.Canonical,
		}
	}

	diff, err := dm.applyReference(doc, refChunks)
	if err != nil {
		return nil, fmt.Errorf("store reference: %w", err)
	}

	result.Status = ReferenceStatusImported
	if existing != nil {
		result.Status = ReferenceStatusUpdated
	}
	result.ID = docID
	result.TotalChunks = len(chunks)
	if diff != nil {
		result.ChunksAdded = len(diff.Inserted)
		result.ChunksUpdated = len(diff.Updated)
		result.ChunksReused = len(diff.Unchanged)
		result.ChunksRemoved = len(diff.Deleted)
	}
	if existing != nil && match.MatchedVia == "legacy-alias" && match.AliasPath != "" {
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("matched an existing document stored under the non-canonical source %q; its identity was upgraded to %s", match.AliasPath, identity.Canonical))
	}

	dm.fillReferenceRetrievalState(ctx, docID, result)
	return result, nil
}

// acquireReferenceContent turns a source into text plus the metadata that
// belongs in the document row.
//
// The file branch is here; the URL branch is the fetch pipeline
// (reference_fetch.go), which owns its own security boundary.
func (dm *DatabaseManager) acquireReferenceContent(ctx context.Context, identity SourceIdentity, existing *ReferenceIdentityMatch) (*acquiredReferenceContent, error) {
	switch identity.Kind {
	case ReferenceSourceFile:
		return acquireReferenceFile(identity)
	case ReferenceSourceURL:
		var cond *referenceConditional
		if existing != nil && existing.Doc != nil && (existing.Doc.HTTPETag != "" || existing.Doc.HTTPLastModified != "") {
			cond = &referenceConditional{
				ETag:         existing.Doc.HTTPETag,
				LastModified: existing.Doc.HTTPLastModified,
			}
		}
		return fetchReferenceURL(ctx, identity, cond)
	default:
		return nil, fmt.Errorf("unsupported source kind %q", identity.Kind)
	}
}

// acquiredReferenceContent is the text and provenance of one source,
// independent of how it was obtained.
type acquiredReferenceContent struct {
	// Content is the text that will be chunked and stored.
	Content string
	// Title is the document's own title, or "" when the format has none.
	Title string
	// SourceType is the parsed format (markdown, html, pdf, …).
	SourceType string
	// EffectiveURL is the post-redirect URL for URL sources.
	EffectiveURL string
	// RequestedURL is the URL the caller asked for.
	RequestedURL string
	// FetchedAt is when the retrieval succeeded (zero for local files).
	FetchedAt time.Time
	// ETag / LastModified are the publisher's validators.
	ETag         string
	LastModified string
	// Notes describe extraction decisions (including losses).
	Notes []string
	// Warnings are non-fatal observations.
	Warnings []string
	// SiteName / Byline / License are publisher attribution, kept as
	// document metadata so retrieving a chunk can cite its source without
	// the attribution text being part of the chunk body.
	SiteName string
	Byline   string
	License  string
	// NotModified is set when a conditional request was answered with 304:
	// the stored document is still current and nothing was transferred.
	NotModified bool
}

// acquireReferenceFile reads and parses a local document.
func acquireReferenceFile(identity SourceIdentity) (*acquiredReferenceContent, error) {
	path := identity.Canonical
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("file not found: %s", identity.Raw)
		}
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	// Defence-in-depth ceiling on the input. The parse libraries can
	// expand compressed formats (EPUB, PDF) well beyond the file size, so
	// this bounds the input, not peak memory.
	const maxReferenceBytes = 100 << 20
	if info.Size() > maxReferenceBytes {
		return nil, fmt.Errorf("reference file too large: %d bytes (max %d = 100 MiB)", info.Size(), maxReferenceBytes)
	}

	content, notes, err := parseReferenceDocument(path)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(content) == "" {
		return nil, fmt.Errorf("%s contains no extractable text", identity.Raw)
	}
	return &acquiredReferenceContent{
		Content:    content,
		Title:      titleFromReferenceContent(path, content),
		SourceType: DetectSourceType(path),
		Notes:      notes,
	}, nil
}

// updateReferenceMetadata refreshes a document's descriptive fields
// without touching its content, chunks or embeddings.
//
// Identity is upgraded here: a legacy row matched through an alias gets
// its canonical file_path written, so the next import takes the exact
// path and the alias scan is not needed again. This is a per-row repair
// on the row the operator just imported — not a bulk migration.
func (dm *DatabaseManager) updateReferenceMetadata(id, title string, tags []string, reason string, identity SourceIdentity, acquired *acquiredReferenceContent) error {
	if title == "" {
		var existingTitle string
		if err := dm.db.QueryRow(`SELECT title FROM reference_docs WHERE id = ?`, id).Scan(&existingTitle); err == nil {
			title = existingTitle
		}
	}
	tagsJSON, _ := MarshalJSON(tags)
	_, err := dm.db.Exec(`
		UPDATE reference_docs
		   SET title = ?,
		       tags = ?,
		       import_reason = ?,
		       file_path = ?,
		       source_kind = COALESCE(NULLIF(?, ''), source_kind),
		       source_url = COALESCE(NULLIF(?, ''), source_url),
		       requested_url = COALESCE(NULLIF(?, ''), requested_url),
		       fetched_at = COALESCE(NULLIF(?, 0), fetched_at),
		       http_etag = COALESCE(NULLIF(?, ''), http_etag),
		       http_last_modified = COALESCE(NULLIF(?, ''), http_last_modified),
		       last_indexed = CAST(strftime('%s','now') AS INTEGER)
		 WHERE id = ?`,
		title, tagsJSON, reason, identity.Canonical,
		identity.Kind, acquired.EffectiveURL, acquired.RequestedURL, acquiredFetchedAt(acquired),
		acquired.ETag, acquired.LastModified, id,
	)
	if err != nil {
		return fmt.Errorf("update reference metadata: %w", err)
	}
	return nil
}

// referenceAttributionJSON renders the publisher identity that the source
// actually advertised, omitting empty fields. Returns "" when nothing was
// advertised so the column stays NULL rather than holding "{}".
func referenceAttributionJSON(a *acquiredReferenceContent) string {
	if a == nil {
		return ""
	}
	fields := map[string]string{}
	if a.SiteName != "" {
		fields["site_name"] = a.SiteName
	}
	if a.Byline != "" {
		fields["author"] = a.Byline
	}
	if a.License != "" {
		fields["license"] = a.License
	}
	if len(fields) == 0 {
		return ""
	}
	b, err := json.Marshal(fields)
	if err != nil {
		return ""
	}
	return string(b)
}

func acquiredFetchedAt(a *acquiredReferenceContent) int64 {
	if a == nil || a.FetchedAt.IsZero() {
		return 0
	}
	return a.FetchedAt.Unix()
}

// fillReferenceRetrievalState computes the retrieval claims for a
// document after a write.
//
// These are measured, not assumed: storage success says nothing about
// embedding success, and a document with pending embeddings is
// lexically searchable but not semantically searchable. Reporting the
// difference is the difference between "searchable" and "will be found
// by the query you are about to run".
func (dm *DatabaseManager) fillReferenceRetrievalState(ctx context.Context, docID string, out *ReferenceIngestResult) {
	if docID == "" || out == nil {
		return
	}
	var total, pending int
	if err := dm.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(CASE WHEN embedding IS NULL THEN 1 ELSE 0 END),0)
		   FROM reference_chunks WHERE doc_id = ?`, docID,
	).Scan(&total, &pending); err != nil {
		out.Warnings = append(out.Warnings, "could not read embedding state: "+err.Error())
		return
	}
	out.TotalChunks = total
	out.LexicalSearchable = total > 0
	// Only run an embedding pass when there is a provider that can
	// actually produce a vector. With a null/absent provider
	// EmbedReferenceChunks writes NULL + source 'null' for every chunk and
	// counts each as "embedded", which would report a write that produced
	// nothing and would churn the table on every unchanged re-import.
	if pending > 0 && CurrentEmbeddingFingerprint().Model != "" {
		embedded, _, embedErr := dm.EmbedReferenceChunks(ctx, docID)
		if embedErr != nil && ctx.Err() == nil {
			out.Warnings = append(out.Warnings, "embedding pass failed: "+embedErr.Error())
		}
		out.Embedded += embedded
		if err := dm.db.QueryRowContext(ctx,
			`SELECT COALESCE(SUM(CASE WHEN embedding IS NULL THEN 1 ELSE 0 END),0)
			   FROM reference_chunks WHERE doc_id = ?`, docID,
		).Scan(&pending); err != nil {
			out.Warnings = append(out.Warnings, "could not re-read embedding state: "+err.Error())
		}
	}
	out.PendingEmbeddings = pending
	out.SemanticSearchable = pending == 0 && total > 0
	if pending > 0 {
		if CurrentEmbeddingFingerprint().Model == "" {
			out.Warnings = append(out.Warnings,
				fmt.Sprintf("%d of %d chunks have no embedding: no embedding provider is configured, so semantic retrieval will not see this document (lexical search still works). Configure one and run `mpm ops backfill-embeddings --include references`.", pending, total))
		} else {
			out.Warnings = append(out.Warnings,
				fmt.Sprintf("%d of %d chunks could not be embedded; semantic retrieval will not see them (lexical search still works). Retry with `mpm ops backfill-embeddings --include references`.", pending, total))
		}
	}
}

// mergeReferenceTags unions the existing tags with the supplied ones,
// preserving order: existing first, then new, de-duplicated
// case-insensitively.
//
// Union rather than replace, because a retry of the same import must be
// idempotent and an import that mentions one tag must not silently drop
// the others a previous import recorded.
func mergeReferenceTags(existing, added []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(existing)+len(added))
	for _, group := range [][]string{existing, added} {
		for _, t := range group {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			key := strings.ToLower(t)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, t)
		}
	}
	return out
}

// tagsDiffer reports whether the supplied tags would add anything.
func tagsDiffer(existing, added []string) bool {
	if len(added) == 0 {
		return false
	}
	have := map[string]bool{}
	for _, t := range existing {
		have[strings.ToLower(strings.TrimSpace(t))] = true
	}
	for _, t := range added {
		t = strings.ToLower(strings.TrimSpace(t))
		if t != "" && !have[t] {
			return true
		}
	}
	return false
}

// defaultReferenceTitle derives a title from the identity when the
// document format provides none.
func defaultReferenceTitle(identity SourceIdentity) string {
	if identity.Kind == ReferenceSourceURL {
		if u, err := parseURLForTitle(identity.Canonical); err == nil {
			return u
		}
		return identity.Canonical
	}
	return filepath.Base(identity.Canonical)
}

// parseURLForTitle renders a URL as a readable fallback title: the last
// non-empty path segment, or the host when the path is "/".
func parseURLForTitle(raw string) (string, error) {
	u, err := parseAbsoluteURL(raw)
	if err != nil {
		return "", err
	}
	path := strings.Trim(u.Path, "/")
	if path == "" {
		return u.Host, nil
	}
	segments := strings.Split(path, "/")
	return segments[len(segments)-1], nil
}

// MarshalReferenceResultJSON renders an ingest result for CLI --json
// output. Kept here so the CLI and any other consumer emit the same key
// set for the same document.
func MarshalReferenceResultJSON(out *ReferenceIngestResult) (string, error) {
	b, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
