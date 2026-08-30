// session_hint.go — alpha-4 W-006: single source of truth for the
// "session_id is required" error message. The previous shape was a
// bare `fmt.Errorf("session_id is required")` scattered across every
// tool surface — the agent had to guess where to find one. The new
// hint names the two recovery paths an agent has:
//
//  1. `mpm_context read_wake_context` returns the current session id
//     as `session_current_id` (use that, or pass it through).
//  2. Set `MPM_SESSION_ID` env var if you're in a context that already
//     has a session but the tool can't see it.
//
// Both paths route through ErrSessionIDRequired.

package internal

import "fmt"

// ErrSessionIDRequired returns the canonical "session_id is required"
// error with a recovery hint. Used by every tool that requires
// session_id so the agent has a single, nameable place to look.
func ErrSessionIDRequired() error {
	return fmt.Errorf("session_id is required; pass the session_id from mpm_context read_wake_context (session_current_id) or set MPM_SESSION_ID env var")
}
