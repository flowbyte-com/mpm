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
	switch table {
	case ChallengeTableLocal, ChallengeTableShared:
	default:
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
		WHERE id = ? AND deleted_at IS NULL`, string(table)), spec.MemoryID).
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
		WHERE id = ? AND deleted_at IS NULL`, string(table), joinPriorPairs(setClauses)), args...)
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
