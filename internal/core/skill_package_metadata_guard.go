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
	"encoding/json"
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

// PackageProtectedMetadataKeys are the metadata keys that are part of a
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
var PackageProtectedMetadataKeys = []string{"version", "content_hash"}

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

// guardPackageMetadataPatch refuses a metadata patch that would change a
// protected key of a packaged skill.
//
// It runs INSIDE the caller's transaction, so the check and the write it
// guards share one snapshot: a package row that appears between the check
// and the UPDATE cannot let a protected key through, because the UPDATE's
// own WHERE clause re-tests the package's ownership.
//
// A row with no package row is returned immediately — there is nothing to
// guard, and the legacy behaviour is preserved exactly.
func guardPackageMetadataPatch(tx *sql.Tx, id string, patchJSON string) error {
	pkg, err := lookupSkillPackageWith(tx.Query, id)
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
func protectedPackageMetadataChange(tx *sql.Tx, id string, patchJSON string) (string, bool, error) {
	var before, after sql.NullString
	err := tx.QueryRow(`
		SELECT COALESCE(metadata, '{}'), json_patch(COALESCE(metadata, '{}'), ?)
		FROM memories WHERE id = ?
	`, patchJSON, id).Scan(&before, &after)
	if errors.Is(err, sql.ErrNoRows) {
		// No row: the UPDATE will report it. Nothing to guard.
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("evaluate the metadata patch against the package contract: %w", err)
	}

	var b, a map[string]any
	if err := json.Unmarshal([]byte(before.String), &b); err != nil {
		b = nil
	}
	if err := json.Unmarshal([]byte(after.String), &a); err != nil {
		// The patch replaced the document with a non-object. Every
		// protected key it carried is gone.
		a = nil
	}

	for _, key := range PackageProtectedMetadataKeys {
		bv, bok := b[key]
		av, aok := a[key]
		if bok == aok && reflect.DeepEqual(bv, av) {
			continue
		}
		return key, true, nil
	}
	return "", false, nil
}
