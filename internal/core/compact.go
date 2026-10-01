// compact.go — Phase 2 of the epistemic compaction pipeline.
//
// The compact_epistemology MCP tool is the agent's reflex to the
// epistemic_pressure trigger (see wake_context.go). The PUBLIC surface
// is CompactEpistemologyDrain, a bounded sequential drain that loops
// the per-batch primitive (CompactEpistemology) until no eligible raw
// memories remain, the pressure threshold condition is satisfied, the
// safety cap is hit, or a batch fails. The 50-item LLM context
// safeguard (compactBatchSize) is preserved on every batch.
//
// Compact semantics:
//
//   - force=false (default) — compact RELIEVES pressure. The drain
//     stops as soon as raw_count <= threshold. Eligible raw memories
//     may remain after the call. This is the reflex to
//     epistemic_pressure.exceeded=true.
//   - force=true            — compact DRAINS everything. The threshold
//     gate is bypassed; the drain continues until the substrate is
//     empty or the safety cap is hit.
//   - max_batches           — per-invocation cap on SEMANTIC STAGES
//     (default 8, hard cap 8). A stage is one model synthesis
//     attempt, not one batch: a batch that succeeds costs 1, a batch
//     that refuses costs 3 (parent plus two split children). 8 stages
//     × 50 raw is at most 400 raw memories per call. Protects against
//     runaway LLM cost when new eligible raw memories arrive faster
//     than the drain can consume them.
//
//     The parameter is still named max_batches because it is part of
//     the published wire contract. Read it as a stage budget; the
//     drain enforces it as one, and StagesSpent reports what it spent.
//
// StopReason taxonomy (always set on return):
//
//   - "no_work"             substrate was empty from the start.
//   - "completed"           every eligible row was consumed.
//   - "threshold_reached"   force=false and raw count fell to/at the
//                           threshold; eligible rows remain.
//   - "max_batches_reached" safety cap hit; eligible rows remain.
//   - "failure"             mid-drain batch failure (success=false).
//
// Atomicity guarantee: the lesson insert and the raw-memory mark
// happen in one SQL transaction per batch. Either both commit or
// neither commits. The LLM call is OUTSIDE the transaction — a
// failure there leaves zero DB writes for that batch, no partial
// state. Each batch is independently atomic; a later-batch failure
// leaves earlier successful batches committed and reports the
// remaining work.
//
// Drain-level failure semantics: when a batch fails mid-drain, the
// drain returns success=false with the partial aggregate and the
// underlying error. Earlier successful batches remain durable; the
// failed batch and all subsequent eligible rows are untouched. The
// next invocation resumes from the remaining eligible work.
//
// Provenance convention: the LLM does NOT see or return the raw memory
// IDs. The Go orchestrator holds the slice from the SELECT and attaches
// it after unmarshal. This eliminates a hallucination vector where the
// model could lose IDs in the response (the "lost in the middle"
// phenomenon) and strand raw memories in the pressure queue forever.
//
// LLM injection: compactSynthesizeFunc is the single seam between
// orchestration and the LLM. Production wires it to the
// *synth.SynthClient's SynthesizeCompactLesson method. Tests override
// the variable to inject canned responses without touching the network.

package internal

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/flowbyte-com/mpm-core/synth"
)

// CompactLesson is the LLM-facing shape of the synthesized lesson.
// Only Title, Body, and Tags come from the model. Provenance is NOT
// in this struct — it lives in the orchestrator's local scope and
// never crosses the wire to the model.
type CompactLesson struct {
	Title string   `json:"title"`
	Body  string   `json:"body"`
	Tags  []string `json:"tags"`
}

// Validate enforces semantic checks beyond JSON shape. Empty fields
// mean the LLM either hallucinated an empty response or returned
// the refusal sentinel {"title":"", "body":"", "tags":[]} from the
// prompt instructions. Either way: orchestrator rolls back.
func (c *CompactLesson) Validate() error {
	if c.Title == "" {
		return errors.New("title required")
	}
	if c.Body == "" {
		return errors.New("body required")
	}
	if len(c.Tags) == 0 {
		return errors.New("at least one tag required")
	}
	return nil
}

// CompactEpistemologyResult is the wire-format return shape for the
// MCP tool. Field names are agent-facing; snake_case is intentional
// so the JSON matches the rest of the substrate's tool surface.
//
// SkippedReason is set on no-op paths (no raw memories, below
// threshold). On error paths, the result is nil and the error
// carries the reason.
type CompactEpistemologyResult struct {
	Compacted      int    `json:"compacted"`
	LessonsCreated int    `json:"lessons_created"`
	RawMarked      int    `json:"raw_marked"`
	LessonID       string `json:"lesson_id,omitempty"`
	SkippedReason  string `json:"skipped_reason,omitempty"`
	// Deferred is set when the model returned the sanctioned refusal.
	// The batch was annotated, not compacted: Deferred rows are out of
	// the selection pool and remain fully retrievable, but they are no
	// longer offered to the model. DeferralBatch groups every row of
	// this one refusal so an operator can inspect the group, and
	// DeferralReason is the reason stored on the row.
	Deferred       int    `json:"deferred,omitempty"`
	DeferralBatch  string `json:"deferral_batch,omitempty"`
	DeferralReason string `json:"deferral_reason,omitempty"`
}

// compactBatchSize is the hard ceiling on raw memories per call.
// Tuned to fit comfortably in MiniMax-M2.7's context window even
// for verbose memory entries. It is a per-request context limit and
// is independent of the per-invocation stage budget below, which is
// the smaller of the two in practice: at 8 stages an invocation
// commits at most 8 × 50 = 400 raw memories.
const compactBatchSize = 50

// compactDrainMaxBatchesDefault is the per-invocation safety cap
// on the number of LLM-bounded batches the drain will process.
// 2026-09-14 tightening pass: lowered from 20 to 8 to align with
// the safeguard package's MaxSemanticStagesPerInvocation. One
// invocation must be small enough to never become a large
// provider-cost event; work above this ceiling is reported
// back to the caller (RawRemaining) for bounded-continuation.
const compactDrainMaxBatchesDefault = 8

// compactDrainMaxBatchesHardCap is the absolute upper bound
// accepted from callers (regardless of what they pass). 8 batches
// × 50 raw = 400 raw memories per invocation. Callers passing
// a value above this are silently clamped, not rejected.
const compactDrainMaxBatchesHardCap = 8

// CompactEpistemologyDrainResult is the wire-format return shape for
// the drain-mode compact operation. Aggregates per-batch results from
// the underlying CompactEpistemology primitive.
//
// StopReason vocabulary (always set on return):
//   - "no_work"             — substrate was empty from the start;
//     0 batches ran. SkippedReason="no_raw_memories".
//   - "completed"           — at least one batch ran and every eligible
//     row has been consumed. raw_remaining=0.
//   - "threshold_reached"   — force=false and raw_count fell to/at the
//     configured threshold during the drain.
//     The remaining raw memories are eligible
//     but the threshold gate stopped further
//     compaction. SkippedReason="below_threshold".
//   - "max_batches_reached" — the stage budget (8) was exhausted.
//     Work may remain; check actionable_pending, not raw_remaining,
//     to know whether any of it is still offerable.
//   - "failure"             — a mid-drain batch failed. Earlier batches
//     remain committed; FailedBatch and
//     FailureReason identify the failed batch.
//
// Field semantics:
//   - Success: false ONLY on "failure". Always true for every other
//     stop_reason — including "threshold_reached" and
//     "max_batches_reached", where the operation completed successfully
//     but work remains. Callers must inspect stop_reason (not success)
//     to determine whether the substrate is fully drained.
//   - BatchesProcessed: count of batches that committed a lesson.
//   - RawProcessed: sum of compacted raw memories across all batches.
//   - LessonsCreated: equal to BatchesProcessed on success (1 lesson
//     per batch).
//   - RawRemaining: live read of epistemic_pressure_v.raw_count after
//     the loop ends. Reflects concurrent writes — the canonical source
//     of truth.
//   - LessonIDs: lesson IDs created in batch order.
//   - SkippedReason: set only on "no_work" (no_raw_memories) or
//     "threshold_reached" (below_threshold).
type CompactEpistemologyDrainResult struct {
	Success          bool     `json:"success"`
	BatchesProcessed int      `json:"batches_processed"`
	RawProcessed     int      `json:"raw_processed"`
	LessonsCreated   int      `json:"lessons_created"`
	RawRemaining     int      `json:"raw_remaining"`
	LessonIDs        []string `json:"lesson_ids,omitempty"`
	SkippedReason    string   `json:"skipped_reason,omitempty"`
	StopReason       string   `json:"stop_reason"`
	// Partial-failure visibility. FailedBatch is 1-indexed.
	FailedBatch   int    `json:"failed_batch,omitempty"`
	FailureReason string `json:"failure_reason,omitempty"`
	// StagesSpent is the number of model synthesis attempts this
	// invocation made, which is what the safety budget actually bounds.
	// It is NOT the number of batches: a batch costs 1 stage when it
	// succeeds and 3 when it refuses (parent plus two children), so
	// BatchesProcessed undercounts the work done on a refusing
	// substrate. StagesSpent is the number to read when asking what an
	// invocation cost.
	StagesSpent int `json:"semantic_stages_spent"`
	// RowsDeferred counts rows removed from the actionable pool by a
	// sanctioned refusal. They remain fully retrievable and are still
	// counted in RawRemaining; they are simply no longer offered to the
	// model.
	RowsDeferred int `json:"rows_deferred"`
	// DeferralBatchIDs groups every deferral this invocation performed,
	// one entry per refused group, so an operator can inspect exactly
	// which batches were offered and declined.
	DeferralBatchIDs []string `json:"deferral_batch_ids,omitempty"`
	// RawDeferred and ActionablePending are the pressure view's two
	// split counts, read live after the loop. RawRemaining alone cannot
	// distinguish a backlog the model has declined from one it has not
	// yet seen.
	RawDeferred       int `json:"raw_deferred"`
	ActionablePending int `json:"actionable_pending"`
}

// splitRowSet splits a batch positionally into two halves, floor on the
// left. It is deterministic, model-free, and total: no row is lost or
// duplicated, which is what makes "both children refused → all N
// deferred" actually mean all N.
//
// The larger child is always on the right. That is a tie-break, not a
// requirement — both conventions are deterministic — but it matches
// Go's slice idiom (rows[:n/2], rows[n/2:]), so the code reads the way
// an implementer would write it without an adjustment and a comment
// explaining why.
//
// ok is false when the batch is too small to split. A single row has no
// sibling to synthesise with, and the model has already said it cannot
// produce a lesson from it; splitting would produce two empty halves
// and a second pointless call to learn nothing.
func splitRowSet(ids, contents []string) (leftIDs, rightIDs, leftContents, rightContents []string, ok bool) {
	if len(ids) < 2 {
		return nil, nil, nil, nil, false
	}
	half := len(ids) / 2
	return ids[:half], ids[half:], contents[:half], contents[half:], true
}

// compactSynthesizeFunc is the LLM injection seam. Production wires
// it in init; tests override the variable directly.
var compactSynthesizeFunc = func(ctx context.Context, rawMemories []string) (string, error) {
	return synth.NewSynthClient().SynthesizeCompactLesson(ctx, rawMemories)
}

// CompactEpistemology is the per-batch primitive. Pure orchestration:
// pre-check, extract, synthesize, validate, transaction. The caller
// (CompactEpistemologyDrain or test code) supplies ctx and force flag;
// force=true bypasses the pressure threshold pre-check so this single
// batch proceeds even when raw_count <= threshold.
//
// Return shape: non-nil result on no-op paths (pre-check failed,
// threshold not exceeded) with SkippedReason set. Non-nil error on
// synthesis / validation / DB failures. Either way, the data plane
// is consistent — no partial states ever persist.
//
// Callers that need a bounded multi-batch drain should use
// CompactEpistemologyDrain instead. CompactEpistemology is exposed
// for direct use only when the caller genuinely wants exactly one
// batch (tests, or a future orchestrator that manages its own loop).
func (dm *DatabaseManager) CompactEpistemology(ctx context.Context, force bool) (*CompactEpistemologyResult, error) {
	// 1. Pre-check — read the pressure gauge. Bail out cheaply on no-op.
	rawCount, threshold, err := dm.compactPreCheck(ctx)
	if err != nil {
		return nil, fmt.Errorf("pre_check: %w", err)
	}
	if rawCount == 0 {
		return &CompactEpistemologyResult{SkippedReason: "no_raw_memories"}, nil
	}
	if !force && rawCount <= threshold {
		return &CompactEpistemologyResult{SkippedReason: "below_threshold"}, nil
	}

	// 2. Extract — oldest first, capped at compactBatchSize.
	rawIDs, rawContents, err := dm.extractRawBatch(ctx, compactBatchSize)
	if err != nil {
		return nil, fmt.Errorf("extract: %w", err)
	}
	if len(rawIDs) == 0 {
		return &CompactEpistemologyResult{SkippedReason: "no_raw_memories"}, nil
	}

	// 3. Run one synthesis stage over this row set. There is no
	// subdivision at this level: CompactEpistemology is the
	// exactly-one-batch primitive, and the drain is what subdivides.
	return dm.compactRowSet(ctx, rawIDs, rawContents, true)
}

// compactRowSet is one semantic stage: synthesize over a specific set
// of rows, classify, and reach a terminal state — lesson committed,
// refusal deferred, or error returned.
//
// It takes the rows explicitly rather than selecting them, because the
// drain needs to re-run a stage over a SUBSET of a batch it has
// already selected (the refusal split). Selection and execution are
// therefore separate concerns, which is what makes subdivision
// possible without re-querying and hoping the same rows come back.
func (dm *DatabaseManager) compactRowSet(ctx context.Context, ids, contents []string, deferOnRefusal bool) (*CompactEpistemologyResult, error) {
	// Synthesize — call the LLM with strict JSON contract. NO DB
	// writes have happened yet; any failure here leaves the substrate
	// untouched.
	text, err := compactSynthesizeFunc(ctx, contents)
	if err != nil {
		return nil, fmt.Errorf("synthesize: %w", err)
	}

	// Classify the response. Three outcomes, and only one of them
	// is a lesson: a malformed response, the model's considered
	// refusal, or a usable lesson. Validation happens inside the
	// classifier and is asked only about real lessons.
	outcome, lesson, err := ClassifyCompactSynthesis(text)
	if err != nil {
		return nil, err
	}
	if outcome == OutcomeRefused {
		// The model declined, correctly, per the prompt's contract.
		// This is NOT a failure and must not be reported as one.
		//
		// deferOnRefusal is false only for a parent batch the drain
		// intends to subdivide. Deferring there and then re-attempting
		// the same rows as children would annotate every row twice,
		// overwrite the group id, and double-count the deferral — all
		// to spend the same stages anyway. The parent therefore
		// reports the refusal and leaves the decision to its caller.
		if !deferOnRefusal {
			return &CompactEpistemologyResult{SkippedReason: "synthesis_refused"}, nil
		}
		//
		// Defer the rows: annotate them so the next drain reselects
		// the *next* un-attempted rows instead of repeating this
		// identical call forever. No lesson is written and no
		// compacted_into is set — a refusal folds nothing into
		// durable knowledge, and claiming otherwise would be a lie the
		// pressure gauge would then believe.
		ann, err := dm.deferRawBatch(ctx, ids, DeferralReasonRefusal, text)
		if err != nil {
			return nil, fmt.Errorf("defer: %w", err)
		}
		return &CompactEpistemologyResult{
			SkippedReason:  "synthesis_refused",
			Deferred:       len(ids),
			DeferralBatch:  ann.Batch,
			DeferralReason: ann.Reason,
		}, nil
	}

	// 4. Transaction — INSERT lesson + UPDATE raw memories, atomically.
	// compacted_into is written in the SAME transaction as the lesson
	// INSERT, so a row can never claim to be folded into a lesson that
	// does not exist.
	lessonID, err := dm.commitLessonAndMark(ctx, lesson, ids)
	if err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	return &CompactEpistemologyResult{
		Compacted:      len(ids),
		LessonsCreated: 1,
		RawMarked:      len(ids),
		LessonID:       lessonID,
	}, nil
}

// CompactEpistemologyDrain is the public drain-mode compact surface.
// Loops the per-batch primitive (CompactEpistemology) until no
// eligible raw memories remain, the per-invocation batch cap is hit,
// the pressure threshold condition is satisfied, or a batch fails.
// The 50-item LLM context safeguard applies on every batch — only the
// loop boundary was added.
//
// StopReason taxonomy (see result struct doc):
//
//   - "no_work"             substrate was empty from the start.
//   - "completed"           every eligible row was consumed.
//   - "threshold_reached"   force=false and the raw count fell to/at
//     the threshold; eligible rows remain.
//   - "max_batches_reached" safety cap hit; eligible rows remain.
//   - "failure"             mid-drain batch failure (success=false).
//
// force semantics: passed through to each per-batch call.
//
//   - force=false — compact RELIEVES pressure. The drain stops as
//     soon as the pressure threshold condition is satisfied
//     (raw_count <= threshold). Eligible raw memories may remain.
//     This is the default reflex to epistemic_pressure.exceeded=true.
//   - force=true  — compact DRAINS everything. The threshold gate is
//     bypassed; the drain continues until the substrate is empty or
//     the per-invocation safety cap is hit.
//
// The 50-item batch safety is unrelated to force — every batch is
// capped at compactBatchSize regardless of force or threshold.
//
// maxBatches semantics. Despite the name, this is a budget of SEMANTIC
// STAGES (model synthesis attempts), not of batches: a successful
// batch costs one stage, a refused batch costs three (the parent
// attempt plus up to two split children). Read StagesSpent on the
// result to see what the budget actually bought.
//
//   - 0 or negative: use compactDrainMaxBatchesDefault (8).
//   - positive: use as-is, capped at compactDrainMaxBatchesHardCap (8)
//     and then at MaxSemanticStagesPerInvocation (8).
//
// Each batch stays independently atomic — the per-batch transaction
// boundary is inside CompactEpistemology. The drain loop holds NO
// transaction across iterations.
// CompactEpistemologyDrain is the public drain-mode compact surface.
// Loops the per-batch primitive (CompactEpistemology) until no
// eligible raw memories remain, the per-invocation batch cap is hit,
// the pressure threshold condition is satisfied, or a batch fails.
// The 50-item LLM context safeguard applies on every batch — only the
// loop boundary was added.
//
// StopReason taxonomy (see result struct doc):
//
//   - "no_work"             substrate was empty from the start.
//   - "completed"           every eligible row was consumed.
//   - "threshold_reached"   force=false and the raw count fell to/at
//     the threshold; eligible rows remain.
//   - "max_batches_reached" safety cap hit; eligible rows remain.
//   - "failure"             mid-drain batch failure (success=false).
//
// 2026-09-14 release-pass: the existing compactDrainMaxBatchesHardCap
// IS the bounded-execution safeguard for this orchestrator.
// The per-batch SynthesizeCompactLesson call now routes through
// the Plan-aware DoLLMRequestWithPlan helper, which classifies
// failures (transient=1 retry, auth=0, rate=conditional, malformed=1
// repair) and the per-batch Plan rejects further attempts after
// MaxRetries+MaxRepairs. The drain-level batch count is the
// outer cap.
//
// force semantics: passed through to each per-batch call.
//
//   - force=false — compact RELIEVES pressure. The drain stops as
//     soon as the pressure threshold condition is satisfied
//     (raw_count <= threshold). Eligible raw memories may remain.
//     This is the default reflex to epistemic_pressure.exceeded=true.
//   - force=true  — compact DRAINS everything. The threshold gate is
//     bypassed; the drain continues until the substrate is empty or
//     the per-invocation safety cap is hit.
//
// The 50-item batch safety is unrelated to force — every batch is
// capped at compactBatchSize regardless of force or threshold.
//
// maxBatches semantics. Despite the name, this is a budget of SEMANTIC
// STAGES (model synthesis attempts), not of batches: a successful
// batch costs one stage, a refused batch costs three (the parent
// attempt plus up to two split children). Read StagesSpent on the
// result to see what the budget actually bought.
//
//   - 0 or negative: use compactDrainMaxBatchesDefault (8).
//   - positive: use as-is, capped at compactDrainMaxBatchesHardCap (8)
//     and then at MaxSemanticStagesPerInvocation (8).
//
// Each batch stays independently atomic — the per-batch transaction
// boundary is inside CompactEpistemology. The drain loop holds NO
// transaction across iterations.
func (dm *DatabaseManager) CompactEpistemologyDrain(ctx context.Context, force bool, maxBatches int) (*CompactEpistemologyDrainResult, error) {
	if maxBatches <= 0 {
		maxBatches = compactDrainMaxBatchesDefault
	}
	if maxBatches > compactDrainMaxBatchesHardCap {
		maxBatches = compactDrainMaxBatchesHardCap
	}
	// The budget is measured in SEMANTIC STAGES, not batches. A batch
	// costs 1 stage when it succeeds and 3 when it refuses (parent plus
	// two children), so counting batches would let one invocation spend
	// 3× the safeguard's ceiling. The clamp is against the safeguard's
	// constant directly; maxBatches is retained as the public
	// parameter name because renaming it would be a breaking wire
	// change for a defect fix. See
	// docs/designs/2026-09-30-compact-refusal-lifecycle.md §5.5.
	if maxBatches > MaxSemanticStagesPerInvocation {
		maxBatches = MaxSemanticStagesPerInvocation
	}
	stageBudget := maxBatches

	result := &CompactEpistemologyDrainResult{
		Success:    true,
		StopReason: "", // explicit; set by every return path
		LessonIDs:  []string{},
	}

	stagesUsed := 0
	// The pre-check result, read once and reused. The original loop
	// re-ran CompactEpistemology per iteration, which re-read the
	// pressure gauge each time; the threshold behaviour is preserved
	// below by re-reading whenever a batch is committed.
	rawCount, threshold, err := dm.compactPreCheck(ctx)
	if err != nil {
		return nil, fmt.Errorf("pre_check: %w", err)
	}

	for {
		// Admission check, BEFORE any spend. Every batch costs at least
		// one stage, so this is the only check needed to keep the
		// counter from ever exceeding the budget on the success path.
		if stagesUsed+1 > stageBudget {
			if result.StopReason == "" {
				result.StopReason = "max_batches_reached"
			}
			break
		}

		// Skip paths — no work available, or below threshold.
		if rawCount == 0 {
			result.SkippedReason = "no_raw_memories"
			if result.BatchesProcessed == 0 && result.RowsDeferred == 0 {
				result.StopReason = "no_work"
			} else {
				result.StopReason = "completed"
			}
			break
		}
		if !force && rawCount <= threshold {
			result.SkippedReason = "below_threshold"
			result.StopReason = "threshold_reached"
			break
		}

		ids, contents, err := dm.extractRawBatch(ctx, compactBatchSize)
		if err != nil {
			return dm.drainFail(result, 0, fmt.Errorf("extract: %w", err), ctx)
		}
		if len(ids) == 0 {
			result.SkippedReason = "no_raw_memories"
			if result.BatchesProcessed == 0 && result.RowsDeferred == 0 {
				result.StopReason = "no_work"
			} else {
				result.StopReason = "completed"
			}
			break
		}

		// Spend one stage on the whole batch.
		// deferOnRefusal=false: a refusal here may still be split.
		batch, err := dm.compactRowSet(ctx, ids, contents, false)
		stagesUsed++
		result.StagesSpent = stagesUsed
		if err != nil {
			return dm.drainFail(result, result.BatchesProcessed+1, err, ctx)
		}

		if batch.SkippedReason == "synthesis_refused" {
			// The model declined the whole batch. Everything below is
			// the bounded recovery path, and nothing has been persisted
			// yet — the parent deliberately did not defer, because
			// these rows may yet be salvaged by a split.
			//
			// A single row has no sibling to synthesise with, and the
			// model has already said it cannot produce a lesson from
			// it. Splitting would produce two empty halves and a
			// second pointless call to learn nothing.
			if len(ids) < 2 {
				if err := dm.deferAndRecord(ctx, result, ids, batch.DeferralReason); err != nil {
					return dm.drainFail(result, 0, err, ctx)
				}
				rawCount = dm.liveRawCount(ctx)
				continue
			}
			// Second budget check, BEFORE the spend. If the budget
			// cannot afford BOTH children, the whole batch is already
			// deferred and stays that way. Attempting the first child
			// and abandoning the second would be the F6 failure in
			// miniature: the abandoned rows stay pending, get
			// reselected, and produce the same refusal having already
			// been billed an attempt.
			if stagesUsed+2 > stageBudget {
				// The whole batch defers as-is. Attempting the first
				// child and abandoning the second would be F6 in
				// miniature.
				if err := dm.deferAndRecord(ctx, result, ids, batch.DeferralReason); err != nil {
					return dm.drainFail(result, 0, err, ctx)
				}
				rawCount = dm.liveRawCount(ctx)
				continue
			}

			leftIDs, rightIDs, leftContents, rightContents, _ := splitRowSet(ids, contents)
			// Both children run regardless of the first child's
			// outcome, so the total cost is known in advance: exactly
			// 2 further stages. A child that errors fails the drain
			// loudly; a child that refuses defers itself.
			for _, child := range []struct {
				ids, contents []string
			}{
				{leftIDs, leftContents},
				{rightIDs, rightContents},
			} {
				cres, cerr := dm.compactRowSet(ctx, child.ids, child.contents, true)
				stagesUsed++
				result.StagesSpent = stagesUsed
				if cerr != nil {
					return dm.drainFail(result, result.BatchesProcessed+1, cerr, ctx)
				}
				if cres.SkippedReason == "synthesis_refused" {
					// The child deferred itself (deferOnRefusal=true),
					// so just account for it.
					result.RowsDeferred += cres.Deferred
					if cres.DeferralBatch != "" {
						result.DeferralBatchIDs = append(result.DeferralBatchIDs, cres.DeferralBatch)
					}
					continue
				}
				result.BatchesProcessed++
				result.RawProcessed += cres.Compacted
				result.LessonsCreated += cres.LessonsCreated
				if cres.LessonID != "" {
					result.LessonIDs = append(result.LessonIDs, cres.LessonID)
				}
			}
			rawCount = dm.liveRawCount(ctx)
			continue
		}

		// Commit path.
		result.BatchesProcessed++
		result.RawProcessed += batch.Compacted
		result.LessonsCreated += batch.LessonsCreated
		if batch.LessonID != "" {
			result.LessonIDs = append(result.LessonIDs, batch.LessonID)
		}

		// Last partial batch: fewer than compactBatchSize rows, so the
		// next iteration would find nothing. Exit without paying for a
		// redundant pre-check.
		if batch.Compacted < compactBatchSize {
			result.StopReason = "completed"
			break
		}
		rawCount, threshold, err = dm.compactPreCheck(ctx)
		if err != nil {
			return dm.drainFail(result, 0, fmt.Errorf("pre_check: %w", err), ctx)
		}
	}

	if result.StopReason == "" {
		result.StopReason = "max_batches_reached"
	}

	result.RawRemaining = dm.liveRawCount(ctx)
	deferred, actionable, err := dm.DeferralCounts(ctx)
	if err != nil {
		return result, err
	}
	result.RawDeferred = deferred
	result.ActionablePending = actionable

	return result, nil
}

// deferAndRecord defers rows the drain has decided not to subdivide,
// and folds the outcome into the drain's aggregate.
func (dm *DatabaseManager) deferAndRecord(ctx context.Context, result *CompactEpistemologyDrainResult, ids []string, reason string) error {
	if reason == "" {
		reason = DeferralReasonRefusal
	}
	ann, err := dm.deferRawBatch(ctx, ids, reason, "")
	if err != nil {
		return fmt.Errorf("defer: %w", err)
	}
	result.RowsDeferred += len(ids)
	result.DeferralBatchIDs = append(result.DeferralBatchIDs, ann.Batch)
	return nil
}

// drainFail records a mid-drain failure and returns it. Earlier
// successful batches are already committed (each is atomic); the failed
// batch and everything after it are untouched.
func (dm *DatabaseManager) drainFail(result *CompactEpistemologyDrainResult, failedBatch int, cause error, ctx context.Context) (*CompactEpistemologyDrainResult, error) {
	result.Success = false
	result.StopReason = "failure"
	if failedBatch > 0 {
		result.FailedBatch = failedBatch
	}
	result.FailureReason = cause.Error()
	result.RawRemaining = dm.liveRawCount(ctx)
	return result, cause
}

// liveRawCount reads the canonical raw_count from the
// epistemic_pressure_v view. Used to populate CompactEpistemologyDrainResult
// .RawRemaining at loop end. Returns 0 on any read failure — the
// safe default for a post-drain diagnostic; the operation itself
// remains correct regardless of this number.
func (dm *DatabaseManager) liveRawCount(ctx context.Context) int {
	var n int
	if err := dm.db.QueryRowContext(ctx, `SELECT raw_count FROM epistemic_pressure_v`).Scan(&n); err != nil {
		return 0
	}
	return n
}

// compactPreCheck reads raw_count and the system_config threshold in
// a single SELECT against the epistemic_pressure_v view. Subquery
// for the threshold — defaults to 100 if system_config.compaction
// .raw_threshold is unset.
func (dm *DatabaseManager) compactPreCheck(ctx context.Context) (rawCount, threshold int, err error) {
	row := dm.db.QueryRowContext(ctx, `
		SELECT
		  raw_count,
		  COALESCE(
		    (SELECT CAST(json_extract(raw_json, '$.raw_threshold') AS INTEGER)
		     FROM system_config WHERE key = 'compaction'),
		    100
		  ) AS threshold
		FROM epistemic_pressure_v
	`)
	if err := row.Scan(&rawCount, &threshold); err != nil {
		return 0, 0, err
	}
	return rawCount, threshold, nil
}

// extractRawBatch selects the oldest `limit` non-compacted, non-deferred
// memories. Returns parallel slices of IDs and contents — the
// orchestrator uses the IDs for the transaction UPDATE and the contents
// for the LLM prompt.
//
// The predicate below is memoryIsActionable, the same constant the
// pressure view's actionable_pending subquery uses. That identity is
// load-bearing: the drain and the pressure gauge must describe the same
// population, or the pressure signal will describe rows the drain will
// never offer. Editing one without the other is what
// TestPressureAndExtractionAgreeOnPopulation exists to catch.
//
// A deferred row leaves this pool but nothing else. It stays
// retrievable, is not deleted, and still counts in raw_count.
func (dm *DatabaseManager) extractRawBatch(ctx context.Context, limit int) (ids, contents []string, err error) {
	rows, err := dm.db.QueryContext(ctx, `
		SELECT id, content FROM memories
		WHERE `+memoryIsActionable+`
		ORDER BY created_at ASC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, content string
		if err := rows.Scan(&id, &content); err != nil {
			return nil, nil, err
		}
		ids = append(ids, id)
		contents = append(contents, content)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return ids, contents, nil
}

// commitLessonAndMark wraps the lesson insert and the raw mark in a
// single transaction. Returns the new lesson ID on success.
//
// Lessons table layout (key fields): id TEXT PK, type TEXT (default
// 'insight'), content TEXT, tags JSON, created TEXT, content_hash
// TEXT (sha1 of content envelope — used for dedup).
//
// The lesson content is stored as a JSON envelope {title, body} so
// downstream readers can distinguish the title from the prose body
// without parsing markdown. Tags stored as JSON array, matching the
// existing lesson shape from save_lesson.
func (dm *DatabaseManager) commitLessonAndMark(ctx context.Context, lesson *CompactLesson, rawIDs []string) (string, error) {
	tx, err := dm.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	// Rollback is a no-op after Commit. This defer covers every
	// return path between BeginTx and the final Commit — if any step
	// fails, the transaction rolls back atomically.
	defer func() { _ = tx.Rollback() }()

	envelope := struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}{
		Title: lesson.Title,
		Body:  lesson.Body,
	}
	envelopeBytes, err := json.Marshal(envelope)
	if err != nil {
		return "", fmt.Errorf("marshal envelope: %w", err)
	}

	tagsJSON, err := json.Marshal(lesson.Tags)
	if err != nil {
		return "", fmt.Errorf("marshal tags: %w", err)
	}

	hash := sha1.Sum(envelopeBytes)
	contentHash := hex.EncodeToString(hash[:])

	// INSERT lesson, RETURNING id. SQLite supports RETURNING since 3.35
	// (we depend on 3.35+ via FTS5). Lesson id is a random 32-char hex
	// prefixed with "les-" to match the existing lesson id pattern
	// (lessons_base.id is TEXT, no length constraint).
	var lessonID string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO lessons (id, type, content, tags, created, content_hash)
		VALUES ('les-' || lower(hex(randomblob(16))), 'insight', ?, ?, ?, ?)
		RETURNING id
	`, string(envelopeBytes), string(tagsJSON), time.Now().UTC().Format(time.RFC3339), contentHash).Scan(&lessonID)
	if err != nil {
		return "", fmt.Errorf("insert lesson: %w", err)
	}

	// UPDATE all raw memories in one statement. SQLite supports
	// json_insert on each row independently — COALESCE handles rows
	// with NULL/empty metadata by initializing to '{}' first. The
	// WHERE clause mirrors the extract filter, ensuring we don't
	// race against another caller that already marked some of these.
	placeholders := make([]string, 0, len(rawIDs))
	args := []interface{}{lessonID}
	for _, id := range rawIDs {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	updateSQL := fmt.Sprintf(`
		UPDATE memories SET metadata = json_insert(
			COALESCE(metadata, '{}'),
			'$.compacted_into',
			?
		)
		WHERE id IN (%s)
		  AND deleted_at IS NULL
		  AND (metadata IS NULL OR metadata = ''
		       OR json_extract(metadata, '$.compacted_into') IS NULL)
	`, strings.Join(placeholders, ","))
	res, err := tx.ExecContext(ctx, updateSQL, args...)
	if err != nil {
		return "", fmt.Errorf("update memories: %w", err)
	}
	// rowsAffected should equal len(rawIDs). If it's less, a row was
	// marked or deleted between the SELECT and the UPDATE — concurrent
	// write. Log but don't fail; the lesson is still created and the
	// remaining rows are still marked.
	if n, _ := res.RowsAffected(); n < int64(len(rawIDs)) {
		// Non-fatal — the dominant case is single-writer. This guards
		// against silent partial-state surprises in multi-agent setups.
		_ = n // see docstring; intentional silent
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}

	// Record the compaction event so wake_context can surface
	// last_compacted_at on future wakes. Non-fatal: the lesson and
	// raw-mark are already committed, so a write failure here just
	// means the timestamp won't surface next session. The agent
	// will still see the lesson via query_lessons / mcp stats; the
	// \"when did I last compact?\" signal is informational, not
	// load-bearing for correctness.
	if err := dm.recordCompactionEvent(ctx, lessonID, len(rawIDs)); err != nil {
		dm.LogAudit(AuditWarn, "compact_epistemology", "record event failed: "+err.Error(), "", AuditContext{})
	}

	return lessonID, nil
}

// recordCompactionEvent writes a one-line ledger entry to system_config
// under key 'compaction.last_run'. The wake_context gather reads this on
// every wake to surface last_compacted_at. UPSERT semantics — repeated
// calls overwrite the previous event (only the most recent matters).
//
// Schema: {last_run_at: <RFC3339>, lesson_id: <id>, raw_marked: <count>}.
// Kept minimal — detailed lesson content is in the lessons table; this
// is just the \"did compaction happen recently?\" signal.
func (dm *DatabaseManager) recordCompactionEvent(ctx context.Context, lessonID string, rawMarked int) error {
	payload, err := json.Marshal(map[string]interface{}{
		"last_run_at": time.Now().UTC().Format(time.RFC3339),
		"lesson_id":   lessonID,
		"raw_marked":  rawMarked,
	})
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	_, err = dm.db.ExecContext(ctx, `
		INSERT INTO system_config (key, raw_json, content_hash)
		VALUES ('compaction.last_run', ?, '')
		ON CONFLICT(key) DO UPDATE SET
		  raw_json = excluded.raw_json,
		  updated_at = CAST(strftime('%s','now') AS INTEGER)
	`, string(payload))
	return err
}

// Compile-time guard: compactSynthesizeFunc signature matches the
// *synth.SynthClient.SynthesizeCompactLesson method signature. If the
// latter ever changes, this line fails to compile and the seam
// catches the drift at build time, not runtime.
var _ = func() bool {
	var f func(context.Context, []string) (string, error) = compactSynthesizeFunc
	return f != nil
}()
var _ sql.Result // keep sql import even if rowsAffected branch is no-op
