package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// IdleConsolidationWorker runs low-priority pattern detection when the
// filesystem watcher has been quiet for a configurable period.
// It uses semantic search to find pairs of high-weight LTM memories that
// are similar but unlinked, synthesizes them, and proposes new Theories
// or Lessons when patterns are detected.
type IdleConsolidationWorker struct {
	db          *DatabaseManager
	quietPeriod time.Duration // e.g. 30 minutes
	checkEvery  time.Duration // poll interval for idle check

	shutdown  chan struct{}
	startedAt time.Time
	wg        sync.WaitGroup

	logger *slog.Logger

	synthClient *SynthClient
}

// NewIdleConsolidationWorker creates an idle worker with a 30-minute quiet period.
// The worker does not start automatically — call Start() to begin.
func NewIdleConsolidationWorker(db *DatabaseManager, quietPeriod time.Duration) *IdleConsolidationWorker {
	if quietPeriod == 0 {
		quietPeriod = 30 * time.Minute
	}
	return &IdleConsolidationWorker{
		db:          db,
		quietPeriod: quietPeriod,
		checkEvery:  5 * time.Minute,
		shutdown:    make(chan struct{}),
		synthClient: NewSynthClient(),
		logger:      slog.Default(),
	}
}

// Start begins the idle consolidation loop.
func (w *IdleConsolidationWorker) Start() {
	w.startedAt = time.Now()
	w.wg.Add(1)
	go w.run()
}

// Stop initiates graceful shutdown. Blocks until the current
// consolidation cycle completes (or hits its deadline).
func (w *IdleConsolidationWorker) Stop() {
	close(w.shutdown)
	w.wg.Wait()
}

func (w *IdleConsolidationWorker) run() {
	defer w.wg.Done()

	// Wait for the initial quiet period before starting the first cycle.
	// Don't fire immediately on startup — give the system time to settle.
	ticker := time.NewTicker(w.checkEvery)
	defer ticker.Stop()

	firstCycle := time.NewTimer(2 * time.Minute)
	defer firstCycle.Stop()

	w.logger.Info("idle_worker: started",
		"quiet_period", w.quietPeriod.String(),
		"check_every", w.checkEvery.String())

	for {
		select {
		case <-w.shutdown:
			w.logger.Info("idle_worker: shutdown")
			return

		case <-firstCycle.C:
			w.doCycle()

		case <-ticker.C:
			w.doCycle()
		}
	}
}

// doCycle performs one idle consolidation pass.
func (w *IdleConsolidationWorker) doCycle() {
	if !w.isQuiet() {
		return
	}

	w.logger.Info("idle_worker: quiet period reached, starting consolidation cycle")

	pairs, err := w.findUnlinkedSimilarPairs()
	if err != nil {
		w.logger.Error("idle_worker: findUnlinkedSimilarPairs failed", "error", err.Error())
		return
	}

	if len(pairs) == 0 {
		w.logger.Info("idle_worker: no unlinked pairs found")
		return
	}

	w.logger.Info("idle_worker: found candidate pairs", "count", len(pairs))

	var proposed, skipped, failed int
	for _, pair := range pairs {
		result := w.examinePair(pair)
		switch result {
		case cycleProposed:
			proposed++
		case cycleSkipped:
			skipped++
		case cycleFailed:
			failed++
		}
	}

	w.logger.Info("idle_worker: cycle complete",
		"proposed", proposed,
		"skipped", skipped,
		"failed", failed)
}

// lastWatcherEventAt is set by the watcher event loop (cmd/mpm/handlers.go).
// This is the shared activity clock between the watcher and the idle worker.
var lastWatcherEventAt time.Time

// isQuiet returns true if no filesystem events have been seen for
// at least quietPeriod duration.
func (w *IdleConsolidationWorker) isQuiet() bool {
	if lastWatcherEventAt.IsZero() {
		return false
	}
	return time.Since(lastWatcherEventAt) >= w.quietPeriod
}

// MemoryPair represents two memories that are semantically similar but
// not linked by a shared topic.
type MemoryPair struct {
	Memory1   Memory
	Memory2   Memory
	CosineSim float32
}

// findUnlinkedSimilarPairs uses semantic search to find pairs of high-weight
// LTM memories (weight >= 10) with cosine similarity > 0.75 that share no
// common topic. These are candidates for cross-pollination.
func (w *IdleConsolidationWorker) findUnlinkedSimilarPairs() ([]MemoryPair, error) {
	rows, err := w.db.SQLDB().Query(`
		SELECT id, content, collection, tags, metadata, embedding, weight, created
		FROM memories
		WHERE deleted_at IS NULL
		  AND is_long_term = 1
		  AND weight >= 10
		  AND embedding IS NOT NULL
		  AND embedding != 'null'
		ORDER BY weight DESC, created DESC
		LIMIT 100
	`)
	if err != nil {
		return nil, fmt.Errorf("idle_worker: query failed: %w", err)
	}
	defer rows.Close()

	type rawMem struct {
		ID, Content, Collection, Tags, Metadata, Embedding string
		Weight                                            int
		Created                                           string
	}
	var memories []rawMem
	for rows.Next() {
		var m rawMem
		if err := rows.Scan(&m.ID, &m.Content, &m.Collection, &m.Tags, &m.Metadata, &m.Embedding, &m.Weight, &m.Created); err != nil {
			continue
		}
		memories = append(memories, m)
	}

	if len(memories) < 2 {
		return nil, nil
	}

	// Build topic membership map
	topicMap := make(map[string][]string)
	topicRows, err := w.db.SQLDB().Query(`
		SELECT memory_id, topic_id FROM topic_memberships
		WHERE memory_id IN (SELECT id FROM memories WHERE deleted_at IS NULL)
	`)
	if err == nil {
		defer topicRows.Close()
		for topicRows.Next() {
			var memID, topicID string
			topicRows.Scan(&memID, &topicID)
			topicMap[memID] = append(topicMap[memID], topicID)
		}
	}

	var pairs []MemoryPair
	for i := 0; i < len(memories); i++ {
		for j := i + 1; j < len(memories); j++ {
			m1, m2 := memories[i], memories[j]

			vec1 := parseEmbedding(m1.Embedding)
			vec2 := parseEmbedding(m2.Embedding)
			if len(vec1) == 0 || len(vec2) == 0 {
				continue
			}

			sim := cosineSimilarity(vec1, vec2)
			if sim < 0.75 {
				continue
			}

			// Check for topic overlap
			t1 := topicMap[m1.ID]
			t2 := topicMap[m2.ID]
			if shareTopics(t1, t2) {
				continue
			}

			pairs = append(pairs, MemoryPair{
				Memory1:   Memory{ID: m1.ID, Content: m1.Content, Tags: parseTags(m1.Tags), Weight: m1.Weight},
				Memory2:   Memory{ID: m2.ID, Content: m2.Content, Tags: parseTags(m2.Tags), Weight: m2.Weight},
				CosineSim: sim,
			})

			if len(pairs) >= 10 {
				return pairs, nil
			}
		}
	}

	return pairs, nil
}

func parseEmbedding(raw string) []float32 {
	if raw == "" || raw == "null" {
		return nil
	}
	var v []float32
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil
	}
	return v
}

func parseTags(raw string) []string {
	if raw == "" {
		return nil
	}
	var t []string
	json.Unmarshal([]byte(raw), &t)
	return t
}

// shareTopics returns true if any topic ID appears in both lists.
func shareTopics(a, b []string) bool {
	seen := make(map[string]bool)
	for _, t := range a {
		seen[t] = true
	}
	for _, t := range b {
		if seen[t] {
			return true
		}
	}
	return false
}

// cycleResult tracks what happened with a pair examination.
type cycleResult int

const (
	cycleProposed cycleResult = iota
	cycleSkipped
	cycleFailed
)

// SynthResult holds the parsed result of the idle synthesis prompt.
type SynthResult struct {
	PatternDetected     bool    `json:"pattern_detected"`
	PatternName         string  `json:"pattern_name"`
	PatternExplanation  string  `json:"pattern_explanation"`
	Confidence          float32 `json:"confidence"`
	ProposedAs          string  `json:"proposed_as"`
}

// examinePair runs a lightweight synthesis on a pair of memories to detect
// a cross-pattern. If a pattern is found, proposes a new Theory or Lesson.
func (w *IdleConsolidationWorker) examinePair(pair MemoryPair) cycleResult {
	prompt := fmt.Sprintf(`You are an epistemological pattern detector. You are given two independent facts from an AI agent's memory system. Your job is to determine if together they reveal a pattern, rule, or lesson that neither captures alone.

FACT A:
%s

FACT B:
%s

EXAMINE: Do these two facts, together, suggest a new pattern, principle, or lesson about how the agent should operate?

Respond with a JSON object only (no extra text):
{
  "pattern_detected": true or false,
  "pattern_name": "short name for the pattern (e.g. 'Premature Generalization', 'API Flapping')",
  "pattern_explanation": "2-3 sentence explanation of what these facts together reveal",
  "confidence": 0.0 to 1.0,
  "proposed_as": "theory" or "lesson"
}

If pattern_detected is false, respond with just {"pattern_detected": false}`, pair.Memory1.Content, pair.Memory2.Content)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	vendor := SynthVendor{Name: "idle-synth", Model: "MiniMax-M2.7", BaseURL: "https://api.minimax.io/anthropic/v1"}
	if key := getEnv("MINIMAX_API_KEY", ""); key != "" {
		vendor.APIKey = key
	}

	result, err := w.synthClient.SynthesizeWithVendor(ctx, vendor, prompt, []string{"idle-dream", "cross-pollination"})
	if err != nil {
		w.logger.Debug("idle_worker: synth failed for pair",
			"error", err.Error(),
			"m1_id", pair.Memory1.ID,
			"m2_id", pair.Memory2.ID)
		return cycleFailed
	}

	var synth SynthResult
	if err := json.Unmarshal([]byte(result.Content), &synth); err != nil {
		w.logger.Debug("idle_worker: parse failed",
			"error", err.Error(),
			"raw", result.Content[:min(len(result.Content), 100)])
		return cycleFailed
	}

	if synth.PatternDetected && synth.Confidence >= 0.6 {
		w.proposeTheoryOrLesson(pair, synth)
		return cycleProposed
	}

	return cycleSkipped
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// proposeTheoryOrLesson saves a proposed pattern as a pending theory or lesson.
func (w *IdleConsolidationWorker) proposeTheoryOrLesson(pair MemoryPair, result SynthResult) {
	// Dedup check
	key := fmt.Sprintf("idle:%s:%s", pair.Memory1.ID, pair.Memory2.ID)
	if !checkSynthDedup(key) {
		return
	}
	markSynthDedup(key)

	content := fmt.Sprintf(
		"[Auto-synthesized from idle consolidation]\n\nPattern: %s\nExplanation: %s\nConfidence: %.0f%%\n\nSource Fact A (id: %s): %s\n\nSource Fact B (id: %s): %s",
		result.PatternName,
		result.PatternExplanation,
		result.Confidence*100,
		pair.Memory1.ID,
		pair.Memory1.Content,
		pair.Memory2.ID,
		pair.Memory2.Content,
	)

	metadata := map[string]interface{}{
		"auto_synthesized":  true,
		"idle_dream":        true,
		"source_pair":       []string{pair.Memory1.ID, pair.Memory2.ID},
		"cosine_similarity": pair.CosineSim,
		"pattern_name":      result.PatternName,
		"confidence":        result.Confidence,
		"proposed_as":       result.ProposedAs,
		"status":            "pending",
	}

	tags := []string{"theories", "auto-synthesized", "idle-dream"}
	if result.ProposedAs == "lesson" {
		tags = []string{"lessons", "auto-synthesized", "idle-dream"}
	}

	embedding := EmbedText(content)

	id, err := w.db.SaveMemory("memories", content, "", tags, metadata, embedding, true, 8)
	if err != nil {
		w.logger.Error("idle_worker: failed to save proposed pattern",
			"error", err.Error(),
			"pattern", result.PatternName)
		return
	}

	w.logger.Info("idle_worker: proposed new pattern",
		"id", id[:12],
		"pattern", result.PatternName,
		"confidence", result.Confidence,
		"type", result.ProposedAs)
}

// checkSynthDedup and markSynthDedup are the local wrappers around the
// package-level synthSeen map in synthesize.go.
func checkSynthDedup(key string) bool {
	synthSeenMu.Lock()
	defer synthSeenMu.Unlock()
	_, exists := synthSeen[key]
	return exists
}

func markSynthDedup(key string) {
	synthSeenMu.Lock()
	defer synthSeenMu.Unlock()
	synthSeen[key] = time.Now()
}