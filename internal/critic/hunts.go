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
// `evidence` and `memories` tables directly.
//
// WHAT IT EMITS (P3b, 2026-10-27). A PENDING PROPOSAL through
// `mpm_theories propose`, citing the memories it measured through P2's
// typed citation path (`source_type='memory'`).
//
// It used to emit `mpm_lessons save` — a TRUSTED lesson, written on every
// cycle, with no evidence and no review step. That is defect D2, and it
// was the last of the critic's automatic trusted-lesson writes.
// `mpm_theories propose` is the only existing untrusted proposal surface
// (adjudication §6c.1), so switching to it is what completes the
// transition, and it needs no schema change: `lessons_base` still has no
// pending state, and no pending state is added to it.
//
// THE MEASUREMENT IS SCOPED TO THE MEMORIES COLLECTION. The aggregate is
// computed over the `memories` collection — the memory save path the
// prey claim is about. The unscoped query conflated TWO variables: the
// provenance SOURCE ('call'/'direct') and the artifact KIND. On the live
// workspace the unscoped aggregate mixed 33 memories-collection rows with
// 10 agentshell, 6 project, 4 changelog, 1 lesson and 2 decision rows —
// every one of them stamped 'call' by the same save layer, but living
// under a different lifecycle with a different survival curve. It also
// made the typed citation impossible: P2's 'memory' source type resolves
// only against the memories collection, so an unscoped citation is
// REJECTED and the proposal could never land. See
// docs/dev/session-b-p3b-surface-switch.md §2 for the measurement.
//
// Everything else about the detection is preserved exactly: the same
// GROUP BY, the same 'call'/'direct' pair, the same sample-size gate, the
// same zero-survival guard and the same threshold comparison.
type SurvivalAsymmetryHunt struct {
	ThresholdRatio float64 // minimum ratio of (direct/call) survival to fire (default 2.0)
	MinSampleSize  int     // minimum memories per source before firing (default 10)
	// MaxCitations bounds how many memories of EACH cohort the proposal
	// cites. Zero means the default. The bound is per cohort because the
	// proposal rests on the CONTRAST between the two cohorts, and a
	// contrast needs both sides.
	MaxCitations int
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
	//
	// The `collection IN ('', 'memories')` scope is the P3b correction
	// described in the type's doc comment: the cohort must be the memory
	// save path, both because the prey claim is about memories and
	// because P2's 'memory' citation type resolves only against it.
	rows, err := a.DB().QueryContext(ctx, `
		SELECT
			json_extract(metadata, '$.source') AS source,
			COUNT(*) AS n,
			AVG(CASE WHEN deleted_at IS NULL THEN 1.0 ELSE 0.0 END) AS survival
		FROM memories
		WHERE json_extract(metadata, '$.source') IS NOT NULL
		  AND collection IN ('', 'memories')
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

	// The proposal cites the memories the hunt measured.
	//
	// Two constraints shape the citation set:
	//
	//  1. It must be LIVE. P2's typed path resolves a 'memory' citation
	//     against `collection IN ('', 'memories') AND deleted_at IS NULL`,
	//     so a soft-deleted row is REJECTED — and this hunt fires
	//     precisely BECAUSE some of its rows are soft-deleted. Citing
	//     the cohort's survivors is the only citable option, and it is
	//     also the honest one: the proposal says so explicitly.
	//  2. It must be STABLE. The OLDEST live rows of each cohort are
	//     taken, bounded by MaxCitations, so the set does not chase the
	//     newest activity and the proposal's meaning does not change
	//     from cycle to cycle.
	maxCitations := h.MaxCitations
	if maxCitations <= 0 {
		maxCitations = defaultCohortCitations
	}
	callIDs, err := citableCohortMemories(ctx, a, callS.source, maxCitations)
	if err != nil {
		return nil, err
	}
	directIDs, err := citableCohortMemories(ctx, a, directS.source, maxCitations)
	if err != nil {
		return nil, err
	}
	cited := make([]string, 0, len(callIDs)+len(directIDs))
	cited = append(cited, callIDs...)
	cited = append(cited, directIDs...)

	// The band is the quantized ratio. The exact ratio is a LIVE
	// measurement — rows age out of the cohorts — so it is VOLATILE and
	// must not enter an identity-bearing field. See ratioBand.
	band := ratioBand(ratio)

	// Asymmetry confirmed. Emit a PENDING PROPOSAL, not a trusted lesson.
	finding := Finding{
		Tool:   "mpm_theories",
		Action: "propose",
		// The cycle number lives HERE, not in the tags. A `cycle_<n>`
		// tag claims a per-cycle identity that the finding does not
		// have, and the tags are part of a proposal's identity hash —
		// so the moment this hunt moves to a proposal surface, the tag
		// would make every cycle a DISTINCT proposal. Removing it now
		// is what makes the surface switch safe.
		//
		// The measured figures live here too, for the same reason: the
		// ratio and the survival rates drift as rows age out, so they
		// are logged, never persisted into the proposal's identity.
		Reason: fmt.Sprintf(
			"survival asymmetry between the %q and %q source cohorts exceeds the %.2fx threshold "+
				"(critic cycle %d): %s=%.3f (%d rows), %s=%.3f (%d rows), ratio=%.2fx, band=%s",
			callS.source, directS.source, thr, a.Cycle(),
			directS.source, directS.survival, directS.n,
			callS.source, callS.survival, callS.n, ratio, band),
		Payload: map[string]interface{}{
			"hypothesis": fmt.Sprintf(
				"Memories recorded through the %q source path may be subject to a survival "+
					"disadvantage relative to those recorded %q (observed %s/%s survival ratio band: %s). "+
					"The cited memories are the surviving members of the two cohorts the critic measured; "+
					"the members that did not survive are not citable, so this proposal does not assert "+
					"the cause of the asymmetry. Whether decay, auto-archive, or a source-priority "+
					"bias in the write path is responsible is unresolved.",
				callS.source, directS.source, directS.source, callS.source, band),
			"validation_criteria": fmt.Sprintf(
				"A reviewer must recompute the survival rate of the %q and %q provenance cohorts over "+
					"the memories collection, establish which mechanism drives the difference (decay, "+
					"auto-archive, or a source-priority bias in the write path), and cite the specific "+
					"rows or lifecycle events that establish it as resolvable evidence. Until then the "+
					"proposal stays pending at the base confidence: an asymmetry between a provenance "+
					"label and a lifecycle outcome is a correlation, not an established cause.",
				callS.source, directS.source),
			"tags": []string{"critic", "survival-asymmetry", "decay-investigation"},
			// The typed citation: P2's 'memory' source type resolves each
			// cited id against the memories collection, so an id that does
			// not resolve — or that resolves to a soft-deleted row —
			// REJECTS the whole proposal and leaves no partial record.
			"source_ids":  cited,
			"source_type": "memory",
		},
		Priority: 2,
	}
	return []Finding{finding}, nil
}

// defaultCohortCitations is how many memories of EACH cohort a survival
// proposal cites when the hunt is not told otherwise.
//
// The proposal rests on the CONTRAST between two cohorts, so it needs both
// sides; the bound is per cohort for that reason. It is generous relative
// to the expected size — the live workspace's largest cohort is 33 rows —
// and exists so a pathological cohort cannot produce an unbounded payload.
const defaultCohortCitations = 5

// citableCohortMemories returns the ids of the LIVE memories of one
// provenance cohort, oldest first, bounded by limit.
//
// "Oldest first" makes the citation set a stable prefix of the cohort: a
// later cycle re-cites the same rows, so the proposal's evidence does not
// churn and the proposal's meaning does not change.
//
// "Live only" is required, not a preference: P2's typed path resolves a
// 'memory' citation against `deleted_at IS NULL`, so a soft-deleted row
// would be REJECTED and the whole proposal would fail to land.
func citableCohortMemories(ctx context.Context, a *Audit, source string, limit int) ([]string, error) {
	rows, err := a.DB().QueryContext(ctx, `
		SELECT id
		FROM memories
		WHERE collection IN ('', 'memories')
		  AND deleted_at IS NULL
		  AND json_extract(metadata, '$.source') = ?
		ORDER BY created_at ASC, id ASC
		LIMIT ?`, source, limit)
	if err != nil {
		return nil, fmt.Errorf("query citable cohort %q: %w", source, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan citable cohort %q: %w", source, err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate citable cohort %q: %w", source, err)
	}
	return out, nil
}

// ratioBand quantizes the observed direct/call survival ratio into a
// power-of-two band, e.g. 2.5 becomes "2x-4x" and 4.0 becomes "4x-8x".
//
// WHY QUANTIZED, AND WHY THIS MATTERS.
//
// The ratio is a LIVE measurement: rows age out of the cohorts as the
// substrate decays, so the exact ratio drifts from one cycle to the next.
// An exact ratio in the proposal's HYPOTHESIS would therefore make every
// cycle a DISTINCT proposal — the D4 defect in a new place, and the exact
// failure mode P3 removed from the tags.
//
// A band is stable under drift: the ratio has to cross a power of two
// before the proposal's identity changes. That is deliberate — a material
// change in the observed asymmetry SHOULD produce a distinct proposal,
// because it is a different observation, while ordinary decay drift should
// not.
//
// The band's crossing is the only thing that can change this proposal's
// identity. The cohorts, the wording and the tags are all fixed.
func ratioBand(ratio float64) string {
	if ratio <= 0 {
		return "unknown"
	}
	lo := 1.0
	for lo*2 <= ratio {
		lo *= 2
	}
	return fmt.Sprintf("%gx-%gx", lo, lo*2)
}

// truncateRunes shortens s to at most n RUNES, appending an ellipsis when
// it had to cut.
//
// Why runes and not bytes: the excerpt lands in the content of a proposal,
// which is FTS-indexed and read by agents. A byte truncation that splits a
// multi-byte rune produces invalid UTF-8 in the store.
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
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
//
// WHAT IT EMITS (P3b, 2026-10-27). A PENDING PROPOSAL through
// `mpm_theories propose`, citing the theory it observed through P2's typed
// citation path (`source_type='theory'`).
//
// It used to emit `mpm_lessons save` — a TRUSTED lesson, written on every
// cycle, with no evidence and no review step. That is defect D2, and it
// was the last of the critic's automatic trusted-lesson writes.
//
// THE DETECTION IS UNCHANGED. The same query, the same confidence floor,
// the same ordering and the same LIMIT. The proposal does not resolve the
// theory, does not promote it and does not withdraw it: it asks a reviewer
// to decide, which is exactly what the old lesson asked for in prose —
// except that the proposal carries the evidence and stays pending until
// someone acts on it.
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
		// Rune-safe truncation. The excerpt now lands in the CONTENT of
		// a proposal — which is FTS-indexed — so a byte truncation that
		// splits a multi-byte rune must not survive into the store.
		desc := truncateRunes(hyp, 100)
		findings = append(findings, Finding{
			Tool:   "mpm_theories",
			Action: "propose",
			// The measured figures live HERE, not in the hypothesis: a
			// theory's confidence DECAYS, so an exact confidence in an
			// identity-bearing field would make every cycle a DISTINCT
			// proposal. The cycle number is here for the same reason.
			Reason: fmt.Sprintf("weak pending theory %s (conf=%.3f, floor=%.2f, critic cycle %d): %s",
				id, conf, maxConf, a.Cycle(), desc),
			Payload: map[string]interface{}{
				"hypothesis": fmt.Sprintf(
					"The theory cited here (%s) is held at a confidence below the critic's floor: %s "+
						"Whether that reflects a genuinely unsupported claim (it should be resolved as "+
						"disproven) or merely thin corroboration (it should be supported with further "+
						"evidence) is unresolved; this proposal asks a reviewer to determine which, and "+
						"asserts neither outcome.",
					id, desc),
				"validation_criteria": fmt.Sprintf(
					"A reviewer must read the cited theory %s and decide whether it should be resolved as "+
						"disproven — citing the evidence that contradicts it — or retained pending with "+
						"further evidence gathered. Until that decision is made with resolvable evidence, "+
						"the proposal stays pending at the base confidence: a theory's confidence is "+
						"observation, not verdict.",
					id),
				"tags": []string{"critic", "weak-theory", fmt.Sprintf("theory_%s", id)},
				// The typed citation: P2's 'theory' source type resolves
				// the cited id against the theories collection, so an id
				// that does not resolve REJECTS the whole proposal and
				// leaves no partial record behind.
				"source_ids":  []string{id},
				"source_type": "theory",
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

	// Manufacture the counter-claim. Tag it poison_pill + target_<id> so
	// 808 can identify and arbitrate cleanly.
	//
	// D4 (2026-10-27, Session B's P3): the hypothesis and the tags must
	// be a PURE FUNCTION of the evidence. Two volatile values used to
	// live here and both are now in the Reason, which is logged and never
	// persisted:
	//
	//   - the CYCLE NUMBER. A `cycle_<n>` tag made every cycle's poison
	//     pill a DIFFERENT proposal (the tags are part of the F19
	//     identity hash), so the hunt flooded the substrate with
	//     indistinguishable pending theories — and it defeated the
	//     hunt's own idempotency. The cycle number is now in the Reason.
	//   - the CONFIDENCE. The confidence of a memory DECAYS between
	//     cycles, so embedding it made the hypothesis text change and
	//     produced a second proposal for the SAME target. It is a derived,
	//     moving property, not evidence; a reviewer reads the current
	//     value from the memory itself.
	//
	// The target's id and content remain in the hypothesis: they are the
	// evidence the claim rests on, and they are stable.
	hyp := fmt.Sprintf(
		"POISON PILL: manufactured counter-claim that the following high-confidence memory is false. Target memory %s: %s",
		id, desc)

	finding := Finding{
		Tool:   "mpm_theories",
		Action: "propose",
		Reason: fmt.Sprintf("poison pill cycle %d targeting %s (conf=%.3f)", h.Cycle, id, conf),
		Payload: map[string]interface{}{
			"hypothesis":          hyp,
			"validation_criteria": "Defense must cite evidence ledgers for " + id + " and resolve_theory as disproven within 7 days. If unresolved at next poison pill cycle, the inverse path is broken.",
			"tags": []string{
				"poison_pill",
				fmt.Sprintf("target_%s", id),
			},
			// The counter-claim rests on the target's existence, so the
			// target is cited. The typed path resolves it against the
			// memories collection, and REJECTS the proposal if the
			// memory is gone — which is right: a counter-claim about a
			// memory that no longer exists is not a counter-claim.
			"source_ids":  []string{id},
			"source_type": "memory",
		},
		Priority: 2,
	}
	return []Finding{finding}, nil
}
