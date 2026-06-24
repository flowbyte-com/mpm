package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"mpm/internal/config"
	"mpm/internal/synth"
)

// IdleConsolidationWorker runs low-priority pattern detection when the
// filesystem watcher has been quiet for a configurable period.
// It uses stochastic sampling to find pairs of high-weight LTM memories that
// are similar but unlinked, synthesizes them, and proposes new Theories
// or Lessons when patterns are detected.
//
// CPU behavior: O(1) per cycle regardless of corpus size.
// Each cycle samples 20 seeds × 10 candidates = 200 comparisons max.
// The combinatorial space is slowly covered over weeks of idle operation.
type IdleConsolidationWorker struct {
	db          *DatabaseManager
	quietPeriod time.Duration // e.g. 30 minutes
	checkEvery  time.Duration // poll interval for idle check
	driftCheckEvery time.Duration // poll interval for concept drift detection

	shutdown  chan struct{}
	startedAt time.Time
	wg        sync.WaitGroup

	logger *slog.Logger

	synthClient *synth.SynthClient
}

// NewIdleConsolidationWorker creates an idle worker with a 30-minute quiet period.
// The worker does not start automatically — call Start() to begin.
func NewIdleConsolidationWorker(db *DatabaseManager, quietPeriod time.Duration) *IdleConsolidationWorker {
	if quietPeriod == 0 {
		quietPeriod = 30 * time.Minute
	}
	return &IdleConsolidationWorker{
		db:             db,
		quietPeriod:    quietPeriod,
		checkEvery:     5 * time.Minute,
		driftCheckEvery: 6 * time.Hour,
		shutdown:       make(chan struct{}),
		synthClient:    synth.NewSynthClient(),
		logger:         slog.Default(),
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

	ticker := time.NewTicker(w.checkEvery)
	defer ticker.Stop()

	firstCycle := time.NewTimer(2 * time.Minute)
	defer firstCycle.Stop()

	driftTicker := time.NewTicker(w.driftCheckEvery)
	defer driftTicker.Stop()

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

		case <-driftTicker.C:
			w.DetectConceptDriftCycle()
		}
	}
}

// doCycle performs one idle consolidation pass.
func (w *IdleConsolidationWorker) doCycle() {
	if !w.isQuiet() {
		return
	}

	w.logger.Info("idle_worker: quiet period reached, starting consolidation cycle")

	// Confidence decay recompute. Runs every cycle, independent of synthesis.
	// The trigger chain handles confidence recompute on evidence insert/delete,
	// but cannot do time-based decay — that's what this cycle is for.
	if decayed, err := w.ConfidenceDecayCycle(); err != nil {
		w.logger.Warn("idle_worker: confidence decay cycle failed", "error", err.Error())
	} else if decayed > 0 {
		w.logger.Info("idle_worker: confidence decay cycle complete", "decayed", decayed)
	}

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

// ConfidenceDecayCycle walks artifacts whose last positive evidence is older
// than the collection's decay half-life and recomputes their confidence
// with trigger=decay_tick. Returns the number of artifacts recomputed.
//
// The half-life is collection-specific: memories ~69 days, theories ~35 days,
// lessons ~231 days, decisions ~693 days. We use a "since last positive
// evidence" anchor, not "since created_at", so a freshly-reinforced
// artifact is not penalized.
func (w *IdleConsolidationWorker) ConfidenceDecayCycle() (int, error) {
	// Find candidate artifacts: rows whose latest positive evidence (or
	// creation, if no positive evidence exists) is older than the decay
	// half-life for the collection.
	//
	// Artifact-type mapping is critical: the `evidence` table is keyed on
	// (artifact_id, artifact_type). Hardcoding 'memory' for every row in
	// the `memories` table would silently strip evidence from theories
	// and decisions — loadEvidenceForRecompute would see zero evidence
	// rows, lastPositiveAt would anchor to created_at, and the recompute
	// would land at the initial value with full decay from creation. The
	// epistemic violation: a theory with 0.85-strength reproduction
	// evidence would have its confidence reset on every decay tick. Use
	// the `collection` discriminator to pick the right artifact_type.
	rows, err := w.db.QueryTracked(`
		SELECT a.artifact_id, a.artifact_type, a.last_positive_at
		FROM (
			SELECT
				m.id AS artifact_id,
				CASE m.collection
					WHEN 'theories'  THEN 'theory'
					WHEN 'decisions' THEN 'decision'
					ELSE 'memory'
				END AS artifact_type,
				CAST(COALESCE(
					(SELECT MAX(e.created_at) FROM evidence e
					 WHERE e.artifact_id = m.id
					   AND e.artifact_type = CASE m.collection
					       WHEN 'theories'  THEN 'theory'
					       WHEN 'decisions' THEN 'decision'
					       ELSE 'memory'
					   END
					   AND e.strength > 0),
					strftime('%s', m.created_at)
				) AS INTEGER) AS last_positive_at
			FROM memories m
			UNION ALL
			SELECT
				l.id,
				'lesson',
				CAST(COALESCE(
					(SELECT MAX(e.created_at) FROM evidence e WHERE e.artifact_id = l.id AND e.artifact_type = 'lesson' AND e.strength > 0),
					strftime('%s', l.created)
				) AS INTEGER)
			FROM lessons l
		) a
		WHERE a.last_positive_at < CAST(strftime('%s', 'now', '-1 day') AS INTEGER)
	`)
	if err != nil {
		return 0, fmt.Errorf("query stale artifacts: %w", err)
	}
	defer rows.Close()

	type candidate struct {
		artifactID     string
		artifactType   string
		lastPositiveAt int64
	}
	var cands []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.artifactID, &c.artifactType, &c.lastPositiveAt); err != nil {
			return 0, err
		}
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	// Schedule the recompute. Each candidate runs in its own transaction so
	// a partial write (confidence updated but history row missing, or vice
	// versa) can't leave the artifact and its audit trail desynced.
	count := 0
	for _, c := range cands {
		// The per-collection half-life check happens inside computeConfidence;
		// we just need to call RecomputeConfidence for the candidate. The
		// actual "is this stale enough" gating is the WHERE clause above
		// (older than 1 day). For finer-grained gating, extend the SQL.
		if err := w.db.WithTx(func(node DBNode) error {
			return RecomputeConfidence(node, c.artifactID, c.artifactType, RecomputeReasonDecayTick)
		}); err != nil {
			w.logger.Warn("idle_worker: recompute failed",
				"artifact_id", c.artifactID, "error", err)
			continue
		}
		count++
	}
	return count, nil
}

// lastWatcherEventAtNano is the atomic activity clock written by the watcher
// goroutine and read by the idle worker. Storing UnixNano avoids data races
// on time.Time without adding mutex overhead to the hot watcher path.
var lastWatcherEventAtNano atomic.Int64

// driftSeen maps artifact_id → time when drift was last raised.
// Kept in-process only; concept drift is low-frequency enough that
// a restart-tolerating SQLite-native dedup runs alongside as a safety net.
var driftSeen     = map[string]time.Time{}
var driftSeenMu   sync.Mutex

// UpdateLastWatcherEvent is called by the watcher event loop on every fsnotify event.
func UpdateLastWatcherEvent() {
	lastWatcherEventAtNano.Store(time.Now().UnixNano())
}

// isQuiet returns true if no filesystem events have been seen for
// at least quietPeriod duration. Atomic read — race-free.
func (w *IdleConsolidationWorker) isQuiet() bool {
	lastNano := lastWatcherEventAtNano.Load()
	if lastNano == 0 {
		return false
	}
	return time.Since(time.Unix(0, lastNano)) >= w.quietPeriod
}

// MemoryPair represents two memories that are semantically similar but
// not linked by a shared topic.
type MemoryPair struct {
	Memory1   Memory
	Memory2   Memory
	CosineSim float32
}

// findUnlinkedSimilarPairs uses stochastic sampling to find pairs of high-weight
// LTM memories (weight >= 10) with cosine similarity > 0.75 that share no
// common topic. Bounded to O(1) comparisons per cycle — 20 seeds × 10 candidates = 200 max.
// This caps CPU at a fixed ceiling regardless of corpus size.
func (w *IdleConsolidationWorker) findUnlinkedSimilarPairs() ([]MemoryPair, error) {
	const seedLimit = 20      // random LTM seeds per cycle
	const candidateLimit = 10 // random candidates per seed
	const similarityThreshold = 0.75
	const pairCap = 10

	// Sample seeds with ORDER BY RANDOM()
	seeds, err := w.fetchRandomMemories(seedLimit)
	if err != nil || len(seeds) < 2 {
		return nil, err
	}

	// Fetch candidates (separate random sample)
	candidates, err := w.fetchRandomMemories(candidateLimit)
	if err != nil || len(candidates) == 0 {
		return nil, err
	}

	// Build topic map for seeds only (reduce query surface)
	seedIDs := make([]string, len(seeds))
	for i, s := range seeds {
		seedIDs[i] = s.ID
	}
	topicMap := w.fetchTopicMap(seedIDs)

	var pairs []MemoryPair
	for _, seed := range seeds {
		vecSeed := parseEmbedding(seed.Embedding)
		if len(vecSeed) == 0 {
			continue
		}

		for _, cand := range candidates {
			if cand.ID == seed.ID {
				continue
			}

			vecCand := parseEmbedding(cand.Embedding)
			if len(vecCand) == 0 {
				continue
			}

			sim := cosineSimilarity(vecSeed, vecCand)
			if sim < similarityThreshold {
				continue
			}

			// Check topic overlap
			t1 := topicMap[seed.ID]
			t2 := topicMap[cand.ID]
			if shareTopics(t1, t2) {
				continue
			}

			pairs = append(pairs, MemoryPair{
				Memory1:   Memory{ID: seed.ID, Content: seed.Content, Tags: parseTags(seed.Tags), Weight: seed.Weight},
				Memory2:   Memory{ID: cand.ID, Content: cand.Content, Tags: parseTags(cand.Tags), Weight: cand.Weight},
				CosineSim: sim,
			})

			if len(pairs) >= pairCap {
				return pairs, nil
			}
		}
	}

	return pairs, nil
}

type rawMem struct {
	ID, Content, Collection, Tags, Metadata, Embedding string
	Weight                                             int
	Created                                            string
}

// fetchRandomMemories returns a random sample of high-weight LTMs.
func (w *IdleConsolidationWorker) fetchRandomMemories(limit int) ([]rawMem, error) {
	rows, err := w.db.SQLDB().Query(`
		SELECT id, content, collection, tags, metadata, embedding, weight, created
		FROM memories
		WHERE deleted_at IS NULL
		  AND is_long_term = 1
		  AND weight >= 10
		  AND embedding IS NOT NULL
		  AND embedding != 'null'
		ORDER BY RANDOM()
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []rawMem
	for rows.Next() {
		var m rawMem
		if err := rows.Scan(&m.ID, &m.Content, &m.Collection, &m.Tags, &m.Metadata, &m.Embedding, &m.Weight, &m.Created); err != nil {
			continue
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

// fetchTopicMap returns topic IDs for each memory ID in the input slice.
func (w *IdleConsolidationWorker) fetchTopicMap(memIDs []string) map[string][]string {
	result := make(map[string][]string)
	if len(memIDs) == 0 {
		return result
	}

	// Build ? placeholders for IN clause
	placeholders := make([]byte, 0, len(memIDs)*2)
	args := make([]interface{}, len(memIDs))
	for i, id := range memIDs {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '?')
		args[i] = id
	}

	query := fmt.Sprintf(`SELECT memory_id, topic_id FROM topic_memberships WHERE memory_id IN (%s)`, string(placeholders))
	rows, err := w.db.SQLDB().Query(query, args...)
	if err != nil {
		return result
	}
	defer rows.Close()

	for rows.Next() {
		var memID, topicID string
		rows.Scan(&memID, &topicID)
		result[memID] = append(result[memID], topicID)
	}
	return result
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

// cycleResult tracks what happened with a pair examination.
type cycleResult int

const (
	cycleProposed cycleResult = iota
	cycleSkipped
	cycleFailed
)

// SynthResult holds the parsed result of the idle synthesis prompt.
type SynthResult struct {
	PatternDetected    bool    `json:"pattern_detected"`
	PatternName        string  `json:"pattern_name"`
	PatternExplanation string  `json:"pattern_explanation"`
	Confidence         float32 `json:"confidence"`
	ProposedAs         string  `json:"proposed_as"`
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

	vendor := config.SynthVendor{Name: "idle-synth", Model: "MiniMax-M2.7", BaseURL: "https://api.minimax.io/anthropic/v1"}
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

// checkSynthDedup and markSynthDedup are wrappers around the package-level
// synthSeen map defined in synthesize.go.
func checkSynthDedup(key string) bool {
	synthSeenMu.Lock()
	defer synthSeenMu.Unlock()
	_, exists := synthSeen[key]
	return !exists
}

func markSynthDedup(key string) {
	synthSeenMu.Lock()
	defer synthSeenMu.Unlock()
	synthSeen[key] = time.Now()
}

// ---------------------------------------------------------------------------
// Concept Drift Detection
// ---------------------------------------------------------------------------

// driftResult holds the parsed output of one drift-detection query row.
type driftResult struct {
	id            string
	content       string
	currentConf   float64
	maxConf       float64
	confDelta     float64
	challengeCnt  int
	lifetimeCnt   int
}

// DetectConceptDriftCycle runs the drift-detection SQL query and proposes a
// pending theory for each artifact that exhibits the concept-drift signature:
//   - Was highly trusted (peak confidence ≥ 0.85)
//   - Has since fallen below 0.60
//   - Has dropped at least 0.25 from its peak
//   - Has received ≥ 2 negative challenges in the last 7 days
//   - Has ≥ 3 lifetime challenges (not a one-off fluke)
//
// The cycle is NOT gated by isQuiet() — drift detection is purely SQLite math
// with no filesystem dependency, so it runs on its own 6-hour ticker
// regardless of watcher activity.
func (w *IdleConsolidationWorker) DetectConceptDriftCycle() {
	w.logger.Info("idle_worker: starting concept drift detection cycle")

	// Cleanup stale entries from driftSeen (older than one drift interval).
	cleanDriftSeen()

	rows, err := w.db.QueryTracked(driftQuery)
	if err != nil {
		w.logger.Error("idle_worker: concept drift query failed", "error", err.Error())
		return
	}
	defer rows.Close()

	var proposed, skipped int
	for rows.Next() {
		var d driftResult
		if err := rows.Scan(&d.id, &d.content, &d.currentConf, &d.maxConf, &d.confDelta, &d.challengeCnt, &d.lifetimeCnt); err != nil {
			w.logger.Warn("idle_worker: scan drift row failed", "error", err.Error())
			continue
		}

		// SQLite-native dedup: check if a concept-drift theory for this artifact
		// was already proposed recently (within one driftCheckEvery window).
		// This survives process restarts unlike the in-memory driftSeen map.
		if hasRecentDriftTheory(w.db, d.id) {
			w.logger.Debug("idle_worker: skipping drift — recent theory exists", "artifact_id", d.id)
			skipped++
			continue
		}

		// In-process dedup as secondary safety.
		if !checkDriftDedup(d.id) {
			w.logger.Debug("idle_worker: skipping drift — in-process dedup", "artifact_id", d.id)
			skipped++
			continue
		}
		markDriftDedup(d.id)

		w.proposeConceptDriftTheory(d)
		proposed++
	}

	if err := rows.Err(); err != nil {
		w.logger.Error("idle_worker: drift row iteration error", "error", err.Error())
	}

	w.logger.Info("idle_worker: concept drift cycle complete",
		"proposed", proposed, "skipped", skipped)
}

// driftQuery is the SQLite CTE that identifies concept-drift signatures.
// Reviewed against schema.go — artifact_id/source_group field names and
// INTEGER Unix-seconds created_at are the canonical shapes in this codebase.
const driftQuery = `
WITH PeakConfidence AS (
	-- Use COALESCE so artifacts without a confidence_history row fall back
	-- to their current confidence as the peak (handles pre-migration rows).
	SELECT
		m.id AS artifact_id,
		COALESCE(MAX(ch.confidence), m.confidence) AS max_conf
	FROM memories m
	LEFT JOIN confidence_history ch ON m.id = ch.artifact_id
	WHERE m.deleted_at IS NULL
	GROUP BY m.id
),
RecentChallenges AS (
	-- Negative evidence in the last 7 days: explicit challenges OR any row
	-- with negative strength (observation/test/reproduction logged with a
	-- negative sign by the caller).
	SELECT artifact_id, COUNT(*) AS challenge_count
	FROM evidence
	WHERE (type = 'challenge' OR strength < 0)
	  AND created_at > (strftime('%s', 'now') - 604800)
	GROUP BY artifact_id
),
LifetimeChallenges AS (
	-- All-time negative evidence count; guards against burst-only false positives.
	SELECT artifact_id, COUNT(*) AS lifetime_count
	FROM evidence
	WHERE type = 'challenge' OR strength < 0
	GROUP BY artifact_id
)
SELECT
	m.id,
	m.content,
	m.confidence AS current_conf,
	p.max_conf,
	(p.max_conf - m.confidence) AS conf_delta,
	r.challenge_count,
	l.lifetime_count
FROM memories m
JOIN PeakConfidence p ON m.id = p.artifact_id
JOIN RecentChallenges r ON m.id = r.artifact_id
JOIN LifetimeChallenges l ON m.id = l.artifact_id
WHERE
	m.confidence < 0.60
	AND p.max_conf >= 0.85
	AND (p.max_conf - m.confidence) >= 0.25
	AND r.challenge_count >= 2
	AND l.lifetime_count >= 3
`

// proposeConceptDriftTheory saves a pending theory for a drifted artifact.
func (w *IdleConsolidationWorker) proposeConceptDriftTheory(d driftResult) {
	// Content snippet for the theory — truncate at 200 chars for readability.
	snippet := d.content
	if len(snippet) > 200 {
		snippet = snippet[:200] + "…"
	}

	hypothesis := fmt.Sprintf(
		"Concept drift detected. The memory %q (id: %s) has fallen from a peak "+
			"confidence of %.2f down to %.2f — a drop of %.2f — driven by %d recent "+
			"challenges (%d lifetime). This paradigm may be obsolete or have been "+
			"superseded by new evidence.",
		snippet, d.id, d.maxConf, d.currentConf, d.confDelta, d.challengeCnt, d.lifetimeCnt,
	)

	metadata := map[string]interface{}{
		"auto_generated":    true,
		"concept_drift":     true,
		"source_artifact":   d.id,
		"peak_confidence":   d.maxConf,
		"current_confidence": d.currentConf,
		"confidence_delta":  d.confDelta,
		"recent_challenges": d.challengeCnt,
		"lifetime_challenges": d.lifetimeCnt,
		"status":            "pending",
	}

	tags := []string{"concept-drift", "auto-generated", "idle-dream"}

	embedding := EmbedText(hypothesis)

	id, err := w.db.SaveMemory("theories", hypothesis, "", tags, metadata, embedding, true, 5)
	if err != nil {
		w.logger.Error("idle_worker: failed to save concept-drift theory",
			"artifact_id", d.id, "error", err.Error())
		return
	}

	// Patch the source memory so retrieval detects the concept-drift flag.
	// This is the hook that causes HybridSearch to prepend the quarantine banner.
	patch := fmt.Sprintf(`{"concept_drift":true,"drift_theory_id":"%s","drift_detected_at":%d}`, id, time.Now().Unix())
	if err := w.db.UpdateMemoryMetadata(d.id, patch); err != nil {
		w.logger.Warn("idle_worker: failed to patch source memory with concept_drift flag",
			"memory_id", d.id, "error", err.Error())
	}

	w.logger.Info("idle_worker: proposed concept-drift theory",
		"theory_id", id[:12],
		"source_artifact", d.id,
		"peak", d.maxConf,
		"current", d.currentConf,
		"delta", d.confDelta)
}

// ---------------------------------------------------------------------------
// Drift dedup helpers
// ---------------------------------------------------------------------------

func checkDriftDedup(id string) bool {
	driftSeenMu.Lock()
	defer driftSeenMu.Unlock()
	_, exists := driftSeen[id]
	return !exists
}

func markDriftDedup(id string) {
	driftSeenMu.Lock()
	defer driftSeenMu.Unlock()
	driftSeen[id] = time.Now()
}

// cleanDriftSeen removes entries older than one drift-check interval.
func cleanDriftSeen() {
	driftSeenMu.Lock()
	defer driftSeenMu.Unlock()
	cutoff := time.Now().Add(-6 * time.Hour)
	for k, v := range driftSeen {
		if v.Before(cutoff) {
			delete(driftSeen, k)
		}
	}
}

// hasRecentDriftTheory checks SQLite for a recently proposed concept-drift
// theory for the same source artifact. This is the persistent dedup layer
// that survives process restarts.
func hasRecentDriftTheory(db *DatabaseManager, sourceArtifactID string) bool {
	// Look for a theories collection memory whose metadata references this
	// source_artifact and was created within the last drift interval.
	var count int
	err := db.QueryRowTracked(`
		SELECT COUNT(*)
		FROM memories
		WHERE collection = 'theories'
		  AND deleted_at IS NULL
		  AND metadata LIKE ?
		  AND created_at > datetime('now', '-6 hours')
	`, "%"+sourceArtifactID+"%").Scan(&count)
	if err != nil {
		return false
	}
	return count > 0
}
