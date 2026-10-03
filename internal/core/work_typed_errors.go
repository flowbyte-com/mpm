// work_typed_errors.go — typed error sentinels for the work state
// machine. RUNTIME OUTCOME WIRING (2026-09-21) introduced these
// so the runtime classification seam (internal.ClassifyError) can
// recognise state-machine conflicts via errors.Is rather than
// parsing message text — addresses brief §3 priority "typed error
// first; string-fallback last."
//
// Usage: in the rare site that surfaces this condition, wrap with
// fmt.Errorf("%w: …", ErrInvalidWorkTransition, …). The human
// message stays informative; errors.Is(err, ErrInvalidWorkTransition)
// resolves to TRUE; internal.ClassifyError maps to OutcomeClassConflict.

package internal

import "errors"

// ErrInvalidWorkTransition is returned by work.UpdateWorkStatus /
// work.AppendWorkEvent when an attempt is made to transition a work
// item into a state that the closed lifecycle forbids (e.g. done → done).
//
// Recognised by internal.ClassifyError (returns OutcomeClassConflict,
// code "state_transition_invalid").
var ErrInvalidWorkTransition = errors.New("work state machine: invalid transition")

// ErrWorkNotTerminal is returned by ArchiveWork when the target work
// item is still `open`. Archive is a visibility operation on finished
// work; letting an open item leave the operational view would hide an
// unfinished commitment with no signal (§1.3 of
// docs/archive/2026-09-30-work-archive-and-purge.md).
//
// Classified as a conflict by internal.ClassifyError, same as
// ErrInvalidWorkTransition: the caller's request is well-formed, the
// substrate state is what conflicts.
var ErrWorkNotTerminal = errors.New("work archive: work item is not in a terminal state (open items cannot be archived)")

// ErrWorkNotArchived is returned by UnarchiveWork when the target work
// item is not currently archived. Unarchiving an already-active item
// is a caller error, not a no-op success: unlike archive (which is
// idempotent), silently succeeding would make a mistyped work id
// indistinguishable from a correct one.
//
// The substrate never lies about whether a write actually happened —
// same contract as SoftDeleteMemory.
var ErrWorkNotArchived = errors.New("work unarchive: work item is not archived")
