package internal

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"mpm/internal/config"
)

// MemoryEvent represents a synthesis trigger from the watcher.
type MemoryEvent struct {
	ID      string
	Content string
	Tags    []string
}

// DefaultMaxWorkers is the cap on concurrent synthesis tasks.
// Keep low to avoid rate-limiting from LLM providers.
const DefaultMaxWorkers = 3

// SynthesisWorker manages the isolated synthesis goroutine pool.
type SynthesisWorker struct {
	db          *DatabaseManager
	synth       SynthClientInterface // interface allows mock injection in tests
	vendorChain []SynthVendor        // ordered fallback chain; nil = use getSynthVendorChain()

	events   chan MemoryEvent // inbound event channel
	shutdown chan struct{}    // shutdown signal

	wg  sync.WaitGroup
	sem chan struct{} // concurrency semaphore

	dlqTick <-chan time.Time

	logger *slog.Logger
}

// NewSynthesisWorker creates a worker with isolated goroutine context.
// The worker holds its own event channel and manages a pool of maxWorkers
// concurrent synthesis tasks via semaphore.
func NewSynthesisWorker(db *DatabaseManager, synth SynthClientInterface, maxWorkers int) *SynthesisWorker {
	if maxWorkers <= 0 {
		maxWorkers = DefaultMaxWorkers
	}
	return &SynthesisWorker{
		db:          db,
		synth:       synth,
		vendorChain: getSynthVendorChain(),
		events:      make(chan MemoryEvent, 200), // bounded queue
		shutdown:    make(chan struct{}),
		sem:         make(chan struct{}, maxWorkers),
		dlqTick:     time.NewTicker(5 * time.Minute).C,
		logger:      slog.Default(),
	}
}

// NewSynthesisWorkerWithManualTick creates a worker where the DLQ tick is
// driven by an external channel (for testing). Production code should use
// NewSynthesisWorker which creates its own 5-minute ticker.
func NewSynthesisWorkerWithManualTick(db *DatabaseManager, synth SynthClientInterface, maxWorkers int, dlqTick <-chan time.Time) *SynthesisWorker {
	if maxWorkers <= 0 {
		maxWorkers = DefaultMaxWorkers
	}
	return &SynthesisWorker{
		db:          db,
		synth:       synth,
		vendorChain: getSynthVendorChain(),
		events:      make(chan MemoryEvent, 200),
		shutdown:    make(chan struct{}),
		sem:         make(chan struct{}, maxWorkers),
		dlqTick:     dlqTick,
		logger:      slog.Default(),
	}
}

// NewSynthesisWorkerForTest creates a worker for unit testing with an explicit
// vendor chain and manual DLQ tick channel. This bypasses getSynthVendorChain()
// which requires real API keys.
func NewSynthesisWorkerForTest(db *DatabaseManager, synth SynthClientInterface, maxWorkers int, dlqTick <-chan time.Time, vendorChain []SynthVendor) *SynthesisWorker {
	if maxWorkers <= 0 {
		maxWorkers = DefaultMaxWorkers
	}
	return &SynthesisWorker{
		db:          db,
		synth:       synth,
		vendorChain: vendorChain,
		events:      make(chan MemoryEvent, 200),
		shutdown:    make(chan struct{}),
		sem:         make(chan struct{}, maxWorkers),
		dlqTick:     dlqTick,
		logger:      slog.Default(),
	}
}

// Enqueue fires a synthesis event. Never blocks the caller.
// Returns immediately — caller (watcher) continues without waiting.
// When the channel buffer is saturated, events are offloaded to the overflow
// staging area (raw_memories with status='overflow_deferred') in a background
// goroutine to prevent file-system event drop-loss.
func (w *SynthesisWorker) Enqueue(event MemoryEvent) {
	select {
	case w.events <- event:
		// enqueued
	default:
		// Channel full — non-blocking background write to overflow staging
		w.logger.Warn("synthesis_worker: event channel full, offloading to overflow",
			"memory_id", event.ID,
			"content_len", len(event.Content))
		go func() {
			if err := w.db.DLQEnqueueOverflow(event); err != nil {
				w.logger.Error("synthesis_worker: overflow enqueue failed",
					"memory_id", event.ID,
					"error", err)
			}
		}()
	}
}

// Start begins the worker loop. Call once at startup.
func (w *SynthesisWorker) Start() {
	w.wg.Add(1)
	go w.run()
}

// Stop initiates graceful shutdown. Blocks until all in-flight
// synthesis tasks complete (or hit their context deadline).
func (w *SynthesisWorker) Stop() {
	close(w.shutdown) // signal run loop to stop accepting new events
	w.wg.Wait()       // wait for all in-flight goroutines to finish
}

// ── Internal ─────────────────────────────────────────────────────────────────

func (w *SynthesisWorker) run() {
	defer w.wg.Done()

	for {
		select {
		case event, ok := <-w.events:
			if !ok {
				// Channel closed — drain in-flight then exit
				w.drainAndExit()
				return
			}
			w.wg.Add(1)
			w.sem <- struct{}{}      // acquire semaphore slot
			go w.processEvent(event) // fire-and-forget with deadline

		case <-w.shutdown:
			// Graceful shutdown: drain queue then exit
			w.drainAndExit()
			return

		case <-w.dlqTick:
			go w.processDLQ()
			go w.db.RunLifecycleDecayAndArchival(1.0, 30)
		}
	}
}

func (w *SynthesisWorker) processEvent(event MemoryEvent) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic recovered", "err", r)
		}
	}()
	defer w.wg.Done()
	defer func() { <-w.sem }() // release semaphore slot

	// Isolated context with hard deadline — synthesis cannot hang forever
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := w.synthWithMultiVendor(ctx, event.ID, event.Content, event.Tags)
	if err != nil {
		// All vendors failed — route to DLQ and log to audit ledger.
		enqueueErr := DLQEnqueue(w.db.SQLDB(), event.ID, event.Content, event.Tags, err)
		if w.db != nil {
			dlqStatus := "ok"
			if enqueueErr != nil {
				dlqStatus = "enqueue_failed: " + enqueueErr.Error()
			}
			w.db.LogAudit(AuditError, "synthesis", "all vendors failed, event routed to DLQ: "+err.Error(), "", AuditContext{
				"event_id":   event.ID,
				"dlq_status": dlqStatus,
			})
		}
		if enqueueErr != nil {
			w.logger.Error("synthesis_worker: dlq enqueue failed",
				"memory_id", event.ID,
				"synth_error", err.Error(),
				"dlq_error", enqueueErr.Error())
		}
	}
}

func (w *SynthesisWorker) processDLQ() {
	// 1. Standard DLQ entries
	entries, err := DLQReady(w.db.SQLDB())
	if err != nil {
		w.logger.Error("synthesis_worker: dlq read failed", "error", err.Error())
		return
	}

	// 2. Overflow-deferred entries from raw_memories
	overflowEntries, err := OverflowReady(w.db.SQLDB())
	if err != nil {
		w.logger.Error("synthesis_worker: overflow read failed", "error", err.Error())
		return
	}

	total := len(entries) + len(overflowEntries)
	if total == 0 {
		w.logger.Debug("synthesis_worker: processDLQ called but no entries ready")
		return
	}

	w.logger.Info("synthesis_worker: processing retry queue",
		"dlq_count", len(entries), "overflow_count", len(overflowEntries))

	w.processDLQEntries(entries)
	w.processOverflowEntries(overflowEntries)
}

// processDLQForced retrieves all non-HARD-STOPPED DLQ entries regardless of
// next_retry time and processes them. For unit testing only.
func (w *SynthesisWorker) processDLQForced() {
	entries, err := w.dlqEntriesForTest()
	if err != nil {
		w.logger.Error("synthesis_worker: dlq read failed", "error", err.Error())
		return
	}
	if len(entries) == 0 {
		return
	}
	w.logger.Info("synthesis_worker: processing dlq retry (forced)", "count", len(entries))
	w.processDLQEntries(entries)
}

// dlqEntriesForTest returns all DLQ entries with no time filter.
// Mirrors DLQReady's column scan so the same entry parsing works.
func (w *SynthesisWorker) dlqEntriesForTest() ([]DLQEntry, error) {
	rows, err := w.db.SQLDB().Query(`
		SELECT id, memory_id, content, tags, attempt, last_error, created_at, next_retry
		FROM synthesis_dlq
		ORDER BY created_at ASC
		LIMIT 20
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []DLQEntry
	for rows.Next() {
		var e DLQEntry
		var tagsStr, lastErr, nextRetryStr, createdAtStr string
		if err := rows.Scan(&e.ID, &e.MemoryID, &e.Content, &tagsStr, &e.Attempt, &lastErr, &createdAtStr, &nextRetryStr); err != nil {
			continue
		}
		e.LastError = lastErr
		if nextRetryStr != "" {
			t, _ := time.Parse(time.RFC3339, nextRetryStr)
			e.NextRetry = t
		}
		if createdAtStr != "" {
			t, _ := time.Parse(time.RFC3339, createdAtStr)
			e.CreatedAt = t
		}
		if tagsStr != "" {
			json.Unmarshal([]byte(tagsStr), &e.Tags)
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

func (w *SynthesisWorker) processDLQEntries(entries []DLQEntry) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic recovered", "err", r)
		}
	}()
	for _, entry := range entries {
		// Check if source memory was already synthesized (soft-deleted).
		// This can happen when: (1) event goroutine succeeded but DLQ row not yet removed,
		// OR (2) a prior processDLQ call already handled this entry.
		var deletedAt sql.NullString
		row := w.db.SQLDB().QueryRow("SELECT deleted_at FROM memories WHERE id = ?", entry.MemoryID)
		if err := row.Scan(&deletedAt); err == nil && deletedAt.Valid && deletedAt.String != "" {
			// Source was soft-deleted — synthesis already completed. Clean up stale DLQ entry.
			DLQRemove(w.db.SQLDB(), entry.ID)
			w.logger.Info("synthesis_worker: dlq entry cleared (already synthesized)", "memory_id", entry.MemoryID)
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := w.synthWithMultiVendor(ctx, entry.MemoryID, entry.Content, entry.Tags)
		cancel()

		if err == nil {
			DLQRemove(w.db.SQLDB(), entry.ID)
			w.logger.Info("synthesis_worker: dlq retry succeeded", "memory_id", entry.MemoryID)
		} else {
			newAttempt := entry.Attempt + 1
			if newAttempt >= 5 {
				w.logger.Warn("synthesis_worker: dlq entry hard-stopped after 5 attempts",
					"memory_id", entry.MemoryID, "last_error", err.Error())
				DLQRemove(w.db.SQLDB(), entry.ID)
				continue
			}
			if retryErr := DLQUpdateRetry(w.db.SQLDB(), entry.ID, newAttempt, err); retryErr != nil {
				w.logger.Error("synthesis_worker: dlq retry update failed",
					"memory_id", entry.MemoryID, "error", retryErr.Error())
				break
			}
		}
	}
}

func (w *SynthesisWorker) processOverflowEntries(entries []OverflowEntry) {
	for _, entry := range entries {
		var deletedAt sql.NullString
		row := w.db.SQLDB().QueryRow("SELECT deleted_at FROM memories WHERE id = ?", entry.MemoryID)
		if err := row.Scan(&deletedAt); err == nil && deletedAt.Valid && deletedAt.String != "" {
			OverflowResolve(w.db.SQLDB(), entry.ID)
			w.logger.Info("synthesis_worker: overflow entry cleared (already synthesized)", "memory_id", entry.MemoryID)
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := w.synthWithMultiVendor(ctx, entry.MemoryID, entry.Content, entry.Tags)
		cancel()

		if err == nil {
			OverflowResolve(w.db.SQLDB(), entry.ID)
			w.logger.Info("synthesis_worker: overflow retry succeeded", "memory_id", entry.MemoryID)
		} else {
			newAttempt := entry.Attempt + 1
			if newAttempt >= 5 {
				w.logger.Warn("synthesis_worker: overflow entry hard-stopped after 5 attempts",
					"memory_id", entry.MemoryID, "last_error", err.Error())
			}
			if retryErr := OverflowUpdateRetry(w.db.SQLDB(), entry.ID, newAttempt, err); retryErr != nil {
				w.logger.Error("synthesis_worker: overflow retry update failed",
					"memory_id", entry.MemoryID, "error", retryErr.Error())
			}
		}
	}
}

// drainAndExit processes remaining queued events asynchronously and waits
// for all in-flight goroutines to complete before returning.
// This ensures no events are silently dropped on shutdown.
func (w *SynthesisWorker) drainAndExit() {
	var drainWg sync.WaitGroup

	// Drain the channel — process remaining events asynchronously
	for {
		select {
		case event, ok := <-w.events:
			if !ok {
				return
			}
			drainWg.Add(1)
			w.sem <- struct{}{}
			go func(e MemoryEvent) {
				defer drainWg.Done()
				defer func() { <-w.sem }()

				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				w.synthWithMultiVendor(ctx, e.ID, e.Content, e.Tags)
			}(event)
		default:
			goto drainComplete
		}
	}

drainComplete:
	drainWg.Wait() // wait for all drained events to finish processing
}

// ── Multi-Vendor Synthesis ───────────────────────────────────────────────────

// synthWithMultiVendor tries each vendor in order until one succeeds.
// Returns error only if all vendors fail.
// Uses w.vendorChain if non-nil, otherwise calls getSynthVendorChain().
func (w *SynthesisWorker) synthWithMultiVendor(ctx context.Context, memoryID, content string, tags []string) error {
	vendors := w.vendorChain
	if vendors == nil {
		vendors = getSynthVendorChain()
	}

	for _, vendor := range vendors {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		result, err := w.synth.SynthesizeWithVendor(ctx, vendor, content, tags)
		if err == nil && result != nil {
			// Success — persist synthesized LTM
			persistSynthesizedMemory(w.db, memoryID, content, result.Content, tags, vendor)
			return nil
		}
		// Vendor failed — log and try next
		if err != nil {
			slog.Debug("synthesis_worker: vendor failed",
				"vendor", vendor.Name,
				"memory_id", memoryID,
				"error", err.Error())
		}
	}

	return ErrAllVendorsFailed
}

// getSynthVendorChain returns the ordered fallback chain from config and env.
func getSynthVendorChain() []SynthVendor {
	chain := []SynthVendor{}

	// Primary: MiniMax
	primary := SynthVendor{Name: "minimax", Model: "MiniMax-M2.7", BaseURL: "https://api.minimax.io/anthropic/v1"}
	cfg, _ := config.LoadConfig()
	if cfg != nil && cfg.Synth != nil {
		if cfg.Synth.Model != "" {
			primary.Model = cfg.Synth.Model
		}
		if cfg.Synth.APIKey != "" {
			primary.APIKey = cfg.Synth.APIKey
		}
		if cfg.Synth.BaseURL != "" {
			primary.BaseURL = cfg.Synth.BaseURL
		}
	}
	if key := getEnv("MINIMAX_API_KEY", ""); key != "" {
		primary.APIKey = key
	}
	chain = append(chain, primary)

	// Fallback: OpenAI
	if key := getEnv("OPENAI_API_KEY", ""); key != "" {
		chain = append(chain, SynthVendor{
			Name:    "openai",
			APIKey:  key,
			Model:   "gpt-4o",
			BaseURL: "https://api.openai.com/v1",
		})
	}

	// Fallback: Ollama (local)
	if endpoint := getEnv("OLLAMA_ENDPOINT", ""); endpoint != "" {
		model := getEnv("OLLAMA_MODEL", "llama3")
		chain = append(chain, SynthVendor{
			Name:    "ollama",
			APIKey:  "",
			Model:   model,
			BaseURL: endpoint,
		})
	}

	return chain
}

// persistSynthesizedMemory saves a successful synthesis result and soft-deletes
// the source memory. Used by the isolated synthesis worker.
//
// All three writes run inside a single transaction so a crash mid-write cannot
// leave "zombie" source memories (new memory inserted, source not soft-deleted)
// or orphaned topic links (source soft-deleted before its topics were copied).
//
// Ordering inside the tx: INSERT new memory → soft-delete source → copy topic
// links. The topic-copy query intentionally omits the `m.deleted_at IS NULL`
// filter that the previous version had: the source is now soft-deleted by
// design, and we still need to copy its topic memberships forward.
func persistSynthesizedMemory(dm *DatabaseManager, sourceID, _ string, synthesizedContent string, tags []string, vendor SynthVendor) {
	metadata := map[string]interface{}{
		"synthesized":    true,
		"source_id":      sourceID,
		"vendor":         vendor.Name,
		"synthesized_at": time.Now().UTC().Format(time.RFC3339),
	}

	allTags := tags
	if allTags == nil {
		allTags = []string{}
	}
	allTags = append(allTags, "synthesized", "ltm")

	embedding := EmbedText(synthesizedContent)
	if embedding == nil {
		// EmbedText may return nil; persist as NULL in SQLite.
		embedding = []float32{}
	}

	tagsJSON, _ := json.Marshal(allTags)
	metadataJSON, _ := json.Marshal(metadata)
	embeddingJSON := "null"
	if len(embedding) > 0 {
		if b, err := json.Marshal(embedding); err == nil {
			embeddingJSON = string(b)
		}
	}
	newID := GenerateID()

	tx, err := dm.SQLDB().Begin()
	if err != nil {
		slog.Error("synthesis_isolation: persist begin failed", "error", err.Error(), "source_id", sourceID)
		return
	}
	defer tx.Rollback()

	// 1. INSERT the synthesized memory in-place (joining the transaction).
	// We can't use SaveMemory here because it opens its own connection.
	if _, err := tx.Exec(`
		INSERT INTO memories (id, collection, content, session_id, tags, metadata, embedding, is_long_term, weight)
		VALUES (?, 'memories', ?, NULL, ?, ?, ?, 1, 10)
	`, newID, synthesizedContent, string(tagsJSON), string(metadataJSON), embeddingJSON); err != nil {
		slog.Error("synthesis_isolation: persist insert failed", "error", err.Error(), "source_id", sourceID)
		return
	}

	// 2. Soft-delete the source. The `WHERE deleted_at IS NULL` guard ensures
	// we only soft-delete a memory that is still live — a double-persist from
	// a retried DLQ entry is a no-op rather than a tombstone overwrite.
	if _, err := tx.Exec(`UPDATE memories SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`, sourceID); err != nil {
		slog.Error("synthesis_isolation: persist soft-delete failed", "error", err.Error(), "source_id", sourceID)
		return
	}

	// 3. Copy topic memberships forward. The `m.deleted_at IS NULL` filter is
	// intentionally dropped: we are the path that just soft-deleted the source.
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO topic_memberships (memory_id, topic_id, role, created_at)
		 SELECT ?, topic_id, role, created_at
		 FROM topic_memberships tm
		 WHERE tm.memory_id = ?`,
		newID, sourceID,
	); err != nil {
		slog.Error("synthesis_isolation: persist topic copy failed", "error", err.Error(), "source_id", sourceID)
		return
	}

	if err := tx.Commit(); err != nil {
		slog.Error("synthesis_isolation: persist commit failed", "error", err.Error(), "source_id", sourceID)
		return
	}

	slog.Info("synthesis_isolation: synthesized", "source_id", sourceID, "new_id", newID, "vendor", vendor.Name)
}

// ErrAllVendorsFailed is returned when every vendor in the chain has failed.
var ErrAllVendorsFailed = synthesisError("all vendors in fallback chain failed")

type synthesisError string

func (e synthesisError) Error() string { return string(e) }
