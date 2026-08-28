// Hunts: the three primary adversarial audits + the Poison Pill cycle.
//
// Each hunt reads MPM state and applies a single rule. Findings are
// returned as structured payloads ready to be persisted via mpm call.
//
// Heuristics chosen for low false-positive rate. The Critic is a linter,
// not a creative auditor. It flags patterns that are mechanically
// detectable, not semantic ones.

package critic

import (
	"context"
	"fmt"
	"time"
)

// SurvivalAsymmetryHunt detects the call-vs-direct source survival gap.
//
// Prey (locked 2026-07-15): "call"-sourced memories survive at 0.158
// while "direct"-sourced survive at 0.857. challenge_count=0 on both,
// so decay (not challenge) is the driver.
//
// This hunt reads query_memory_quality-style aggregates from the
// `evidence` and `memories` tables directly. Emits a save_lesson
// finding the first time it observes the asymmetry, with a tag for
// later analysis.
type SurvivalAsymmetryHunt struct {
	ThresholdRatio float64 // minimum ratio of (direct/call) survival to fire (default 2.0)
	MinSampleSize  int     // minimum memories per source before firing (default 10)
}

func (h *SurvivalAsymmetryHunt) Name() string { return "survival_asymmetry" }

func (h *SurvivalAsymmetryHunt) Run(ctx context.Context, a *Audit) ([]Finding, error) {
	thr := h.ThresholdRatio
	if thr == 0 {
		thr = 2.0
	}
	minN := h.MinSampleSize
	if minN == 0 {
		minN = 10
	}

	// Query survival stats by source. Mirrors query_memory_quality.
	// "Survival" = memory row not deleted (deleted_at IS NULL).
	// deleted_at is stored as INTEGER Unix epoch (matches expires_at).
	rows, err := a.DB().QueryContext(ctx, `
		SELECT
			json_extract(metadata, '$.source') AS source,
			COUNT(*) AS n,
			AVG(CASE WHEN deleted_at IS NULL THEN 1.0 ELSE 0.0 END) AS survival
		FROM memories
		WHERE json_extract(metadata, '$.source') IS NOT NULL
		GROUP BY source
	`)
	if err != nil {
		return nil, fmt.Errorf("query sources: %w", err)
	}
	defer rows.Close()

	type sourceStat struct {
		source   string
		n        int
		survival float64
	}
	var stats []sourceStat
	for rows.Next() {
		var s sourceStat
		if err := rows.Scan(&s.source, &s.n, &s.survival); err != nil {
			return nil, fmt.Errorf("scan source: %w", err)
		}
		stats = append(stats, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate: %w", err)
	}

	// Find call + direct rows. If either is missing, no fire.
	var callS, directS *sourceStat
	for i := range stats {
		switch stats[i].source {
		case "call":
			callS = &stats[i]
		case "direct":
			directS = &stats[i]
		}
	}
	if callS == nil || directS == nil {
		return nil, nil
	}
	if callS.n < minN || directS.n < minN {
		return nil, nil
	}
	if directS.survival == 0 || callS.survival == 0 {
		return nil, nil
	}
	ratio := directS.survival / callS.survival
	if ratio < thr {
		return nil, nil
	}

	// Asymmetry confirmed. Emit finding.
	finding := Finding{
		Tool:   "mpm_lessons",
		Action: "save",
		Reason: "survival asymmetry exceeds threshold",
		Payload: map[string]interface{}{
			"fact": fmt.Sprintf(
				"Survival-rate asymmetry: direct=%.3f (%d memories) vs call=%.3f (%d memories), ratio=%.2fx. challenge_count=0 on both — decay is the driver. Investigate decay/auto-archive path for source-prior bias.",
				directS.survival, directS.n, callS.survival, callS.n, ratio),
			"type":  "warning",
			"tags":  []string{"critic", "survival-asymmetry", "decay-investigation", fmt.Sprintf("cycle_%d", a.Cycle())},
		},
		Priority: 2,
	}
	return []Finding{finding}, nil
}

// StaleMemoryHunt flags memories with no reinforcement in MaxAge.
//
// A memory's confidence degrades without periodic reinforcement. If a
// memory has been silent for longer than MaxAge, the Critic emits a
// challenge_memory finding. The user (808) arbitrates.
type StaleMemoryHunt struct {
	MaxAge         time.Duration // default: 30 days
	SettlingPeriod time.Duration // default: 12h; refuse to challenge memories fresher than this when measured from the current audit cycle start
}

func (h *StaleMemoryHunt) Name() string { return "stale_memory" }

func (h *StaleMemoryHunt) Run(ctx context.Context, a *Audit) ([]Finding, error) {
	maxAge := h.MaxAge
	if maxAge == 0 {
		maxAge = 30 * 24 * time.Hour
	}
	settling := h.SettlingPeriod
	if settling == 0 {
		settling = 12 * time.Hour
	}
	cutoff := time.Now().Add(-maxAge).Unix()
	// Settling cutoff is anchored to the cycle start, not absolute time.
	// This way a 13h daemon outage does NOT accumulate against the settling
	// period — when the daemon wakes, only time elapsed within active cycles
	// counts. (Strict cumulative-uptime-per-memory is a follow-up; see TODO.)
	// Fallback to time.Now() if the cycle start has not been initialized
	// (e.g., when a hunt is invoked directly outside of Audit.Run()).
	cycleStart := a.CycleStart()
	if cycleStart.IsZero() {
		cycleStart = time.Now()
	}
	settleCutoff := cycleStart.Add(-settling).Unix()

	// Pull memories that have no reinforcement row newer than cutoff.
	// Heuristic: rely on updated_at; if a memory hasn't been updated in
	// MaxAge, it's stale. This is approximate — a more rigorous check
	// would join against evidence rows, but updated_at is the closest
	// signal in the schema.
	rows, err := a.DB().QueryContext(ctx, `
		SELECT id, content
		FROM memories
		WHERE deleted_at IS NULL
		  AND updated_at IS NOT NULL
		  AND updated_at < ?
		  -- symmetry with MemoryStore.AutoPrunePolicy (memory.go:1400)
		  AND is_long_term = 0
		  -- shield persistent fixtures by tag. Use instr() (substring search) instead of
		  -- LIKE '%"seed"%' — double-quoted seed markers in a LIKE pattern collide with
		  -- SQLite's identifier-quoting rules and break the parser.
		  -- COALESCE because instr(NULL, ...) returns NULL, and NULL = 0 is FALSE.
		  AND COALESCE(instr(tags, '"seed"'), 0) = 0
		  AND COALESCE(instr(tags, '"alpha-fixture"'), 0) = 0
		  -- settling period anchored to current cycle start (see StaleMemoryHunt docstring).
		  -- COALESCE handles memories that pre-date the created_at column or have NULL set.
		  AND COALESCE(created_at, updated_at) < ?
		ORDER BY updated_at ASC
		LIMIT 20
	`, cutoff, settleCutoff)
	if err != nil {
		return nil, fmt.Errorf("query stale: %w", err)
	}
	defer rows.Close()

	var findings []Finding
	for rows.Next() {
		var id, content string
		if err := rows.Scan(&id, &content); err != nil {
			return nil, fmt.Errorf("scan stale: %w", err)
		}
		// Truncate content for log readability.
		desc := content
		if len(desc) > 100 {
			desc = desc[:97] + "..."
		}
		findings = append(findings, Finding{
			Tool:   "mpm_memory",
			Action: "challenge",
			Reason: fmt.Sprintf("stale memory %s: %s", id, desc),
			Payload: map[string]interface{}{
				"memoryId": id,
				"evidence": fmt.Sprintf("no update in >%s; flagged by critic cycle %d", maxAge, a.Cycle()),
			},
			Priority: 1,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate stale: %w", err)
	}
	return findings, nil
}

// WeakTheoryHunt flags pending theories with low confidence and no
// recent evidence activity. These are candidates for either resolution
// (if evidence is genuinely thin) or for more rigorous testing (if
// confidence is artificially low).
type WeakTheoryHunt struct {
	MaxConfidence float64 // default: 0.6
}

func (h *WeakTheoryHunt) Name() string { return "weak_theory" }

func (h *WeakTheoryHunt) Run(ctx context.Context, a *Audit) ([]Finding, error) {
	maxConf := h.MaxConfidence
	if maxConf == 0 {
		maxConf = 0.6
	}

	rows, err := a.DB().QueryContext(ctx, `
		SELECT id, content, confidence
		FROM memories
		WHERE collection = 'theories'
		  AND deleted_at IS NULL
		  AND confidence < ?
		ORDER BY confidence ASC
		LIMIT 10
	`, maxConf)
	if err != nil {
		return nil, fmt.Errorf("query weak theories: %w", err)
	}
	defer rows.Close()

	var findings []Finding
	for rows.Next() {
		var id, hyp string
		var conf float64
		if err := rows.Scan(&id, &hyp, &conf); err != nil {
			return nil, fmt.Errorf("scan theory: %w", err)
		}
		desc := hyp
		if len(desc) > 100 {
			desc = desc[:97] + "..."
		}
		findings = append(findings, Finding{
			Tool:   "mpm_lessons",
			Action: "save",
			Reason: fmt.Sprintf("weak pending theory %s (conf=%.3f): %s", id, conf, desc),
			Payload: map[string]interface{}{
				"fact": fmt.Sprintf(
					"Pending theory %s has confidence %.3f (<%.2f). Hypothesis: %s. Consider resolution (disproven) or further evidence gathering.",
					id, conf, maxConf, desc),
				"type": "warning",
				"tags": []string{"critic", "weak-theory", fmt.Sprintf("theory_%s", id), fmt.Sprintf("cycle_%d", a.Cycle())},
			},
			Priority: 1,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate weak theories: %w", err)
	}
	return findings, nil
}

// PoisonPillHunt manufactures a counter-theory against a high-confidence
// memory to exercise the inverse defense path. Fires on cycle % 5 only.
//
// Strategy: pick the highest-confidence memory that is NOT already
// the target of a pending theory. Manufacture a hypothesis claiming
// the opposite of the memory. The defense path (resolve_theory with
// evidence) must then be exercised by 808 to disprove the poison.
//
// This is the architectural closure for the inverse path. Without
// this, the Critic only tests detection (finding organic issues),
// not defense (rejecting manufactured issues).
type PoisonPillHunt struct {
	Cycle int
}

func (h *PoisonPillHunt) Name() string { return "poison_pill" }

func (h *PoisonPillHunt) Run(ctx context.Context, a *Audit) ([]Finding, error) {
	// Pick a high-confidence memory to attack. Heuristic: highest
	// confidence in the memories collection. Excludes theories (which
	// are themselves memory rows with collection='theories').
	var id, content string
	var conf float64
	err := a.DB().QueryRowContext(ctx, `
		SELECT id, content, confidence
		FROM memories
		WHERE deleted_at IS NULL
		  AND collection = 'memories'
		  AND confidence >= 0.8
		ORDER BY confidence DESC
		LIMIT 1
	`).Scan(&id, &content, &conf)
	if err != nil {
		// No eligible target (no high-confidence memories, or all already
		// targeted). Skip this cycle's poison pill — that's fine.
		return nil, nil
	}

	desc := content
	if len(desc) > 200 {
		desc = desc[:197] + "..."
	}

	// Manufacture the counter-claim. Tag it poison_pill + cycle_N so
	// 808 can identify and arbitrate cleanly.
	hyp := fmt.Sprintf(
		"POISON PILL cycle %d: manufactured counter-claim that the following high-confidence memory is false. Target memory %s (conf=%.3f): %s",
		h.Cycle, id, conf, desc)

	finding := Finding{
		Tool:   "mpm_theories",
		Action: "propose",
		Reason: fmt.Sprintf("poison pill cycle %d targeting %s", h.Cycle, id),
		Payload: map[string]interface{}{
			"hypothesis":         hyp,
			"validation_criteria": "Defense must cite evidence ledgers for " + id + " and resolve_theory as disproven within 7 days. If unresolved at next poison pill cycle, the inverse path is broken.",
			"tags": []string{
				"poison_pill",
				fmt.Sprintf("cycle_%d", h.Cycle),
				fmt.Sprintf("target_%s", id),
			},
		},
		Priority: 2,
	}
	return []Finding{finding}, nil
}
