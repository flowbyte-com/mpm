// challenge_state.go — the one primitive every challenge writer shares.
//
// Issue #24. Three production paths set `status=challenged` on a memory:
//
//	ChallengeMemory / ChallengeMemoryWithTheory / `mpm challenge`
//	    local `memories`. Reduces weight by a fixed amount, neutralises the
//	    memory's evidence, drops confidence to the challenged floor, and
//	    links a pending theory that a restore will mark disproven.
//
//	applyResolution  (`mpm ops resolve-contradictions --apply`, decisive
//	    margin only)
//	    `shared.memories`. Reduces weight AND retrieval_priority.
//
//	applyArbitrationResolution  (operator closes a close-call theory)
//	    `shared.memories`. Reduces weight AND retrieval_priority.
//
// Before this file each wrote its own metadata patch inline. The two Arc 1
// writers recorded NOTHING reversible: no prior weight, no prior retrieval
// priority, and — because the queue row's `resolution_memory_id` points at
// the resolution memory and not at a theory — no forward link to whatever
// contested them. `mpm challenge restore` refused such a memory outright
// ("is not challenged"), so a legitimate, operator-adjudicated verdict was
// permanently irreversible. That is the worse of the two: adjudication gates
// *who* may trigger a slash (#10), and once triggered nobody could take it
// back through MPM's own interfaces.
//
// This file is the fix's load-bearing piece. One statement, inside the
// caller's transaction, does all of it:
//
//  1. merges the challenge's metadata into the memory's JSON;
//  2. captures the pre-challenge baseline for every column this challenge is
//     about to change — and only those columns;
//  3. applies the reduction.
//
// Atomicity is therefore structural rather than a convention: the metadata
// that describes the change and the change itself cannot diverge, because
// there is only one statement.
//
// Two properties are load-bearing and easy to break:
//
//   - The baseline is never overwritten. `COALESCE(json_extract(metadata,
//     '$.challenged_prior_weight'), weight)` keeps the FIRST recorded prior
//     value, so a second challenge on an already-challenged memory still
//     restores to where the memory stood before the first one. Restoring the
//     first prior is the only baseline that means anything: any later value
//     already includes the first slash.
//
//   - A baseline is recorded only for a column the challenge actually
//     changes. The challenge surfaces never touch retrieval_priority, so
//     recording one there would claim a change that did not happen and would
//     let a restore clobber an unrelated later adjustment. Restore keys its
//     writes off what is present, so "absent" means "this challenge did not
//     move it" rather than "we lost it".
package internal

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
)

// ChallengeTable names the memories table a challenge reads or writes.
//
// It is a closed set rather than a free-form string because the value is
// interpolated into SQL text (SQLite cannot bind a schema-qualified table
// name as a parameter). applyChallengeStateInTx validates against this set
// before it builds a statement.
type ChallengeTable string

const (
	// ChallengeTableLocal is the manager's own `memories` table.
	ChallengeTableLocal ChallengeTable = "memories"

	// ChallengeTableShared is the ATTACHed federated `shared.memories`
	// table, where the Arc 1 resolution writers slash the loser.
	ChallengeTableShared ChallengeTable = "shared.memories"
)

// validChallengeTables is the allowlist mpm-lint's sql parameterization
// rule recognizes. Every call site that interpolates a ChallengeTable into
// SQL text must guard against an unknown value with:
//
//	if !validChallengeTables[table] { return err }
//
// The linter only looks for the IndexExpr pattern `validChallengeTables[table]`
// (and named equivalents) inside the enclosing function body — a `switch
// table { case ... }` is structurally equivalent but invisible to the
// classifier, so the allowlist form is the only one the gate accepts.
var validChallengeTables = map[ChallengeTable]bool{
	ChallengeTableLocal:  true,
	ChallengeTableShared: true,
}

// challengeTables returns the tables that exist for this manager, in the
// order a restore should prefer them: the local store first, because a
// memory id is authoritative there, and the shared store only when the
// shared DB is actually attached.
func (dm *DatabaseManager) challengeTables() []ChallengeTable {
	tables := []ChallengeTable{ChallengeTableLocal}
	if dm.SharedAttached() != "" {
		tables = append(tables, ChallengeTableShared)
	}
	return tables
}

// `challenged_via` values — which writer produced a challenge.
//
// Recorded by every writer so a restore, an audit query or an operator can
// tell a `--apply` slash from an operator arbitration from a plain theory
// challenge without inferring it from which optional keys happen to be
// present. The legacy rows that predate this key are still readable: their
// recovery metadata is what matters, and that is keyed off
// challenged_prior_*.
const (
	// ChallengeViaTheory is the ChallengeMemory family, including the
	// `mpm challenge` CLI and `mpm_memory action=challenge`. These link a
	// pending theory that a restore marks disproven.
	ChallengeViaTheory = "challenge_theory"

	// ChallengeViaResolution is applyResolution — `mpm ops
	// resolve-contradictions --apply` on a decisive-margin row.
	ChallengeViaResolution = "contradiction_resolution"

	// ChallengeViaArbitration is applyArbitrationResolution — the
	// operator closing a close-call theory.
	ChallengeViaArbitration = "operator_arbitration"
)

// ChallengeSpec describes one challenge application. It is deliberately
// declarative: the caller says what the challenge MEANS, and this file
// decides what SQL that means.
type ChallengeSpec struct {
	// MemoryID is the contested memory.
	MemoryID string

	// SlashAmount is a POSITIVE weight reduction. A negative value is
	// rejected rather than silently inverted into a weight boost —
	// challenging knowledge must never raise its rank.
	SlashAmount int

	// SlashRetrievalPriority additionally reduces retrieval_priority by
	// SlashAmount/100, floored at 0.01. Only the Arc 1 writers set this,
	// and only they record challenged_prior_retrieval_priority, so a
	// restore knows to put that column back.
	SlashRetrievalPriority bool

	// DropConfidence sets confidence to ChallengedMemoryConfidenceFloor.
	// Restoration must not silently re-promote verification, so a restore
	// resets the column to the floor rather than to the prior value; it
	// only does so for challenges that dropped it in the first place.
	DropConfidence bool

	// ResetAccessTimestamps zeroes the runtime decay accumulators, which
	// is what updateMemoryMetadataTx did for the metadata-only patch this
	// primitive replaces. Kept as an option because it is a behavioural
	// difference between the writers, not part of the challenge contract.
	ResetAccessTimestamps bool

	// ChallengedAt is the challenge timestamp, stored as Unix epoch
	// SECONDS. One representation across all writers, so an audit query
	// does not have to handle both an integer and an RFC3339 string. The
	// two Arc 1 writers previously wrote RFC3339 here; nothing reads the
	// value back, and every reader of a challenged row keys off
	// `status`, so unifying the representation changes no behaviour.
	ChallengedAt int64

	// Fields is the challenge metadata merged into the memory's JSON,
	// typically `status` plus whatever provenance this writer owns
	// (challenged_theory_id, challenged_by_resolution, challenged_via,
	// arbitration_theory). Keys are additive; a nil value removes a key
	// (RFC 7396).
	Fields map[string]interface{}
}

// ChallengeApplied reports what a challenge actually did to a row,
// including the baseline it recorded so a caller (or a test) can assert on
// the value rather than inferring it.
type ChallengeApplied struct {
	// Table is the memories table the challenge was applied to.
	Table ChallengeTable

	// RowsAffected is the observed RowsAffected of the single UPDATE.
	// Zero means the row was not there (or was deleted) when the write
	// landed; callers decide whether that is tolerable.
	RowsAffected int64

	// PriorWeight, PriorRetrievalPriority and PriorConfidence are the
	// baselines now on the row. They are computed with exactly the rule
	// the SQL applies, so a caller reporting them is reporting what was
	// written rather than re-deriving it.
	PriorWeight            float64
	PriorRetrievalPriority float64
	PriorConfidence        float64

	// BaselinePreserved reports that a baseline was ALREADY on the row
	// and the non-overwriting COALESCE kept it. False on a first
	// challenge. This is the observable form of "a repeated challenge did
	// not move the restoration baseline".
	BaselinePreserved bool
}

// applyChallengeStateInTx is the single challenge-state mutation used by
// every writer. It participates in the caller's transaction — it never
// begins, commits or rolls one back — so a challenge stays atomic with the
// other work its caller is doing (theory creation, evidence
// neutralisation, queue-row close, resolution-memory insert).
//
// A zero RowsAffected with a nil error means the row was not found in that
// table. It is not an error here: `applyResolution` deliberately tolerates a
// loser that has since been deleted, and turning its tolerance into a
// failure would change behaviour the Arc 1 tests pin.
func applyChallengeStateInTx(tx *sql.Tx, table ChallengeTable, spec ChallengeSpec) (ChallengeApplied, error) {
	out := ChallengeApplied{Table: table}

	if spec.MemoryID == "" {
		return out, fmt.Errorf("applyChallengeStateInTx: memory_id is required")
	}
	if spec.SlashAmount < 0 {
		return out, fmt.Errorf("applyChallengeStateInTx: slash amount must be >= 0 (got %d); a negative reduction would silently increase the challenged memory's weight", spec.SlashAmount)
	}
	if !validChallengeTables[table] {
		return out, fmt.Errorf("applyChallengeStateInTx: refusing unknown memories table %q", string(table))
	}

	// Observe the row first. This is what lets ChallengeApplied report the
	// baseline that was recorded, and what makes "the baseline already
	// existed" observable rather than inferred.
	var (
		weight, retrievalPriority, confidence float64
		existingPriorWeight                   sql.NullFloat64
	)
	err := tx.QueryRow(fmt.Sprintf(`
		SELECT COALESCE(weight, 1),
		       COALESCE(retrieval_priority, 0.5),
		       COALESCE(confidence, 0.5),
		       json_extract(metadata, '$.challenged_prior_weight')
		FROM %s
		WHERE id = ? AND deleted_at IS NULL`, table), spec.MemoryID).
		Scan(&weight, &retrievalPriority, &confidence, &existingPriorWeight)
	if err == sql.ErrNoRows {
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("applyChallengeStateInTx: read prior state for %s: %w", spec.MemoryID, err)
	}

	out.BaselinePreserved = existingPriorWeight.Valid
	out.PriorWeight = weight
	if existingPriorWeight.Valid {
		out.PriorWeight = existingPriorWeight.Float64
	}
	out.PriorRetrievalPriority = retrievalPriority
	out.PriorConfidence = confidence

	// Caller metadata, plus the one field every writer owns.
	fields := map[string]interface{}{}
	for k, v := range spec.Fields {
		fields[k] = v
	}
	fields["challenged_at"] = spec.ChallengedAt
	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		return out, fmt.Errorf("applyChallengeStateInTx: marshal challenge metadata: %w", err)
	}

	// The baselines are written by SQL rather than by Go so the
	// non-overwriting rule is enforced by the write itself, not by the
	// read that preceded it. The read above cannot be relied on to
	// exclude a concurrent writer: a deferred SQLite transaction that
	// takes a read snapshot and later tries to write against a snapshot
	// another connection has already advanced fails with SQLITE_BUSY
	// rather than clobbering — but the correct outcome should not depend
	// on that behaviour holding.
	//
	// A baseline is included only for a column this challenge moves.
	priorPairs := []string{
		"'challenged_prior_weight', COALESCE(json_extract(metadata, '$.challenged_prior_weight'), weight)",
	}
	if spec.SlashRetrievalPriority {
		priorPairs = append(priorPairs,
			"'challenged_prior_retrieval_priority', COALESCE(json_extract(metadata, '$.challenged_prior_retrieval_priority'), retrieval_priority)")
	}
	if spec.DropConfidence {
		priorPairs = append(priorPairs,
			"'challenged_prior_confidence', COALESCE(json_extract(metadata, '$.challenged_prior_confidence'), confidence)")
	}

	setClauses := []string{
		`metadata = json_patch(COALESCE(metadata,'{}'), json_patch(?, json_object(` + joinPriorPairs(priorPairs) + `)))`,
		`weight = MAX(1, weight - ?)`,
	}
	args := []interface{}{string(fieldsJSON), spec.SlashAmount}

	if spec.SlashRetrievalPriority {
		setClauses = append(setClauses, `retrieval_priority = MAX(0.01, retrieval_priority - ?)`)
		args = append(args, float64(spec.SlashAmount)/100.0)
	}
	if spec.DropConfidence {
		setClauses = append(setClauses, `confidence = ?`)
		args = append(args, ChallengedMemoryConfidenceFloor)
	}
	setClauses = append(setClauses,
		`updated_at = CAST(strftime('%s','now') AS INTEGER)`)
	if spec.ResetAccessTimestamps {
		setClauses = append(setClauses,
			`last_accessed_at = CAST(strftime('%s','now') AS INTEGER)`,
			`runtime_seconds_since_access = 0`,
			`runtime_last_accrued_at = CAST(strftime('%s','now') AS INTEGER)`)
	}

	args = append(args, spec.MemoryID)
	res, err := tx.Exec(fmt.Sprintf(`
		UPDATE %s
		SET %s
		WHERE id = ? AND deleted_at IS NULL`, table, joinPriorPairs(setClauses)), args...)
	if err != nil {
		return out, fmt.Errorf("applyChallengeStateInTx: challenge %s: %w", spec.MemoryID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return out, fmt.Errorf("applyChallengeStateInTx: challenge %s: cannot determine affected rows: %w", spec.MemoryID, err)
	}
	out.RowsAffected = affected
	return out, nil
}

// joinPriorPairs joins SQL fragments with a comma. It exists so the
// statement above reads as a list of clauses rather than a concatenation
// expression; the name is about the JSON-prior pairs it was written for.
func joinPriorPairs(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

// ApplyChallengeInTx is the exported entry point to the shared challenge
// primitive, for callers outside this package (the `mpm challenge` CLI
// handler, which builds its own transaction so that challenge application,
// evidence neutralisation and theory creation stay atomic together).
//
// It adds nothing to applyChallengeStateInTx and performs no transaction
// management of its own: the caller owns the transaction, and a rollback by
// the caller rolls this back with it.
func ApplyChallengeInTx(tx *sql.Tx, table ChallengeTable, spec ChallengeSpec) (ChallengeApplied, error) {
	return applyChallengeStateInTx(tx, table, spec)
}

// ChallengedMemory is the read-only projection used by the
// `mpm challenge list` CLI surface and the observational doctor check.
//
// What is in this struct is exactly what an operator needs to make a
// per-row adjudication: the identity, the current standing (weight,
// confidence), the baselines the prior challenge recorded (so a
// restore caller can see whether a restore would be a no-op on
// weight), which writer produced the challenge (so the operator
// knows the contract that the restore will follow), and whether
// the row is restorable through MPM (a value, not a derived
// computation, so a script can drive restoration directly).
//
// Nothing here is intended to be written. The struct is
// observational by design: a doctor report or CLI list that built
// a write path off it would be a layering violation.
//
// `PriorWeight` etc. are (value, present) pairs because the Arc 1
// writers do not record these columns — `mpm challenge restore`
// refuses such rows, and the listing must surface the same
// distinction so an operator does not waste time restoring a row
// that cannot be restored.
type ChallengedMemory struct {
	// Table names the memories table the row came from, in the
	// canonical ChallengeTable form. UI surfaces should display
	// Store (the human label) rather than Table.
	Table ChallengeTable

	// Store is the operator-facing label for Table: "local" or
	// "shared". Matches storeLabel in challenge_restore.go so
	// scripts that read both surfaces see consistent values.
	Store string

	// ID is the memory's primary key. The operator pipes this to
	// `mpm challenge restore <id>`.
	ID string

	// Collection is the memories row's `collection` column.
	// The audit-time observation in issue #20 records 40 rows in
	// `memories` and 1 in `decisions`; surfacing it lets an
	// operator see at a glance whether a row is a normal memory
	// or a higher-stakes artifact.
	Collection string

	// CurrentWeight is the row's current `weight` after the
	// challenge was applied. For 27 of the 41 live rows this is
	// exactly 1.0 because the `MAX(1, weight - slash)` clamp
	// hit the floor.
	CurrentWeight float64

	// CurrentConfidence is the row's current `confidence` after
	// the challenge. For all 41 live rows this is
	// ChallengedMemoryConfidenceFloor (0.5) because
	// `mpm challenge` (and every other challenge writer in the
	// current code) drops confidence to the floor.
	CurrentConfidence float64

	// PriorWeight is the value the restore path will reinstate
	// to the row's `weight` column, when present. Absent on
	// rows written by the Arc 1 writers — those rows are
	// unrestorable through MPM.
	PriorWeight        float64
	PriorWeightPresent bool

	// PriorConfidence is the value the challenge captured into
	// the audit trail. The restore path does NOT re-promote to
	// this value (F7.1: a restore is the memory half, not the
	// belief half), so this is forensic only. Surfaced for
	// operators who want to see what was lost.
	PriorConfidence        float64
	PriorConfidencePresent bool

	// PriorRetrievalPriority mirrors PriorWeight for the
	// retrieval_priority column. Recorded only by the Arc 1
	// writers (the local challenge paths do not touch
	// retrieval_priority), so the live 41 have it absent.
	PriorRetrievalPriority        float64
	PriorRetrievalPriorityPresent bool

	// ChallengedVia identifies which writer produced this
	// challenge: one of the ChallengeVia* constants
	// ("challenge_theory", "contradiction_resolution",
	// "operator_arbitration"). Empty when the metadata key
	// is absent (legacy rows written before the field existed).
	ChallengedVia string

	// ChallengedAt is the challenge timestamp as Unix epoch
	// SECONDS — the same representation every challenge writer
	// uses after the 2026-10-13 unification. The CLI surface
	// converts this to ISO8601; the core reports the raw value
	// so a script can sort or range-filter.
	ChallengedAt int64

	// TheoryID is the value of the metadata key
	// `challenged_theory_id`. Empty for challenges that were
	// never linked to a theory. The audit-time 41 carry
	// free-text values here (the retired auto-challenge
	// defect wrote human-readable reasons into this field
	// rather than a theory id).
	TheoryID string

	// TheoryReferenceShape classifies TheoryID by shape:
	// "identifier" (16 hex chars), "free_text" (anything
	// else), or "" (absent). The same classification
	// `classifyTheoryReference` (challenge_restore.go) uses
	// to label the theory half of a restore — a free_text
	// reference is the operator's signal that the theory
	// half will report not_found even on a successful
	// memory half.
	TheoryReferenceShape string

	// Restoreable reports whether `mpm challenge restore` can
	// reverse this challenge through MPM. Mirrors the
	// "refuse to write a half-restore" test in
	// RestoreMemoryFromChallenge: a row with no
	// challenged_prior_weight AND no
	// challenged_prior_retrieval_priority is unrestorable
	// (the pre-slash values were never recorded). Every
	// row in the audit-time 41 carries
	// challenged_prior_weight, so all 41 are restorable.
	Restoreable bool

	// RestoreWeightChange is `PriorWeight - CurrentWeight`
	// when PriorWeight is present. For 27 of the 41 audit-time
	// rows, PriorWeight is 1.0 and CurrentWeight is 1.0, so
	// restore is a no-op on weight (only the status flag
	// clears). For 4 rows it is 1.0, meaning restore would
	// return the row to weight 2.0. The operator needs this
	// number to know whether a restore is actually worth
	// running on a given row.
	//
	// RestoreWeightChangePresent distinguishes "the delta is
	// zero because both sides are 1.0" (a real fact about
	// the row) from "no prior weight was recorded, so the
	// delta is undefined" (an unrestorable row). Without
	// this flag, a row whose prior weight was never recorded
	// and a row clamped at 1.0 from prior 1.0 would both
	// report 0.0 — a misleadingly identical state for two
	// different rows. The CLI surface renders absent as
	// "[!] no prior" and the JSON envelope renders it as
	// null. The two states are operationally distinct and
	// must not collapse.
	RestoreWeightChange        float64
	RestoreWeightChangePresent bool
}

// ListChallengedMemories returns every row currently in
// `status='challenged'`, across both the local and shared memories
// tables (when the shared DB is attached). Ordered by challenged_at
// ascending, then id, so the audit-time residue reads top-down by
// chronology.
//
// The query is a plain SELECT, never a write. It uses the database
// handle the caller passes in, so callers that hold the read-only
// handle (the doctor service after MPM #28) get the observational
// contract for free: this function does not perform any mutations
// of its own, and the query plan is a single read against indexed
// columns.
//
// The function reads from BOTH tables so an operator that runs
// `mpm challenge list` against a manager with the shared DB
// attached sees the full population, not just the local slice. The
// audit-time 41 are all in the local table (the shared table is
// where the Arc 1 writers slashed, and those writers recorded no
// prior weight — so a future shared-table row would surface as
// `Restoreable == false` and `RestoreWeightChange == 0`).
func (dm *DatabaseManager) ListChallengedMemories() ([]ChallengedMemory, error) {
	if dm == nil {
		return nil, fmt.Errorf("ListChallengedMemories: nil database manager")
	}

	var out []ChallengedMemory
	for _, table := range dm.challengeTables() {
		rows, err := dm.queryChallengedRows(table)
		if err != nil {
			return nil, fmt.Errorf("ListChallengedMemories: read %s: %w", string(table), err)
		}
		out = append(out, rows...)
	}

	// Global chronological order across both tables. Each
	// queryChallengedRows call returns its own table sorted by
	// (challenged_at ASC, id ASC), but the two slices are
	// appended local-first, shared-second. Without a merge
	// step, a shared row at challenged_at=10 followed by a
	// local row at challenged_at=20 would land in that order
	// regardless of the actual chronology. The shared DB may
	// carry a small number of legacy Arc 1 rows interleaved
	// with the local residue, so the global sort is the only
	// way to make "oldest first" honest.
	//
	// Tie-breaker: local before shared on identical
	// (challenged_at, id). Local rows are the canonical
	// substrate; shared is the secondary attachment. The
	// id-already-breaks-ties rule means the tie-breaker only
	// matters across tables, and the local-first rule
	// matches the iteration order the rows came out of
	// `challengeTables()`.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ChallengedAt != out[j].ChallengedAt {
			return out[i].ChallengedAt < out[j].ChallengedAt
		}
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		// Identical (challenged_at, id) across tables is
		// not possible in practice (memory ids are unique
		// across the local and shared substrates), but the
		// rule is documented for completeness: local wins.
		return out[i].Store == "local"
	})
	return out, nil
}

// queryChallengedRows runs the actual SELECT for one table. Pulled
// out so the table-name interpolation is in one place and the
// table-allowlist check is unambiguous (the linter expects
// `validChallengeTables[table]` next to the format string, and
// inline that becomes hard to follow inside a loop).
func (dm *DatabaseManager) queryChallengedRows(table ChallengeTable) ([]ChallengedMemory, error) {
	if !validChallengeTables[table] {
		return nil, fmt.Errorf("queryChallengedRows: refusing unknown memories table %q", string(table))
	}

	// One SELECT per table, reading every column the projection
	// needs. The values that are stored as JSON metadata keys
	// (challenged_at, challenged_via, challenged_theory_id,
	// the three challenged_prior_* baselines) are pulled out
	// with json_extract so the caller's struct fields are
	// concrete Go values rather than raw JSON to re-parse.
	rows, err := dm.db.Query(fmt.Sprintf(`
		SELECT id,
		       collection,
		       COALESCE(weight, 1),
		       COALESCE(confidence, 0.5),
		       json_extract(metadata, '$.challenged_at'),
		       json_extract(metadata, '$.challenged_via'),
		       json_extract(metadata, '$.challenged_theory_id'),
		       json_extract(metadata, '$.challenged_prior_weight'),
		       json_extract(metadata, '$.challenged_prior_confidence'),
		       json_extract(metadata, '$.challenged_prior_retrieval_priority')
		FROM %s
		WHERE deleted_at IS NULL
		  AND json_extract(metadata, '$.status') = 'challenged'
		ORDER BY json_extract(metadata, '$.challenged_at') ASC, id ASC`, table))
	if err != nil {
		return nil, fmt.Errorf("queryChallengedRows: select from %s: %w", string(table), err)
	}
	defer rows.Close()

	var out []ChallengedMemory
	for rows.Next() {
		var (
			row          ChallengedMemory
			challengedAt sql.NullFloat64
			via          sql.NullString
			theoryID     sql.NullString
			priorWeight  sql.NullFloat64
			priorConf    sql.NullFloat64
			priorRP      sql.NullFloat64
		)
		if err := rows.Scan(
			&row.ID,
			&row.Collection,
			&row.CurrentWeight,
			&row.CurrentConfidence,
			&challengedAt,
			&via,
			&theoryID,
			&priorWeight,
			&priorConf,
			&priorRP,
		); err != nil {
			return nil, fmt.Errorf("queryChallengedRows: scan: %w", err)
		}

		row.Table = table
		row.Store = storeLabel(table)

		if challengedAt.Valid {
			row.ChallengedAt = int64(challengedAt.Float64)
		}
		if via.Valid {
			row.ChallengedVia = via.String
		}
		if theoryID.Valid {
			row.TheoryID = theoryID.String
			row.TheoryReferenceShape = classifyTheoryReference(theoryID.String)
		}

		// The three baselines are present-or-absent: any
		// NULL in json_extract means the challenge writer
		// did not record a value for that column. The
		// restore path keys off presence, and the listing
		// must surface the same distinction.
		if priorWeight.Valid {
			row.PriorWeight = priorWeight.Float64
			row.PriorWeightPresent = true
		}
		if priorConf.Valid {
			row.PriorConfidence = priorConf.Float64
			row.PriorConfidencePresent = true
		}
		if priorRP.Valid {
			row.PriorRetrievalPriority = priorRP.Float64
			row.PriorRetrievalPriorityPresent = true
		}

		// Restoreable mirrors the test in
		// RestoreMemoryFromChallenge: a row is restorable
		// if at least one of the two columns the restore
		// would actually move is recorded. The audit-time
		// 41 all carry challenged_prior_weight, so all
		// 41 are restorable.
		row.Restoreable = row.PriorWeightPresent || row.PriorRetrievalPriorityPresent

		// RestoreWeightChange is meaningful only when a
		// prior weight was actually recorded. When
		// PriorWeight is absent, the delta is undefined
		// — the row is unrestorable through MPM (the
		// restore path's "refuse to write a half-restore"
		// test fires), and reporting 0.0 would collapse
		// this row with a floor no-op row in the operator
		// display. RestoreWeightChangePresent is the
		// honest marker.
		if row.PriorWeightPresent {
			row.RestoreWeightChange = row.PriorWeight - row.CurrentWeight
			row.RestoreWeightChangePresent = true
		}

		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queryChallengedRows: iterate: %w", err)
	}
	return out, nil
}
