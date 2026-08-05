// Package capability implements the substrate side of the capability
// lifecycle subsystem.
//
// A capability is a stateful artifact (a script or snippet) that an agent
// proposes, the forge validates, and the runtime executes under `bwrap`.
// The lifecycle is enforced at three layers:
//
//   - Storage boundary: SQLite CHECK constraints on the state column
//     reject illegal values at insert/update time. See BaseTables in
//     internal/core/schema.go for the DDL.
//   - Type system: CapabilityState.CanTransitionTo() refuses illegal
//     transitions before any SQL is constructed. The transition matrix
//     here MUST stay in sync with the CHECK constraint and §1.3 of the
//     spec — adding a transition requires updating both.
//   - Forge pipeline: the validation sequence (§3.2 of the spec) is the
//     only path that creates the linted / validated / probation states;
//     every other state transition goes through the matrix.
//
// JSON columns (metadata, tags) implement sql.Scanner and driver.Valuer
// so handler code reads and writes Go-native types without any byte-slice
// marshaling boilerplate.
//
// Spec: docs/capability-lifecycle-spec.md
package capability
