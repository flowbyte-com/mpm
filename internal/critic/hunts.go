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

	mpmcore "github.com/flowbyte-com/mpm-core"
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

// StaleMemoryHunt flags memories that are BOTH wall-clock stale AND
// sufficiently settled in cumulative scheduler-active uptime.
//
// A memory's confidence degrades without periodic reinforcement. If a
// memory has been silent for longer than MaxAge, the Critic emits a
// challenge_memory finding. The user (808) arbitrates.
//
// TWO INDEPENDENT CLOCKS, BOTH REQUIRED:
//
//	MaxAge         WALL CLOCK. "Is this stale?" — a recency judgement
//	               about the memory's content age.
//	SettlingPeriod CUMULATIVE SCHEDULER-ACTIVE UPTIME. "Has the system
//	               actually been up long enough to judge it fairly?"
//	               Measured as the global active-uptime counter minus the
//	               memory's own admission baseline.
//
// A memory is challengeable only when BOTH gates pass. The second gate
// exists so the Critic never judges a memory the system has barely had
// the opportunity to observe.
//
// WHY SETTLING IS NOT WALL CLOCK. The previous implementation computed
// `cycleStart.Add(-settling)` where cycleStart is time.Now() captured
// inside the one-shot mpm-critic process. That is identically
// `now - settling`, so the predicate carried no information beyond
// MaxAge: a scheduler outage of ANY length accrued settling credit. The
// in-code comment at the time claimed "a 13h daemon outage does NOT
// accumulate against the settling period" — that was false, and a
// memory with 6h of true active residency could be challenged after a
// 20h outage.
//
// Settling now reads durable active uptime that the PERSISTENT
// scheduler accrues from its own monotonic process elapsed time. A
// restart contributes no credit for the wall-clock gap preceding it.
//
// REINFORCEMENT DOES NOT RESET SETTLING. The baseline is immutable
// once written; it records admission residency, not a cooldown after
// epistemic reinforcement. The audit below reads updated_at ONLY for
// the MaxAge recency judgement — never for settling.
type StaleMemoryHunt struct {
	MaxAge         time.Duration // default: 30 days — WALL CLOCK
	SettlingPeriod time.Duration // default: 12h — CUMULATIVE SCHEDULER-ACTIVE UPTIME
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
	settleSeconds := int64(settling / time.Second)

	// Read the durable active-uptime total. An absent row reads as 0
	// (nothing has ever been accrued); a MALFORMED row is an explicit
	// error. Either way we fail closed below — we never guess.
	currentActive, err := mpmcore.ReadActiveUptime(ctx, a.DB())
	if err != nil {
		return nil, fmt.Errorf("stale_memory: read active uptime: %w", err)
	}

	maxAgeCutoff := time.Now().Add(-maxAge).Unix()

	// Select memories that pass BOTH gates.
	//
	// The settling predicate lives in SQL so the LIMIT selects genuinely
	// eligible rows rather than paging over ineligible ones.
	//
	// The LEFT JOIN + IS NOT NULL check is the "no baseline" rule: a
	// memory the scheduler has never observed has UNKNOWN residency, and
	// unknown must not read as settled. This also covers memories created
	// since the last scheduler tick and every pre-rollout memory.
	//
	// A negative difference (baseline ahead of the counter, which the
	// accrual guard prevents) simply fails `>= settleSeconds`, so it can
	// never manufacture eligibility.
	rows, err := a.DB().QueryContext(ctx, `
		SELECT m.id, m.content
		FROM memories m
		LEFT JOIN memory_settling_baselines b ON b.memory_id = m.id
		WHERE m.deleted_at IS NULL
		  AND m.updated_at IS NOT NULL
		  AND m.updated_at < ?
		  -- symmetry with MemoryStore.AutoPrunePolicy (memory.go:1400)
		  AND m.is_long_term = 0
		  -- shield persistent fixtures by tag. Use instr() (substring search) instead of
		  -- LIKE '%"seed"%' — double-quoted seed markers in a LIKE pattern collide with
		  -- SQLite's identifier-quoting rules and break the parser.
		  -- COALESCE because instr(NULL, ...) returns NULL, and NULL = 0 is FALSE.
		  AND COALESCE(instr(m.tags, '"seed"'), 0) = 0
		  AND COALESCE(instr(m.tags, '"alpha-fixture"'), 0) = 0
		  -- GATE 2: cumulative ACTIVE residency since admission.
		  -- Boundary is INCLUSIVE: exactly SettlingPeriod is eligible.
		  AND b.memory_id IS NOT NULL
		  AND (? - b.baseline_active_seconds) >= ?
		ORDER BY m.updated_at ASC
		LIMIT 20
	`, maxAgeCutoff, currentActive, settleSeconds)
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
