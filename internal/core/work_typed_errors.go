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
