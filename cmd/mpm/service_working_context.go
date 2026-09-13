// cmd/mpm/services/working_context_service.go — Services layer for
// the working-context domain.
//
// WorkingContextService EARNED its existence (RFC §7) by owning
// behaviour that crosses persistence:
//
//   - GetCurrent: applies expiry policy (validation + lifecycle), so
//     expired contexts surface as "no current context" rather than
//     half-delivered data.
//   - Promote: cross-cutting operation — read scratchpad, save a
//     memory through the canonical save path, delete scratchpad,
//     all atomic. Spans persistence + policy + lineage tagging.
//   - Clear: removes the working context with policy choice about
//     audit trail (we choose hard delete; the schema's TTL decay
//     was designed for this).
//
// If this service disappeared, every command that called it would
// have to reimplement expiry policy + promote atomicity + clear
// semantics — losing behaviour, not just routing.
//
// Layering contract (RFC §7):
//   WorkingContextService composes WorkingContextStore. It does NOT
//   touch SQLite directly. It does NOT call other commands. It
//   does NOT own rendering.
//
// Composition discipline: services do NOT call commands. This
// service returns domain values (WorkingContext, PromoteResult, nil)
// — formatter and renderer layers decide presentation.

package main

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/usererror"
)

// PromoteResult is the outcome of WorkingContextService.Promote. Returned
// to the caller for Formatter/Renderer to format. Includes the new memory
// id so the caller can cite it in audit / next-step messaging.
type PromoteResult struct {
	// MemoryID is the canonical id assigned by SaveMemoryWithExtras to
	// the promoted permanent memory. Empty on failure.
	MemoryID string
	// SessionID is the scratchpad session that was promoted.
	SessionID string
	// Thesis is the original thesis text (preserved for the caller's UI).
	Thesis string
}

// WorkingContextService is the behavioural boundary for ephemeral
// working-context state.
type WorkingContextService struct {
	store        *WorkingContextStore
	memoryWriter MemoryWriter
}

// MemoryWriter is the minimal interface WorkingContextService needs
// to convert ephemeral content into a permanent memory. The default
// production implementation wraps DatabaseManager.SaveMemoryWithExtras;
// a test double can override for unit tests without a live DB.
type MemoryWriter interface {
	// SaveMemory persists content as a permanent memory tagged with the
	// supplied lineage tag (typically "from-scratchpad:<sessionID>").
	// Returns the new memory id.
	SaveMemory(content string, lineageTag string) (string, error)
	// FindPromotionByLineage returns the canonical id of an existing
	// memory whose tags include the supplied lineage tag, or "" if
	// none exists. Used by Promote for idempotency: a retry after a
	// partial success (memory saved but scratchpad delete failed)
	// returns the existing memory id rather than creating a duplicate.
	FindPromotionByLineage(lineageTag string) (string, error)
}

// NewWorkingContextService wires the service to its dependencies.
// Returns nil if either dep is nil — callers check.
func NewWorkingContextService(store *WorkingContextStore, writer MemoryWriter) *WorkingContextService {
	if store == nil || writer == nil {
		return nil
	}
	return &WorkingContextService{store: store, memoryWriter: writer}
}

// GetCurrent loads the working context for a session and applies
// expiry policy. Returns (nil, nil) when:
//
//   - no row exists for the session, OR
//   - the row exists but has expired past decay_at
//
// Both cases surface as "no current context" — never as an error —
// because expiry is a normal lifecycle state, not an exceptional
// condition.
//
// A real error is only returned for genuine store failure (DB down,
// schema mismatch, permission denied).
func (s *WorkingContextService) GetCurrent(sessionID string) (*WorkingContext, error) {
	if sessionID == "" {
		return nil, mpminternal.ErrSessionIDRequired()
	}
	wc, err := s.store.Load(sessionID)
	if err != nil {
		if errors.Is(err, ErrWorkingContextNotFound) {
			return nil, nil
		}
		return nil, err
	}
	// Behaviour: enforce expiry policy. A Store that did this would
	// violate layering (persistence owning policy). Service owns it.
	if wc.IsExpired(time.Now().UTC()) {
		return nil, nil
	}
	return wc, nil
}

// Promote converts an ephemeral working context into a permanent
// memory. Behaviour:
//
//   1. Read current scratchpad via Store.
//   2. Build content from Thesis + Supporting.
//   3. Save via canonical MemoryWriter (which routes through
//      SaveMemoryWithExtras → scanner → atomic INSERT).
//   4. Delete scratchpad via Store.
//
// Step 1 happens BEFORE Step 4 — if 3 fails the scratchpad is
// preserved for retry. The atomicity guarantees are inherited from
// SaveMemoryWithExtras's scanner path (rejection aborts before any
// DB write completes); the scratchpad delete is a separate operation
// against the same DB, but the failure modes are well-bounded:
//
//   - Read fails → return error, scratchpad untouched.
//   - Save fails → return error, scratchpad untouched.
//   - Delete fails after successful save → log warning, return
//     success with the new memory id. The promoted memory exists;
//     a stale scratchpad will TTL out via decay_at.
//
// Idempotency (defect A retry safety, 2026-09-13 acceptance):
// before saving, the service queries the canonical memory substrate
// for any existing memory tagged with the lineage key. If one
// exists, the service returns that id without creating a duplicate.
// This makes retry-after-partial-success safe — the second promote
// finds the memory that the first promote already created and
// returns it as the result, with no second insert.
//
// Lineage: the new memory is tagged with "from-scratchpad:<sessionID>"
// via the injected MemoryWriter. This is the cross-cutting policy
// behaviour that earned this service its existence.
func (s *WorkingContextService) Promote(sessionID string) (*PromoteResult, error) {
	if sessionID == "" {
		return nil, mpminternal.ErrSessionIDRequired()
	}
	wc, err := s.store.Load(sessionID)
	if err != nil {
		if errors.Is(err, ErrWorkingContextNotFound) {
			return nil, fmt.Errorf("no working context to promote for session: %s", sessionID)
		}
		return nil, err
	}
	if wc.IsExpired(time.Now().UTC()) {
		return nil, fmt.Errorf("working context for session %s has expired; nothing to promote", sessionID)
	}

	lineage := fmt.Sprintf("from-scratchpad:%s", sessionID)

	// Idempotency check: if a memory with this lineage already
	// exists, a previous promote succeeded (or partially succeeded)
	// and we must NOT create a duplicate. Return the existing id
	// so the operator's retry sees the same outcome.
	existingID, lookupErr := s.memoryWriter.FindPromotionByLineage(lineage)
	if lookupErr != nil {
		return nil, fmt.Errorf("promote: idempotency lookup failed: %w", lookupErr)
	}
	if existingID != "" {
		return &PromoteResult{
			MemoryID:  existingID,
			SessionID: sessionID,
			Thesis:    wc.Thesis,
		}, nil
	}

	content := buildPromoteContent(wc)
	memoryID, err := s.memoryWriter.SaveMemory(content, lineage)
	if err != nil {
		return nil, fmt.Errorf("promote: save memory failed: %w", err)
	}

	if delErr := s.store.Delete(sessionID); delErr != nil {
		// Memory was created. The idempotency check above means a
		// retry will return the same memory id; the stale scratchpad
		// will TTL out via decay_at, OR the operator can run
		// `mpm work clear` to retry the delete explicitly.
		usererror.Warn("promoted memory %s created but scratchpad delete failed: %v (retry-safe via idempotency)", memoryID, delErr)
	}

	return &PromoteResult{
		MemoryID:  memoryID,
		SessionID: sessionID,
		Thesis:    wc.Thesis,
	}, nil
}

// Clear removes the working context for a session. Idempotent.
//
// Behaviour: hard delete only. The TTL decay_at on the scratchpad
// already provides eventual cleanup; mpm work clear is the explicit
// operator-driven path. Audit trail choice: we DO NOT archive
// pre-clear (operators who want an audit trail promote first via
// `mpm work promote`).
func (s *WorkingContextService) Clear(sessionID string) error {
	if sessionID == "" {
		return mpminternal.ErrSessionIDRequired()
	}
	return s.store.Delete(sessionID)
}

// buildPromoteContent is the small amount of presentation-shaped
// behaviour that is allowed to live in the service (and not the
// formatter) because it's the canonical shape that BOTH the CLI and
// any future substrate consumer (e.g., MCP tool, rest API) need to
// agree on. Format match: handlePromoteScratchpad in internal/core/tools/handlers.go
func buildPromoteContent(wc *WorkingContext) string {
	if wc == nil {
		return ""
	}
	content := fmt.Sprintf("Thesis: %s", wc.Thesis)
	if wc.Supporting != "" {
		content += fmt.Sprintf("\nSupporting Context: %s", wc.Supporting)
	}
	return content
}

// DatabaseManagerMemoryWriter is the production MemoryWriter
// implementation. It wraps the canonical SaveMemoryWithExtras path
// so the scanner runs and atomicity is preserved.
type DatabaseManagerMemoryWriter struct {
	dm *mpminternal.DatabaseManager
}

// NewDatabaseManagerMemoryWriter wires a DatabaseManager into the
// MemoryWriter interface. Returns nil if dm is nil.
func NewDatabaseManagerMemoryWriter(dm *mpminternal.DatabaseManager) *DatabaseManagerMemoryWriter {
	if dm == nil {
		return nil
	}
	return &DatabaseManagerMemoryWriter{dm: dm}
}

// SaveMemory persists the promoted working-context content as a
// permanent memory in the standard memories collection, with the
// supplied lineage tag. Routes through SaveMemoryWithExtras — the
// same path the MCP `promote_scratchpad` tool uses — so the
// 20-pattern scanner runs and rejection aborts before any DB write
// completes.
//
// Note on signature: SaveMemoryWithExtras's full signature is wide;
// we expose only the slice the WorkingContextService needs (content,
// lineage tag) and let the canonical defaults apply for weight,
// retrieval, importance, etc. (medium weight, balanced retrieval,
// ephemeral-by-default).
func (w *DatabaseManagerMemoryWriter) SaveMemory(content string, lineageTag string) (string, error) {
	if w.dm == nil {
		return "", errors.New("nil database manager")
	}
	tags := []string{lineageTag}
	// Promoted memories are NOT long-term by default — the operator or
	// a later `mpm kb lesson add` call decides whether to elevate.
	// Weight 5 (medium) matches handlePromoteScratchpad's choice.
	return w.dm.SaveMemoryWithExtras(
		"memories", content, "", tags, nil, nil,
		false, 5, "", "0.5", "0.5", "",
	)
}

// FindPromotionByLineage returns the canonical id of an existing
// memory tagged with the supplied lineage tag, or "" if none exists.
// Used by WorkingContextService.Promote to make retries idempotent.
//
// Implementation: scans memories whose tags JSON array contains the
// lineage value (via EXISTS + json_each). Returns the first match by
// created_at desc — a session_id should map to exactly one promotion
// in practice, but the LIMIT 1 + order keeps the response deterministic
// even if an operator manually creates a second.
func (w *DatabaseManagerMemoryWriter) FindPromotionByLineage(lineageTag string) (string, error) {
	if w.dm == nil {
		return "", errors.New("nil database manager")
	}
	if lineageTag == "" {
		return "", nil
	}
	// Use the DM's tracked query so it goes through the audit/observability
	// layer. SELECT id (no content) for speed — we just need the pointer.
	var id string
	row := w.dm.QueryRowTracked(`
		SELECT id FROM memories
		WHERE deleted_at IS NULL
		  AND EXISTS (SELECT 1 FROM json_each(tags) WHERE value = ?)
		ORDER BY created_at DESC
		LIMIT 1
	`, lineageTag)
	if err := row.Scan(&id); err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", err
	}
	return id, nil
}
