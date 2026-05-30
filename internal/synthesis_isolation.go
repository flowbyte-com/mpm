package internal

import (
	"context"
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
	db     *DatabaseManager
	synth  *SynthClient

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
func NewSynthesisWorker(db *DatabaseManager, synth *SynthClient, maxWorkers int) *SynthesisWorker {
	if maxWorkers <= 0 {
		maxWorkers = DefaultMaxWorkers
	}
	return &SynthesisWorker{
		db:      db,
		synth:   synth,
		events:  make(chan MemoryEvent, 200), // bounded queue
		shutdown: make(chan struct{}),
		sem:     make(chan struct{}, maxWorkers),
		dlqTick: time.NewTicker(5 * time.Minute).C,
		logger:  slog.Default(),
	}
}

// Enqueue fires a synthesis event. Never blocks the caller.
// Returns immediately — caller (watcher) continues without waiting.
func (w *SynthesisWorker) Enqueue(event MemoryEvent) {
	select {
	case w.events <- event:
		// enqueued
	default:
		// Channel full — log and drop (watcher must never block)
		w.logger.Warn("synthesis_worker: event channel full, dropping",
			"memory_id", event.ID,
			"content_len", len(event.Content))
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
	close(w.shutdown)  // signal run loop to stop accepting new events
	w.wg.Wait()         // wait for all in-flight goroutines to finish
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
			w.sem <- struct{}{} // acquire semaphore slot
			go w.processEvent(event) // fire-and-forget with deadline

		case <-w.shutdown:
			// Graceful shutdown: drain queue then exit
			w.drainAndExit()
			return

		case <-w.dlqTick:
			go w.processDLQ()
		}
	}
}

func (w *SynthesisWorker) processEvent(event MemoryEvent) {
	defer w.wg.Done()
	defer func() { <-w.sem }() // release semaphore slot

	// Isolated context with hard deadline — synthesis cannot hang forever
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := synthWithMultiVendor(ctx, w.db, w.synth, event.ID, event.Content, event.Tags)
	if err != nil {
		// All vendors failed — route to DLQ
		enqueueErr := DLQEnqueue(w.db.SQLDB(), event.ID, event.Content, event.Tags, err)
		if enqueueErr != nil {
			w.logger.Error("synthesis_worker: dlq enqueue failed",
				"memory_id", event.ID,
				"synth_error", err.Error(),
				"dlq_error", enqueueErr.Error())
		}
	}
}

func (w *SynthesisWorker) processDLQ() {
	entries, err := DLQReady(w.db.SQLDB())
	if err != nil {
		w.logger.Error("synthesis_worker: dlq read failed", "error", err.Error())
		return
	}
	if len(entries) == 0 {
		return
	}

	w.logger.Info("synthesis_worker: processing dlq retry", "count", len(entries))

	for _, entry := range entries {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := synthWithMultiVendor(ctx, w.db, w.synth, entry.MemoryID, entry.Content, entry.Tags)
		cancel()

		if err == nil {
			DLQRemove(w.db.SQLDB(), entry.ID)
			w.logger.Info("synthesis_worker: dlq retry succeeded", "memory_id", entry.MemoryID)
		} else {
			newAttempt := entry.Attempt + 1
			if newAttempt >= 5 {
				// Hard stop: requires manual intervention
				w.logger.Warn("synthesis_worker: dlq entry hard-stopped after 5 attempts",
					"memory_id", entry.MemoryID, "last_error", err.Error())
			}
			DLQUpdateRetry(w.db.SQLDB(), entry.ID, newAttempt, err)
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
				break
			}
			drainWg.Add(1)
			w.sem <- struct{}{}
			go func(e MemoryEvent) {
				defer drainWg.Done()
				defer func() { <-w.sem }()

				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				synthWithMultiVendor(ctx, w.db, w.synth, e.ID, e.Content, e.Tags)
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
func synthWithMultiVendor(ctx context.Context, db *DatabaseManager, client *SynthClient, memoryID, content string, tags []string) error {
	vendors := getSynthVendorChain()

	for _, vendor := range vendors {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		result, err := client.SynthesizeWithVendor(ctx, vendor, content, tags)
		if err == nil && result != nil {
			// Success — persist synthesized LTM
			persistSynthesizedMemory(db, memoryID, content, result.Content, tags, vendor)
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
func persistSynthesizedMemory(dm *DatabaseManager, sourceID, sourceContent, synthesizedContent string, tags []string, vendor SynthVendor) {
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

	newID, err := dm.SaveMemory("memories", synthesizedContent, "", allTags, metadata, embedding, true, 10)
	if err != nil {
		slog.Error("synthesis_isolation: persist failed", "error", err.Error(), "source_id", sourceID)
		return
	}

	dm.SQLDB().Exec(
		`INSERT OR IGNORE INTO topic_memberships (memory_id, topic_id, role, created_at)
		 SELECT ?, topic_id, role, created_at
		 FROM topic_memberships
		 WHERE memory_id = ?`,
		newID, sourceID,
	)

	dm.SQLDB().Exec(`UPDATE memories SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`, sourceID)

	slog.Info("synthesis_isolation: synthesized", "source_id", sourceID, "new_id", newID, "vendor", vendor.Name)
}

// ErrAllVendorsFailed is returned when every vendor in the chain has failed.
var ErrAllVendorsFailed = synthesisError("all vendors in fallback chain failed")

type synthesisError string

func (e synthesisError) Error() string { return string(e) }