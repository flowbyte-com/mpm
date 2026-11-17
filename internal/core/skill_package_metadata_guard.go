// skill_package_metadata_guard.go — the package's METADATA contract.
//
// WHY THIS EXISTS
// ----------------
// A packaged skill's projection row carries a small set of metadata keys
// that are not decoration. They are the row's claims about the package:
//
//   - `version`      — the package's effective version. The package
//                      verification reads it and compares it with the
//                      package row's version and with the version the
//                      skill id names. A row whose `version` disagrees
//                      with its package row FAILS the `version_identity`
//                      check: the row says one thing and the package
//                      says another, and nothing can say which is right.
//
//   - `content_hash` — the projection's own content hash. It is the same
//                      SHA-256 the manifest records as `projection_sha256`
//                      (the import asserts the two are equal before it
//                      writes the row). Changing it makes the row assert a
//                      hash that describes neither the stored content nor
//                      the manifest.
//
// So these two keys are part of the packaged skill's IDENTITY, INTEGRITY
// and VERSIONING contract.
//
// The legacy metadata patch path could change them.
//
// `UpdateMemoryMetadata` merges an arbitrary JSON patch into the metadata
// column. It is reachable from the agent (`mpm_memory`'s `patch` action),
// from the CLI (`mpm patch-memory`) and from the epistemology handlers.
// Before this guard it could rewrite a packaged skill's `version` to a
// value that contradicts the package row — producing exactly the
// contradictory integrity state that the package contract forbids, and
// doing it through a path that has no authority over the package at all.
//
// WHAT THE GUARD DOES
// -------------------
// It refuses a patch that would CHANGE a protected key of a packaged
// skill. It is narrow on purpose:
//
//   - it applies ONLY to a skill that owns a package row. An ordinary
//     memory and an ordinary (unpackaged) skill are untouched, and every
//     metadata key on them stays as patchable as it always was;
//   - it applies ONLY to the protected keys. A patch that touches any
//     other key — `status`, `note`, `confidence`, anything — is permitted
//     and lands exactly as before;
//   - it applies ONLY when the patch would actually CHANGE the key. A
//     patch that names a protected key with the value it already holds
//     leaves the row identical, so it is permitted. A blanket prohibition
//     on the key's mere presence would refuse writes that change nothing,
//     which is a broader prohibition than the contract requires.
//
// The refusal is FAIL CLOSED: the guard runs inside the patch's own
// transaction, before the UPDATE, so nothing is written — not the metadata,
// not the timestamp, not the FTS index.
//
// THE COMPARISON IS SQLITE'S, NOT GO'S
// ------------------------------------
// The guard compares the protected key's value BEFORE the patch with its
// value AFTER the patch, and both values are computed by SQLite itself:
// the "after" value by the very same `json_patch` the UPDATE applies, and
// both values extracted with SQLite's own `json_type` / `json_extract`.
//
// Doing the comparison in SQL rather than in Go is not a style choice. A
// Go-side comparison has to decode the metadata document into a Go map,
// and a document can be VALID JSON that a Go map cannot represent — a
// number outside float64's range, for instance. The decode then fails for
// the WHOLE document, the guard sees an empty map on both sides, and a
// real change to `version` sails through unnoticed. The SQLite-side
// comparison has no such blind spot: it reads the one key it cares about
// and nothing else.
//
// THE TYPE IS PART OF THE VALUE
// -----------------------------
// The comparison reads the key's TYPE as well as its value, because
// SQLite's `json_extract` returns SQL NULL for two different states —
// an ABSENT path and a path whose value is JSON null — and because a
// text "1" and a number 1 are different values that render alike.
//
// A protected key whose stored value is neither absent nor a STRING is a
// MALFORMED row: the contract's own comparison cannot be made precisely
// on it (two distinct JSON numbers can be the same float64, and an object
// compares as its serialised text). A malformed row is refused outright
// — every patch on it is refused — because the alternative is to reason
// about a value the contract does not describe.
//
// THE VOCABULARY
// --------------
// The refusal has its OWN code, `package_metadata_immutable`, distinct from
// `package_immutable`. The two are different refusals: `package_immutable`
// says "you tried to replace the document", this one says "you tried to
// change a field the package owns". Both are state conflicts — the call is
// well-formed, and the skill's current state is what forbids it — but a
// caller that wants to count them separately can.

package internal

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"
)

// ErrPackageMetadataImmutable is the sentinel for the package-metadata
// refusal.
//
// It is deliberately NOT the same sentinel as ErrPackageImmutable: the two
// refusals describe different mutations, and ClassifyError gives each its
// own bounded code so they are countable apart.
var ErrPackageMetadataImmutable = errors.New("package_metadata_immutable")

// packageProtectedMetadataKeys are the metadata keys that are part of a
// packaged skill's identity, integrity and versioning contract.
//
// The list is closed and short on purpose. A broad prohibition would
// refuse legitimate metadata updates that have nothing to do with the
// package — and the contract does not require that. Each key below earns
// its place:
//
//	version      the version the package row and the skill id agree on;
//	             the verification's `version_identity` check reads it
//	content_hash the projection's content hash, which is the same value the
//	             manifest records as `projection_sha256`
//
// `is_latest` is deliberately NOT here: it is the substrate's own semver
// bookkeeping across a skill's versions, no package verification check
// reads it, and the substrate's own save path owns it. `author`,
// `promoted_at` and `decay_floor_days` are attribution and retention
// policy, not the package's identity.
//
// The slice is unexported on purpose. An exported slice is mutable state
// that any package in the tree could rewrite, and a guard whose protected
// key list is writable by its neighbours is not a guard.
var packageProtectedMetadataKeys = []string{"version", "content_hash"}

// PackageMetadataImmutableError is the typed refusal returned when a
// legacy metadata patch would change a protected key of a packaged skill.
type PackageMetadataImmutableError struct {
	// SkillID is the canonical `skill:<name>-v<version>` id.
	SkillID string
	// Name is the package's name.
	Name string
	// Version is the package's effective version.
	Version string
	// Key is the protected metadata key the patch would have changed.
	Key string
}

// Error implements the error interface.
//
// The message leads with the stable code `package_metadata_immutable`, so
// the CLI and the MCP surface both carry a greppable refusal rather than
// prose that has to be matched by shape.
func (e *PackageMetadataImmutableError) Error() string {
	return fmt.Sprintf(
		"package_metadata_immutable: skill %q v%s is a package; its %q metadata is part of "+
			"the package's identity and integrity contract and cannot be changed through the "+
			"legacy metadata patch path.",
		e.Name, e.Version, e.Key)
}

// Unwrap exposes the sentinel, so errors.Is(err, ErrPackageMetadataImmutable)
// recognises the refusal through any number of wrapping layers.
func (e *PackageMetadataImmutableError) Unwrap() error { return ErrPackageMetadataImmutable }

// The guard runs against the substrate's OWN executor abstraction, DBNode.
//
// DBNode is the shape every transaction in this substrate is reached
// through — `DatabaseManager.WithTx` hands one out, and `txNode` is the
// transaction-backed implementation of it. Using it here means the guard
// is written against the substrate's transaction discipline rather than
// against a private abstraction of its own.
//
// The guard MUST run inside the transaction that carries the patch's
// UPDATE. That is what makes the refusal atomic with the write: the check
// and the write share ONE snapshot, so a package row that appears after
// the check cannot be seen by the write either — SQLite refuses to
// promote a stale snapshot to a write — and the write cannot land on a
// state the check did not see.
//
// The statements therefore run on the TRANSACTION, never on the pool:
// there is no pool path here to lose a write on, which is why this is a
// transaction primitive and not a pool-write primitive.

// guardPackageMetadataPatch refuses a metadata patch that would change a
// protected key of a packaged skill.
//
// It runs INSIDE the caller's transaction, before the UPDATE, so the
// refusal is FAIL CLOSED: not the metadata, not the timestamp, not the FTS
// index.
//
// A row with no package row is returned immediately — there is nothing to
// guard, and the legacy behaviour is preserved exactly. So is a row the
// patch's own UPDATE would not touch: the UPDATE excludes tombstoned rows,
// so the guard's row read excludes them too, and a patch against a
// soft-deleted row keeps its pre-guard behaviour (the UPDATE reports the
// row as not found).
func guardPackageMetadataPatch(tx DBNode, id string, patchJSON string) error {
	pkg, err := lookupSkillPackageWith(tx.QueryTracked, id)
	if err != nil {
		return err
	}
	if pkg == nil {
		return nil
	}

	key, changed, err := protectedPackageMetadataChange(tx, id, patchJSON)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	return &PackageMetadataImmutableError{
		SkillID: id,
		Name:    pkg.name,
		Version: pkg.version,
		Key:     key,
	}
}

// packageMetadataValue is one protected key's value in a metadata
// document, as SQLite reports it.
type packageMetadataValue struct {
	// Type is SQLite's `json_type` for the value: "text", "integer",
	// "real", "true", "false", "null", "array" or "object" — or SQL
	// NULL when the path does not exist in the document at all.
	//
	// It is what distinguishes an ABSENT key from a key whose value is
	// JSON null: both extract to SQL NULL, and only the type tells
	// them apart.
	Type sql.NullString

	// Value is the value itself, extracted by SQLite.
	Value any
}

// absent reports whether the key is not in the document at all.
func (v packageMetadataValue) absent() bool { return !v.Type.Valid }

// malformed reports whether the key holds a value the contract does not
// describe: present, and not a string.
//
// A protected key is a version string or a hex digest. Anything else is a
// corrupt row, and the guard refuses it outright rather than reason about
// a value it cannot compare precisely.
func (v packageMetadataValue) malformed() bool {
	return v.Type.Valid && v.Type.String != "text"
}

// equal reports whether two extracted values are the same value.
//
// The comparison is type-aware: SQLite's `json_extract` returns an
// integer for an integer and a text value for a text value, and "1" is
// not 1. The type is compared first, so a patch that swaps a text version
// for a number is a change even when the two render alike.
func (v packageMetadataValue) equal(other packageMetadataValue) bool {
	if v.Type.Valid != other.Type.Valid {
		return false
	}
	if v.Type.Valid && v.Type.String != other.Type.String {
		return false
	}
	return packageMetadataScalarEqual(v.Value, other.Value)
}

// packageMetadataScalarEqual compares two extracted JSON scalars.
func packageMetadataScalarEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	switch av := a.(type) {
	case int64:
		bv, ok := b.(int64)
		return ok && av == bv
	case float64:
		bv, ok := b.(float64)
		return ok && av == bv
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case []byte:
		bv, ok := b.([]byte)
		return ok && bytesEqual(av, bv)
	}
	return reflect.DeepEqual(a, b)
}

// bytesEqual compares two byte slices.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// protectedPackageMetadataChange reports whether the patch would change
// one of the protected keys, and which one.
//
// The comparison is made against the PATCHED metadata, computed by SQLite
// itself with the very same `json_patch` the UPDATE applies. That matters:
// it means the guard reasons about the result the write will actually
// produce, with the engine's own merge semantics, rather than about an
// approximation of them.
//
// A patch that replaces the metadata document with something that is not an
// object (an array, a scalar) destroys every protected key the row carried,
// so that counts as a change too — and is reported the same way.
func protectedPackageMetadataChange(tx DBNode, id string, patchJSON string) (string, bool, error) {
	for _, key := range packageProtectedMetadataKeys {
		before, after, err := packageMetadataKeyValues(tx, id, patchJSON, key)
		if err != nil {
			return "", false, err
		}
		if before.malformed() {
			// A corrupt protected key: the contract's comparison
			// cannot be made precisely on it, so the row is refused
			// outright.
			return key, true, nil
		}
		if !before.equal(after) {
			return key, true, nil
		}
	}
	return "", false, nil
}

// packageMetadataKeyValues reads one metadata key's value before and
// after the patch, in ONE statement.
//
// Both values come from the same statement, so they come from the same
// snapshot: the "before" is the row's current document and the "after" is
// that same document with the patch applied, exactly as the UPDATE will
// compute it.
//
// The row read mirrors the UPDATE's own predicate (`deleted_at IS NULL`),
// so a tombstoned row is not guarded: the UPDATE will not touch it, and
// the patch keeps its pre-guard behaviour.
//
// The key's JSON path is BOUND as a parameter, not interpolated, so the
// statement is a constant and the path cannot escape it.
func packageMetadataKeyValues(tx DBNode, id, patchJSON, key string) (packageMetadataValue, packageMetadataValue, error) {
	path := "$." + key
	var before, after packageMetadataValue
	err := tx.QueryRowTracked(`
		SELECT
			json_type(doc, ?),
			json_extract(doc, ?),
			json_type(json_patch(doc, ?), ?),
			json_extract(json_patch(doc, ?), ?)
		FROM (
			SELECT COALESCE(metadata, '{}') AS doc
			FROM memories
			WHERE id = ? AND deleted_at IS NULL
		)
	`, path, path, patchJSON, path, patchJSON, path, id).Scan(
		&before.Type, &before.Value, &after.Type, &after.Value,
	)
	if errors.Is(err, sql.ErrNoRows) {
		// No live row: the UPDATE will not touch it. Nothing to guard.
		return before, after, nil
	}
	if err != nil {
		return before, after, fmt.Errorf(
			"evaluate the metadata patch against the package contract: %w", err)
	}
	return before, after, nil
}
