// similarity_candidates_store.go — the ONLY path that persists candidates.
//
// Separation of concerns, stated plainly:
//
//   - similarity_candidates.go DISCOVERS candidates and writes nothing.
//   - this file PERSISTS them, and is reachable only from an explicitly
//     invoked operator command.
//
// Ordinary semantic retrieval calls neither. That is the whole guarantee:
// a read cannot add a row to the persistent queue, so a read cannot grow it
// without bound and a diagnostic command cannot leave a durable trace
// behind.
//
// The queue rows themselves are unchanged in shape. Existing
// shared.contradiction_log records are read, resolved and arbitrated exactly
// as before; nothing here migrates, reinterprets or deletes them. What
// changed is only WHO may add to the queue: not a retrieval, only an
// operator who asked.

package internal

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	sqlite3 "github.com/mattn/go-sqlite3"
)

// similarityCandidateSource is set on every row this file writes, so a
// reviewer can tell at a glance which queue items arrived from an explicit
// operator scan and which predate the split. Existing rows carry whatever
// detected_by they already had and are not touched.
const similarityCandidateSource = "similarity-scan"

// LoadSimilarityScanEntries reads the most recent live memories as scan
// inputs. `limit` caps the working set (0 = no cap); pairs are evaluated
// within that set only, so a smaller limit is a cheaper, shallower scan.
//
// A memory with no embedding, a literal 'null' vector, or a synthetic hash
// vector cannot be compared meaningfully and is excluded rather than
// silently scored.
//
// The embedding_model filter is the important one, and it is what every
// retrieval path already does (hybrid_search.go, vector_index.go, db.go all
// compare against CurrentEmbeddingFingerprint().Model). validEmbeddingPair
// only checks that two vectors are the same LENGTH, so two 768-dim vectors
// from different models sail through it and produce a confident cosine
// between unrelated spaces. That was survivable while one model was in use;
// it is not survivable mid-migration, when a scan run over a partially
// re-embedded store would compare all-minilm rows against nomic rows and
// propose pairs derived from a number that means nothing.
func (dm *DatabaseManager) LoadSimilarityScanEntries(limit int, schemaPrefix string) ([]ScanEntry, error) {
	if dm.db == nil {
		return nil, fmt.Errorf("LoadSimilarityScanEntries: dm.db is nil")
	}
	// schemaPrefix is interpolated into the statement, so it is validated the
	// same way both HybridSearch call sites validate theirs. The scan is a
	// WRITE path — it feeds EnqueueSimilarityCandidates — so an unvalidated
	// prefix here is a far worse problem than a bad read.
	if err := ValidateSchemaPrefix(schemaPrefix); err != nil {
		return nil, fmt.Errorf("LoadSimilarityScanEntries: %w", err)
	}
	// An empty active model means no provider is configured, so there is no
	// fingerprint to filter on. Selecting everything then is the honest
	// behaviour: the scan runs, the operator sees candidates, and the
	// absence of a provider is visible in `mpm config show` rather than
	// silently emptying the queue.
	fingerprint := CurrentEmbeddingFingerprint().Model

	q := fmt.Sprintf(`
		SELECT id, embedding, content, COALESCE(metadata, '{}')
		FROM %smemories
		WHERE deleted_at IS NULL
		  AND embedding IS NOT NULL
		  AND embedding != 'null'
		  AND embedding_source != 'hash'
	`, schemaPrefix)
	args := []any{}
	if fingerprint != "" {
		q += " AND embedding_model = ?"
		args = append(args, fingerprint)
	}
	q += " ORDER BY created_at DESC"
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}

	rows, err := dm.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("LoadSimilarityScanEntries: %w", err)
	}
	defer rows.Close()

	var out []ScanEntry
	for rows.Next() {
		var id, embStr, content, meta string
		if err := rows.Scan(&id, &embStr, &content, &meta); err != nil {
			return nil, fmt.Errorf("LoadSimilarityScanEntries: scan: %w", err)
		}
		var emb []float32
		if embStr == "" || json.Unmarshal([]byte(embStr), &emb) != nil || len(emb) == 0 {
			continue
		}
		out = append(out, ScanEntry{
			ID:           id,
			Content:      content,
			Embedding:    emb,
			IsChallenged: memoryIsChallenged(meta),
			MetadataJSON: meta,
		})
	}
	return out, rows.Err()
}

// LoadSimilarityScanEntriesFederated scans BOTH sides of the corpus — the
// local `memories` table and the ATTACHed `shared.memories` — and returns the
// union. This is the entry point the propose command uses.
//
// Scanning local only would be a silent under-scan on a shared-attached
// install. It is not a subtle one: hybridSearchScope (memory_tools.go:419)
// returns local ∪ shared to every reader, so the memories an operator sees on
// screen are exactly the memories a local-only scan would fail to compare.
// The queue those candidates are enqueued into — shared.contradiction_log —
// lives on the shared side regardless, so the scan and its destination were
// reading from different databases.
//
// The scan degrades to local-only when no shared DB is attached, matching
// hybridSearchScope: an unattached shared side is a normal configuration, not
// an error, and the operator gets a scan of what actually exists.
//
// limit applies PER SIDE, exactly as hybridSearchScope applies its limit per
// side. The union is therefore up to 2x limit. This is deliberate: the limit
// is a per-source cost bound, and a scan that silently saw only half the
// corpus would be worse than a wider one.
func (dm *DatabaseManager) LoadSimilarityScanEntriesFederated(limit int) ([]ScanEntry, error) {
	local, err := dm.LoadSimilarityScanEntries(limit, "")
	if err != nil {
		return nil, err
	}

	shared, err := dm.LoadSimilarityScanEntries(limit, "shared.")
	if err != nil {
		if isSharedUnavailable(err) {
			return local, nil
		}
		return nil, fmt.Errorf("LoadSimilarityScanEntriesFederated: %w", err)
	}

	// A memory can be promoted to shared and remain in local. Both copies are
	// the same claim with the same ID, and FindSimilarityCandidates skips only
	// index pairs (j > i) — it would compare the two copies against each other
	// and propose a memory as a contradiction of itself. Dedupe on ID.
	seen := make(map[string]struct{}, len(local)+len(shared))
	out := make([]ScanEntry, 0, len(local)+len(shared))
	for _, e := range local {
		seen[e.ID] = struct{}{}
		out = append(out, e)
	}
	for _, e := range shared {
		if _, dup := seen[e.ID]; dup {
			continue
		}
		seen[e.ID] = struct{}{}
		out = append(out, e)
	}
	return out, nil
}

// memoryIsChallenged reports whether the metadata carries the challenge flag.
// It READS pre-existing state and never sets it.
//
// The test is the same one the schema's own aggregate uses —
// json_extract(metadata, '$.status') = 'challenged' (schema.go) — rather
// than a substring search over the raw JSON.
//
// A substring search was wrong in four measured ways:
//
//	{"status":"challenged",...}                            -> true   (correct)
//	{"restored_from_challenge":true,"challenged_at":100}    -> true   (WRONG)
//	{"challenged_memory_id":"other"}                        -> true   (WRONG)
//	{"note":"we challenged the rollout last sprint"}        -> true   (WRONG)
//
// The restored case is the one that matters. challenge_restore.go nulls
// status and challenged_theory_id but deliberately KEEPS challenged_at and
// challenged_prior_weight as audit trail — so a substring match reads every
// restored memory as challenged forever. That mis-assigns the
// state_collision candidate kind and swaps MemoryA/MemoryB, so the pair an
// operator reviews names the wrong side.
func memoryIsChallenged(metadataJSON string) bool {
	if metadataJSON == "" {
		return false
	}
	var meta struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(metadataJSON), &meta); err != nil {
		return false
	}
	return meta.Status == "challenged"
}

// EnqueueSimilarityCandidates persists candidates to the shared queue.
//
// This is the explicit opt-in write. It is not called by HybridSearch, by
// recall, or by any diagnostic path.
//
// EnqueueContradictionReport is idempotent on the unresolved memory pair, so
// re-running a scan over the same corpus adds nothing the second time. That
// is what keeps a repeated operator scan from growing the queue without
// bound. Idempotency was briefly a claim without a mechanism: the table's
// uniqueness constraint includes a one-second-resolution `detected_at`, so
// OR IGNORE alone suppressed a duplicate only within the same second. See
// the pre-check in contradiction_log.go.
//
// Returns the number of rows ACTUALLY inserted. Candidates already queued
// are not counted, so a repeat run over unchanged data returns 0 rather than
// re-announcing last run's work as new. Callers show this number to an
// operator, and a number that counts writes it did not perform is worse than
// no number at all.
func (dm *DatabaseManager) EnqueueSimilarityCandidates(cands []SimilarityCandidate) (int, error) {
	if len(cands) == 0 {
		return 0, nil
	}
	inserted := 0
	for _, c := range cands {
		ev := ContradictionEvidence{
			MemoryA:    c.MemoryA,
			MemoryB:    c.MemoryB,
			Evidence:   c.Evidence,
			Similarity: c.Similarity,
			DetectedBy: similarityCandidateSource,
		}
		wrote, err := dm.EnqueueContradictionReport(ev)
		if err != nil {
			// Surface the shared-attachment failure rather than swallowing
			// it: a caller that asked for persistence and got silence would
			// reasonably conclude the queue was updated.
			if isSharedUnavailable(err) {
				return inserted, fmt.Errorf(
					"EnqueueSimilarityCandidates: shared database unavailable: %w", err)
			}
			return inserted, err
		}
		if wrote {
			inserted++
		}
	}
	return inserted, nil
}

// isSharedUnavailable reports whether err is the expected "no shared
// database attached" failure rather than a real write error.
func isSharedUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such table") ||
		strings.Contains(msg, "no such schema") ||
		strings.Contains(msg, "shared")
}

// CountContradictionQueue reports how many unresolved rows the queue holds.
// Used by the explicit review command to show before/after, and by tests
// that assert a retrieval wrote nothing.
//
// The count runs on a PINNED connection whose shared attachment has been
// verified, because the queue lives in the `shared` schema and SQLite
// attaches a database PER CONNECTION.
//
// The pool is not limited to one connection — db.go deliberately does not
// call SetMaxOpenConns(1), because a single-connection pool deadlocks
// against a concurrent transaction — and there is no connect hook that
// attaches `shared` to each new connection. The attachment established at
// startup therefore lives on ONE pooled connection. A pooled query can
// land on a different connection, find no `shared` schema, and fail with
// "no such table: shared.contradiction_log" even though the table exists.
//
// This is the same pinning the transactional writer uses
// (ensureSharedAttachedOnConn), applied to the read path so that the count
// and the write agree about where the queue is.
func (dm *DatabaseManager) CountContradictionQueue() (int, error) {
	if dm.db == nil {
		return 0, &ErrDatabaseUnavailable{}
	}
	ctx := context.Background()
	conn, err := dm.db.Conn(ctx)
	if err != nil {
		return 0, &ErrDatabaseUnavailable{Underlying: err}
	}
	defer conn.Close()
	if err := dm.ensureSharedAttachedOnConn(ctx, conn); err != nil {
		return 0, fmt.Errorf("CountContradictionQueue: %w", err)
	}
	var n int
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM shared.contradiction_log WHERE resolved_at IS NULL`).Scan(&n); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return n, nil
}

// ── transactional proposal insertion (issue #21) ───────────────────
//
// Everything below exists to fix one specific weakness in the write path
// above.
//
// EnqueueSimilarityCandidates issues, per candidate, a SELECT that counts
// unresolved rows for the pair and then an INSERT OR IGNORE. Between those
// two statements another writer can commit the same pair. Two concurrent
// proposals of the same fresh pair can therefore both insert, and the
// queue — which is a worklist a human reads — holds a duplicate the
// operator has to notice and dismiss.
//
// The schema cannot fix this on its own. The table's constraint is
//
//	UNIQUE(memory_id_a, memory_id_b, detected_at)
//
// and detected_at is CURRENT_TIMESTAMP, whose resolution is one second.
// The constraint is therefore a same-second duplicate suppressor, not an
// unresolved-pair uniqueness guarantee: two proposals 1.5 seconds apart
// have different detected_at values and are both accepted. The constraint
// also says nothing about resolved_at, so it cannot express "one
// unresolved row per pair" even in principle.
//
// The fix is to do the check and the writes inside one transaction that
// holds the write lock, so they are serialised against every other
// writer. That is what the code below does.

const (
	// DefaultWriteBudget is the default number of NEW queue rows a single
	// proposal may insert.
	//
	// It counts new insertions, not candidates found. A scan that found
	// 900 candidates of which 700 were already queued is 200 new
	// insertions and fits this budget comfortably.
	DefaultWriteBudget = 250

	// MaxWriteBudget is the hard ceiling on the write budget.
	//
	// The budget may be lowered by an operator who wants a narrower
	// insertion, but not raised. The number exists to bound how much one
	// invocation can put in front of a human reviewer, and a larger one
	// would not bound anything.
	MaxWriteBudget = 250
)

// CommitState describes what became of a proposal transaction.
//
// The distinction the whole type exists for: a transaction that was
// refused or rolled back wrote NOTHING, and a transaction that committed
// wrote rows that are durable. There is no third reading, and in
// particular an uncertain outcome must never be reported as a success.
type CommitState int

const (
	// CommitStateRefused: the transaction never started, or was refused
	// before any write. Nothing was persisted. Covers an over-budget
	// refusal and a lock that could not be acquired.
	CommitStateRefused CommitState = iota

	// CommitStateRolledBack: the transaction was aborted and the
	// ROLLBACK statement itself succeeded. The queue is provably
	// unchanged by this invocation.
	//
	// This state can follow a failed COMMIT as well as a failed INSERT.
	// A ROLLBACK that succeeds after a failed COMMIT is proof that the
	// commit did not take effect: SQLite refuses to roll back a
	// transaction that has already committed.
	CommitStateRolledBack

	// CommitStateUncertain: the outcome could not be established. Either
	// the COMMIT failed, or a ROLLBACK failed and the connection's state
	// is unknown. The operator must verify the queue before trusting it.
	CommitStateUncertain

	// CommitStateConfirmed: the COMMIT succeeded. Whatever the
	// TransactionResult reports as inserted is durable.
	CommitStateConfirmed
)

// String renders the commit state for the JSON envelope and for operator
// text. These are stable identifiers; a consumer may match on them.
func (s CommitState) String() string {
	switch s {
	case CommitStateRefused:
		return "refused"
	case CommitStateRolledBack:
		return "rolled_back"
	case CommitStateUncertain:
		return "uncertain"
	case CommitStateConfirmed:
		return "confirmed"
	default:
		return "unknown"
	}
}

// TransactionResult reports what a proposal transaction did.
//
// The counts here are of things that HAPPENED, not things that were
// attempted. Inserted counts rows that were committed; a rolled-back
// transaction reports zero inserted even though it issued the INSERT
// statements, because those statements left nothing behind.
type TransactionResult struct {
	// CommitState is the authoritative statement of what happened.
	CommitState CommitState

	// WroteTheNewRows is true only when the transaction committed AND at
	// least one row was inserted. A transaction that committed zero rows
	// (everything was already pending) is a SUCCESSFUL transaction that
	// wrote nothing — which is a different statement from "failed".
	WroteTheNewRows bool

	// Inserted counts CONFIRMED COMMITTED rows. It is zero unless
	// CommitState is CommitStateConfirmed.
	Inserted int

	// Eligible counts the canonical pairs that were new — not already
	// pending, not duplicated within the batch — and therefore would have
	// been inserted had the write budget allowed it.
	Eligible int

	// SkippedPending counts candidates whose pair was already an
	// unresolved row in the queue.
	SkippedPending int

	// SkippedDuplicate counts candidates whose canonical pair appeared
	// more than once in the batch.
	SkippedDuplicate int

	// SkippedConstrained counts eligible candidates whose INSERT was
	// suppressed by the table's own UNIQUE(memory_id_a, memory_id_b,
	// detected_at) constraint.
	//
	// This is a rare case and it is reported rather than hidden: the
	// constraint includes a one-second-resolution detected_at, so a pair
	// whose previous row was RESOLVED and detected within the same second
	// as the current proposal collides with it. The pair is not pending —
	// it is resolved — so the in-transaction pending check does not skip
	// it, and the constraint is what stops the re-queue.
	//
	// A count of zero differences matters: "eligible 1, inserted 0,
	// skipped_constrained 1" is an explanation, while "eligible 1,
	// inserted 0" is a puzzle.
	SkippedConstrained int

	// PendingUnresolved counts the unresolved rows observed in the queue
	// when the transaction took its snapshot.
	PendingUnresolved int

	// WriteBudget is the insertion budget that was in force.
	WriteBudget int

	// WriteBudgetHit is true when the eligible set exceeded the budget
	// and the transaction was refused.
	WriteBudgetHit bool

	// UnderlyingError is the underlying cause of a failure, preserved
	// through Unwrap so errors.Is and errors.As keep working.
	UnderlyingError error

	// RollbackError is set ONLY when the ROLLBACK statement itself
	// failed. Its presence is what distinguishes "we know nothing was
	// written" from "we cannot prove it".
	RollbackError error

	// Busy is true when the failure was SQLite's lock contention
	// (SQLITE_BUSY / SQLITE_LOCKED) rather than anything else.
	Busy bool
}

// ErrWriteBudgetExceeded is returned when the eligible set is larger than
// the authorised write budget.
//
// The transaction is refused outright. Inserting the first N rows and
// stopping would report a number that does not describe what the scan
// found, and would leave the queue in a state that depends on where the
// truncation happened to fall.
type ErrWriteBudgetExceeded struct {
	// Eligible is the number of new canonical pairs the batch contained.
	Eligible int

	// WriteBudget is the budget that was exceeded.
	WriteBudget int
}

func (e *ErrWriteBudgetExceeded) Error() string {
	return fmt.Sprintf("proposal refused: %d new candidate pairs exceed the write budget of %d. "+
		"No queue rows were written. Raise --write-budget (up to %d) or narrow the scan.",
		e.Eligible, e.WriteBudget, DefaultWriteBudget)
}

// ErrInvalidWriteBudget is returned when the write budget is not positive.
//
// This is a different failure from ErrWriteBudgetExceeded and means a
// different thing to the operator. ErrWriteBudgetExceeded says "there was
// more to write than you authorised"; this says "the authorisation you
// supplied is not an authorisation at all". The batch is never measured
// against a budget of zero — reporting "0 new candidate pairs exceed the
// write budget of 0" describes nothing — so the refusal reports the
// budget that was supplied and how many candidates were offered against it.
type ErrInvalidWriteBudget struct {
	// WriteBudget is the budget that was supplied.
	WriteBudget int

	// Candidates is how many candidates were offered for insertion.
	Candidates int
}

func (e *ErrInvalidWriteBudget) Error() string {
	return fmt.Sprintf("proposal refused: the write budget must be positive (got %d), but %d candidate(s) "+
		"were offered. No queue rows were written. Raise --write-budget (up to %d).",
		e.WriteBudget, e.Candidates, DefaultWriteBudget)
}

// ErrSharedUnavailable is returned when the shared database cannot be
// reached.
//
// It is a distinct type because it is a CONFIGURATION fact rather than a
// write failure: the contradiction queue lives in the `shared` schema, so
// an install with no shared database has no queue to read or write. Nothing
// was attempted, and no amount of retrying will change that.
//
// It exists so callers can tell "the queue is not available" from "the
// queue is available and the write failed" — two very different operator
// actions.
type ErrSharedUnavailable struct {
	// Path is the configured shared database path. It is empty when no
	// shared database is configured at all.
	Path string

	// Underlying is the driver error, when the failure was an attach
	// attempt rather than a missing configuration.
	Underlying error
}

func (e *ErrSharedUnavailable) Error() string {
	if e.Path == "" {
		return "the shared database is not attached; the contradiction queue lives in the shared schema " +
			"and cannot be read or written. Set MPM_SHARED_DB and initialise the shared database."
	}
	return fmt.Sprintf("the shared database (%s) could not be attached; the contradiction queue lives in "+
		"the shared schema and cannot be read or written: %v", e.Path, e.Underlying)
}

// Unwrap exposes the driver error so callers can match on it.
func (e *ErrSharedUnavailable) Unwrap() error { return e.Underlying }

// ErrDatabaseUnavailable is returned when the database itself could not be
// reached: the manager holds no open handle, or no connection could be
// acquired from the pool.
//
// It is distinct from ErrSharedUnavailable, which says the shared schema
// could not be attached. The first is a database that is not open; the
// second is an attachment that is missing. Both are raised BEFORE any
// write is attempted, so both mean the queue was left untouched.
type ErrDatabaseUnavailable struct {
	// Underlying is the driver error, when there was one.
	Underlying error
}

func (e *ErrDatabaseUnavailable) Error() string {
	if e.Underlying == nil {
		return "the database could not be reached: the manager holds no open handle"
	}
	return fmt.Sprintf("the database could not be reached: %v", e.Underlying)
}

// Unwrap exposes the driver error so callers can match on it.
func (e *ErrDatabaseUnavailable) Unwrap() error { return e.Underlying }

// ErrProposalInsertion is returned when the batch could not be written or
// committed.
//
// Phase says which statement failed, so the operator can tell an insert
// failure from a commit failure. The two have different consequences: a
// confirmed rollback after an insert failure means the queue is provably
// unchanged, whereas a failed commit means the commit phase did not
// complete and the queue must be verified.
//
// The message deliberately does NOT claim that a rollback happened.
//
// At the point this error is constructed the rollback has not run yet: the
// rollback is attempted by the deferred cleanup, which runs after this
// error is built. A message that said "was rolled back" would therefore
// assert something that was not yet true — and would be wrong outright in
// the case that matters most, where the ROLLBACK itself fails and the
// outcome is unknown.
//
// The message points at TransactionResult.CommitState instead, which is
// the authoritative statement of what happened: a confirmed rollback means
// nothing was written, and an outcome that could not be established means
// the queue must be verified before it is trusted.
type ErrProposalInsertion struct {
	// Phase is "insert" or "commit".
	Phase string

	// Underlying is the driver error, preserved for errors.Is/As.
	Underlying error
}

func (e *ErrProposalInsertion) Error() string {
	// The tail is identical for both phases because it describes the
	// STATE rather than the phase, and the state is not known yet.
	const stateNote = " The batch's fate is reported by the transaction's commit state: " +
		"a confirmed rollback means nothing was written, and an outcome that could not be " +
		"established means the queue must be verified before it is trusted."
	switch e.Phase {
	case "commit":
		return fmt.Sprintf("proposal commit failed: %v.%s", e.Underlying, stateNote)
	default:
		return fmt.Sprintf("proposal insertion failed: %v.%s", e.Underlying, stateNote)
	}
}

// Unwrap exposes the driver error so callers can match on it.
func (e *ErrProposalInsertion) Unwrap() error { return e.Underlying }

// ErrDatabaseBusy is returned when the write lock could not be acquired
// within the connection's busy timeout.
//
// It is a distinct type because it is a transient, retryable condition
// rather than a defect: another writer held the lock for the whole
// 5-second window. Nothing was written.
type ErrDatabaseBusy struct {
	// Underlying is the driver error.
	Underlying error
}

func (e *ErrDatabaseBusy) Error() string {
	return fmt.Sprintf("the database is locked by another writer; the proposal was refused and no "+
		"queue rows were written. Retry once the other writer has finished: %v", e.Underlying)
}

// Unwrap exposes the driver error so callers can match on it.
func (e *ErrDatabaseBusy) Unwrap() error { return e.Underlying }

// ErrInvalidCandidate is returned when a candidate cannot be validated.
//
// The producer of a candidate is internal code, so a malformed one is a
// bug — and a bug that silently drops rows is worse than one that stops
// the batch. The transaction is refused and nothing is written.
type ErrInvalidCandidate struct {
	// Candidate describes the offending pair.
	MemoryA string
	MemoryB string

	// Underlying is the validation error.
	Underlying error
}

func (e *ErrInvalidCandidate) Error() string {
	return fmt.Sprintf("proposal refused: candidate pair (%q, %q) is not valid: %v",
		e.MemoryA, e.MemoryB, e.Underlying)
}

// Unwrap exposes the validation error.
func (e *ErrInvalidCandidate) Unwrap() error { return e.Underlying }

// contradictionPairKey is the canonical identity of a memory pair in the
// queue.
//
// Canonical means lexicographically sorted, which is exactly the form the
// queue stores. This matters because the candidate layer does NOT always
// produce sorted pairs: classifyPair's state_collision branch deliberately
// puts the flagged memory first so the operator sees the row that already
// carries state. Two candidates naming the same pair from opposite
// directions therefore arrive in different orders and must be collapsed
// here, before any budget is calculated.
type contradictionPairKey struct {
	a string
	b string
}

// contradictionPairKeyFor returns the canonical key for a pair.
func contradictionPairKeyFor(a, b string) contradictionPairKey {
	if a > b {
		a, b = b, a
	}
	return contradictionPairKey{a: a, b: b}
}

// proposalTxFaults is a test seam for the two transaction-control failures
// SQLite offers no way to induce on demand.
//
// A COMMIT can fail in production (a full disk, a broken connection, a
// checkpoint that cannot complete) and a ROLLBACK can fail (the connection
// has already lost its transaction). Both have reporting consequences that
// must be pinned: the difference between "we know nothing was written" and
// "we cannot prove it" is the difference between an operator looking at the
// queue and an operator rebuilding it.
//
// The seam substitutes the failing STATEMENT, not the code around it. The
// reporting logic, the deferred safety net, the connection-discard path and
// the error construction are all the production ones.
//
// The default values execute the real statements, so production behaviour is
// unchanged.
var proposalTxFaults struct {
	mu       sync.Mutex
	commit   func(ctx context.Context, conn *sql.Conn) error
	rollback func(ctx context.Context, conn *sql.Conn) error
}

// SetProposalTxFaultsForTest installs the seam. Passing nil for either
// function restores the real statement.
//
// It is EXPORTED so that the CLI layer can drive the same fault injection
// the core tests use. The classification of a failed commit and a failed
// rollback is a CLI contract as much as a core one: the operator reads the
// JSON envelope and the human summary, and both have to agree with what
// the transaction actually did. Testing that end to end needs the fault to
// be injectable from outside the package.
//
// Test-only. Production behaviour is unchanged.
func SetProposalTxFaultsForTest(commit, rollback func(ctx context.Context, conn *sql.Conn) error) {
	proposalTxFaults.mu.Lock()
	defer proposalTxFaults.mu.Unlock()
	proposalTxFaults.commit = commit
	proposalTxFaults.rollback = rollback
}

// proposalCommit ends the transaction.
func proposalCommit(ctx context.Context, conn *sql.Conn) error {
	proposalTxFaults.mu.Lock()
	commit := proposalTxFaults.commit
	proposalTxFaults.mu.Unlock()
	if commit != nil {
		return commit(ctx, conn)
	}
	_, err := conn.ExecContext(ctx, "COMMIT")
	return err
}

// proposalRollback aborts the transaction.
func proposalRollback(ctx context.Context, conn *sql.Conn) error {
	proposalTxFaults.mu.Lock()
	rollback := proposalTxFaults.rollback
	proposalTxFaults.mu.Unlock()
	if rollback != nil {
		return rollback(ctx, conn)
	}
	_, err := conn.ExecContext(ctx, "ROLLBACK")
	return err
}

// isSQLiteBusyError reports whether err is SQLite lock contention.
//
// The driver reports SQLITE_BUSY and SQLITE_LOCKED as distinct codes; both
// mean "another writer holds the lock", and both are retryable rather than
// defects.
//
// Both the value form and the pointer form of the driver's error type are
// matched. mattn/go-sqlite3 declares `func (err Error) Error() string` — a
// VALUE receiver — and hands back `sqlite3.Error` values, so a chain that
// holds only `*sqlite3.Error` would never match and a lock contention would
// be reported as an unknown database failure instead of the transient
// condition it is.
func isSQLiteBusyError(err error) bool {
	if err == nil {
		return false
	}
	var valueErr sqlite3.Error
	if errors.As(err, &valueErr) {
		return valueErr.Code == sqlite3.ErrBusy || valueErr.Code == sqlite3.ErrLocked
	}
	var ptrErr *sqlite3.Error
	if errors.As(err, &ptrErr) {
		return ptrErr.Code == sqlite3.ErrBusy || ptrErr.Code == sqlite3.ErrLocked
	}
	return false
}

// ensureSharedAttachedOnConn makes sure the `shared` schema is attached on
// THIS connection.
//
// SQLite's ATTACH is per-connection, not per-database. The production
// attach path (DatabaseManager.attachShared) issues ATTACH through the
// pooled *sql.DB, so the attachment lands on whichever pooled connection
// happened to serve that Exec. A transaction on a DIFFERENT pooled
// connection would then find no `shared` schema at all, and every
// qualified statement against it would fail with "no such table".
//
// The pool has no SetMaxOpenConns(1) — deliberately, because a single
// connection deadlocks against a concurrent transaction (see the note in
// db.go). So this cannot be assumed away. The check is explicit, per
// connection, and the attachment is established here if it is missing.
//
// This must be called BEFORE BEGIN IMMEDIATE: SQLite forbids ATTACH
// inside a transaction.
func (dm *DatabaseManager) ensureSharedAttachedOnConn(ctx context.Context, conn *sql.Conn) error {
	if dm.sharedPath == "" {
		return &ErrSharedUnavailable{}
	}

	rows, err := conn.QueryContext(ctx, "PRAGMA database_list")
	if err != nil {
		return fmt.Errorf("inspect attached databases: %w", err)
	}
	attached := false
	for rows.Next() {
		var seq int
		var name, file string
		if err := rows.Scan(&seq, &name, &file); err != nil {
			rows.Close()
			return fmt.Errorf("inspect attached databases: scan: %w", err)
		}
		if name == "shared" {
			attached = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("inspect attached databases: %w", err)
	}
	rows.Close()
	if attached {
		return nil
	}

	stmt := fmt.Sprintf("ATTACH DATABASE '%s' AS shared", strings.ReplaceAll(dm.sharedPath, "'", "''"))
	if _, err := conn.ExecContext(ctx, stmt); err != nil {
		return &ErrSharedUnavailable{Path: dm.sharedPath, Underlying: err}
	}
	return nil
}

// discardConn poisons a pooled connection so it is not reused.
//
// Used when a ROLLBACK fails: the connection may still hold an open
// transaction, and handing that to the next borrower would give them a
// transaction they did not start. Returning driver.ErrBadConn from Raw
// tells database/sql to close the underlying connection instead of
// returning it to the pool.
func discardConn(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
}

// loadUnresolvedPairsTx reads the set of unresolved memory pairs.
//
// It must run inside the acquired transaction, so the snapshot it returns
// reflects every write that committed before this transaction took the
// write lock. Reading it outside the transaction would reintroduce the
// race the transaction exists to close.
//
// The pairs are canonicalised on read as well as on write: rows written
// before the normalisation existed may not be in sorted order, and a
// mismatch here would let a duplicate through.
func loadUnresolvedPairsTx(ctx context.Context, conn *sql.Conn) (map[contradictionPairKey]struct{}, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT memory_id_a, memory_id_b
		FROM shared.contradiction_log
		WHERE resolved_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("read unresolved queue rows: %w", err)
	}
	defer rows.Close()

	out := make(map[contradictionPairKey]struct{})
	for rows.Next() {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			return nil, fmt.Errorf("read unresolved queue rows: scan: %w", err)
		}
		out[contradictionPairKeyFor(a, b)] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read unresolved queue rows: %w", err)
	}
	return out, nil
}

// EnqueueSimilarityCandidatesTransactional persists candidates to the
// shared queue as ONE atomic batch, under a write budget.
//
// The guarantee it provides over EnqueueSimilarityCandidates is
// all-or-nothing: either every eligible pair in the batch is committed, or
// none of them is. There is no state in which half the batch is in the
// queue.
//
// # How the serialisation works
//
// The transaction is opened with BEGIN IMMEDIATE on a pinned *sql.Conn.
//
// BEGIN IMMEDIATE takes SQLite's write lock at transaction start rather
// than at first write. A deferred BEGIN would let two concurrent
// transactions both take a read snapshot, both see the pair as absent, and
// both insert — which is precisely the duplicate this function exists to
// prevent. With BEGIN IMMEDIATE the second transaction blocks on the lock
// until the first commits, and then sees the committed row.
//
// The lock is held for the busy_timeout window (5,000 ms, established in
// the connection DSN and re-asserted per connection at open). If it cannot
// be acquired in that window, SQLite returns SQLITE_BUSY and this function
// refuses with ErrDatabaseBusy having written nothing.
//
// # What is counted
//
// Inserted counts CONFIRMED COMMITTED rows. A rolled-back transaction
// reports zero, however many INSERT statements it issued.
//
// # The write budget
//
// The eligible set — canonical pairs that are neither already pending nor
// duplicated within the batch — is calculated inside the transaction,
// after the unresolved-pair snapshot is taken. If it exceeds the budget the
// transaction is refused and NOTHING is written.
//
// The budget counts NEW insertions, not candidates found. A scan that
// found 900 candidates of which 700 were already queued is 200 new
// insertions and fits a budget of 250.
func (dm *DatabaseManager) EnqueueSimilarityCandidatesTransactional(
	ctx context.Context,
	cands []SimilarityCandidate,
	writeBudget int,
) (res TransactionResult, err error) {
	res.CommitState = CommitStateRefused
	res.WriteBudget = writeBudget

	if dm.db == nil {
		err = &ErrDatabaseUnavailable{}
		res.UnderlyingError = err
		return res, err
	}
	if writeBudget <= 0 {
		// A non-positive budget is not a tight budget, it is a missing
		// one. It is reported as an invalid budget rather than as an
		// over-budget batch: the batch was never measured against a
		// budget of zero, so a count of "pairs that exceed the budget of
		// zero" would describe nothing at all.
		err = &ErrInvalidWriteBudget{WriteBudget: writeBudget, Candidates: len(cands)}
		res.UnderlyingError = err
		res.WriteBudgetHit = true
		return res, err
	}
	if len(cands) == 0 {
		// Nothing to do, and nothing that can fail. This is a successful
		// transaction that wrote no rows.
		res.CommitState = CommitStateConfirmed
		return res, nil
	}

	conn, err := dm.db.Conn(ctx)
	if err != nil {
		err = &ErrDatabaseUnavailable{Underlying: err}
		res.UnderlyingError = err
		return res, err
	}
	defer conn.Close()

	if err := dm.ensureSharedAttachedOnConn(ctx, conn); err != nil {
		res.UnderlyingError = err
		return res, err
	}

	// Acquire the write lock. Everything after this point is serialised
	// against every other writer in the process and in every other
	// process sharing this database file.
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		res.UnderlyingError = err
		res.Busy = isSQLiteBusyError(err)
		if res.Busy {
			return res, &ErrDatabaseBusy{Underlying: err}
		}
		return res, err
	}

	// The transaction is open. Every exit path from here must close it:
	// either COMMIT succeeds, or ROLLBACK is attempted. The named return
	// values let the deferred function report what it managed to do.
	//
	// The deferred function is also what decides the state after a failed
	// COMMIT. A COMMIT that fails leaves the transaction open, and a
	// successful ROLLBACK after it is PROOF that the commit did not take
	// effect — SQLite would have refused the ROLLBACK with "no transaction
	// is active" had the commit succeeded. A ROLLBACK that fails leaves the
	// outcome genuinely unknown. The two are different facts and the
	// operator is told which one applies.
	txOpen := true
	// refused records that the transaction was closed deliberately rather
	// than by a failure. The distinction survives the rollback below: a
	// refusal that had to roll back its (empty) transaction is still a
	// REFUSAL, and reporting it as "rolled back" would tell the operator
	// that an attempt was made and undone — which is not what happened.
	refused := false
	defer func() {
		if !txOpen {
			return
		}
		// The transaction did not commit, so nothing it inserted is
		// durable. The counts that describe the INSERT loop are therefore
		// zeroed: they describe statements that were issued, not rows that
		// exist. The counts that describe the BATCH — eligible, skipped
		// pending, skipped duplicate — are facts about the batch and the
		// snapshot taken inside the transaction, and they stay as they are.
		res.Inserted = 0
		res.SkippedConstrained = 0
		res.WroteTheNewRows = false
		// Use a context that is not cancelled by the caller's: a
		// cancellation must not prevent the transaction from being closed.
		if rbErr := proposalRollback(context.WithoutCancel(ctx), conn); rbErr != nil {
			res.RollbackError = rbErr
			res.CommitState = CommitStateUncertain
			// The connection may still hold an open transaction. Do not
			// hand it back to the pool.
			discardConn(conn)
		} else if refused {
			res.CommitState = CommitStateRefused
		} else {
			res.CommitState = CommitStateRolledBack
		}
	}()

	// Snapshot the unresolved pairs INSIDE the transaction.
	pending, err := loadUnresolvedPairsTx(ctx, conn)
	if err != nil {
		res.UnderlyingError = err
		return res, err
	}
	res.PendingUnresolved = len(pending)

	// Canonicalise, validate, and deduplicate the candidates.
	type eligibleCandidate struct {
		ev       ContradictionEvidence
		evidence string
	}
	eligible := make([]eligibleCandidate, 0, len(cands))
	seen := make(map[contradictionPairKey]struct{}, len(cands))

	for _, c := range cands {
		ev := ContradictionEvidence{
			MemoryA:    c.MemoryA,
			MemoryB:    c.MemoryB,
			Evidence:   c.Evidence,
			Similarity: c.Similarity,
			DetectedBy: similarityCandidateSource,
		}
		// The same validation the original writer performs. Sharing the
		// helper is what keeps the two paths from drifting: a producer
		// cannot get a forged adjudication segment, a self-pair, or an
		// empty ID past one path and not the other.
		if verr := validateAndNormaliseContradictionEvidence(&ev); verr != nil {
			invalid := &ErrInvalidCandidate{MemoryA: c.MemoryA, MemoryB: c.MemoryB, Underlying: verr}
			res.UnderlyingError = invalid
			// Refused, not failed: nothing was written and nothing was
			// attempted. The deferred function still closes the
			// transaction, but what it reports is a refusal — an attempt
			// that was made and undone is not what happened.
			refused = true
			return res, invalid
		}

		key := contradictionPairKeyFor(ev.MemoryA, ev.MemoryB)
		if _, dup := seen[key]; dup {
			res.SkippedDuplicate++
			continue
		}
		seen[key] = struct{}{}

		if _, isPending := pending[key]; isPending {
			res.SkippedPending++
			continue
		}

		// Marshal AFTER validation: the evidence JSON must record the
		// canonical pair order and the stripped provenance, not what the
		// producer supplied.
		evJSON, merr := json.Marshal(ev)
		if merr != nil {
			// Evidence that cannot be serialised is an unusable
			// candidate, and an unusable candidate refuses the batch. It
			// is reported as such rather than as a database failure: the
			// database was never asked to write anything.
			invalid := &ErrInvalidCandidate{
				MemoryA:    ev.MemoryA,
				MemoryB:    ev.MemoryB,
				Underlying: fmt.Errorf("marshal contradiction evidence: %w", merr),
			}
			res.UnderlyingError = invalid
			refused = true
			return res, invalid
		}
		eligible = append(eligible, eligibleCandidate{ev: ev, evidence: string(evJSON)})
	}

	res.Eligible = len(eligible)

	// Refuse an over-budget batch BEFORE the first INSERT.
	if len(eligible) > writeBudget {
		err := &ErrWriteBudgetExceeded{Eligible: len(eligible), WriteBudget: writeBudget}
		res.UnderlyingError = err
		res.WriteBudgetHit = true
		refused = true
		return res, err
	}

	// Insert the whole eligible batch.
	//
	// INSERT OR IGNORE, not a bare INSERT: the table's own UNIQUE
	// constraint can still suppress a row that the pending check let
	// through — a pair whose previous row was RESOLVED and detected within
	// the same second as this proposal. A bare INSERT would abort the
	// whole batch on that collision; ignoring it keeps the batch atomic
	// and honest, and the suppression is counted in SkippedConstrained so
	// the report explains itself.
	inserted := 0
	for _, e := range eligible {
		execRes, ierr := conn.ExecContext(ctx, `
			INSERT OR IGNORE INTO shared.contradiction_log
				(memory_id_a, memory_id_b, evidence, similarity, detected_by, detected_at)
			VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`,
			e.ev.MemoryA, e.ev.MemoryB, e.evidence, e.ev.Similarity, e.ev.DetectedBy)
		if ierr != nil {
			insErr := &ErrProposalInsertion{Phase: "insert", Underlying: ierr}
			res.UnderlyingError = insErr
			return res, insErr
		}
		n, aerr := execRes.RowsAffected()
		if aerr != nil {
			insErr := &ErrProposalInsertion{Phase: "insert", Underlying: aerr}
			res.UnderlyingError = insErr
			return res, insErr
		}
		if n > 0 {
			inserted++
		} else {
			res.SkippedConstrained++
		}
	}

	// Commit.
	if cerr := proposalCommit(ctx, conn); cerr != nil {
		// The commit failed. SQLite's COMMIT is atomic, so a commit that
		// returns an error has not committed — but the failure modes
		// (a broken connection, a lost transaction) are not all equally
		// diagnosable from here.
		//
		// The deferred function performs the ROLLBACK and decides the final
		// state: a successful ROLLBACK proves the batch was not written
		// (CommitStateRolledBack), a failed one leaves the outcome unknown
		// (CommitStateUncertain). Either way the transaction is NOT
		// reported as confirmed, and the error says the outcome must be
		// verified.
		commitErr := &ErrProposalInsertion{Phase: "commit", Underlying: cerr}
		res.UnderlyingError = commitErr
		return res, commitErr
	}

	txOpen = false
	res.CommitState = CommitStateConfirmed
	res.Inserted = inserted
	res.WroteTheNewRows = inserted > 0
	return res, nil
}
