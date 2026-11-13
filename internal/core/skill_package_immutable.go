package internal

import (
	"errors"
	"fmt"
)

// ErrPackageImmutable is the sentinel for the package-immutability refusal.
//
// The refusal is a deliberate policy decision, not a substrate fault: the
// call is well-formed, and the skill's current state — it owns a package —
// is what forbids it. The sentinel lets ClassifyError route it to the
// conflict class with the bounded code `package_immutable`, so the refusal
// is countable without parsing prose (the same shape as
// ErrInvalidWorkTransition).
var ErrPackageImmutable = errors.New("package_immutable")

// PackageImmutableError is the typed refusal returned when the legacy
// single-document write path targets a skill that owns a package.
//
// The legacy force path replaces `memories.content` IN PLACE, under the
// same id. For a packaged skill that would give one id two documents: the
// `read` body would be the new one and `read_file(SKILL.md)` the old one,
// `projection_sha256` would stop describing `memories.content`, and the
// package row, the file rows and the blobs would all still describe the
// previous document.
//
// So the refusal is FAIL CLOSED: nothing is written — not the content, not
// the metadata, not the provenance, not the package manifest, not the file
// rows, not the blobs — and the caller is told to publish a new version
// instead.
type PackageImmutableError struct {
	// SkillID is the canonical `skill:<name>-v<version>` id.
	SkillID string
	// Name is the package's name.
	Name string
	// Version is the package's effective version.
	Version string
}

// Error implements the error interface.
//
// The message leads with the stable code `package_immutable`, so the CLI
// and the MCP surface both carry a greppable refusal rather than prose
// that has to be matched by shape.
func (e *PackageImmutableError) Error() string {
	return fmt.Sprintf(
		"package_immutable: skill %q v%s is a package; its entrypoint and manifest are immutable. "+
			"Publish a new version instead of force-overwriting the projection.",
		e.Name, e.Version)
}

// Unwrap exposes the sentinel, so errors.Is(err, ErrPackageImmutable)
// recognises the refusal through any number of wrapping layers.
func (e *PackageImmutableError) Unwrap() error { return ErrPackageImmutable }

// skillPackageIdentity is the identity a package row carries. It is the
// minimum the refusal needs to say which package it is refusing.
type skillPackageIdentity struct {
	name    string
	version string
}

// lookupSkillPackage reports whether a package row exists for the skill
// id, and returns the package's name and version when it does.
//
// The read runs through the caller's DBNode, so the check and the write it
// guards share ONE transaction and therefore ONE snapshot. That is what
// makes the guard safe: a package row created between the check and the
// write is either seen by the check or blocks the write, never slips
// between them.
//
// The read deliberately does NOT filter on the memories row's deleted_at:
// a soft-deleted (tombstoned) packaged skill still owns its package row,
// and the refusal applies to it too.
func lookupSkillPackage(node DBNode, skillID string) (*skillPackageIdentity, error) {
	rows, err := node.QueryTracked(`
		SELECT name, version FROM skill_packages WHERE skill_id = ?
	`, skillID)
	if err != nil {
		return nil, fmt.Errorf("lookup skill package: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("lookup skill package: %w", err)
		}
		return nil, nil
	}
	var id skillPackageIdentity
	if err := rows.Scan(&id.name, &id.version); err != nil {
		return nil, fmt.Errorf("scan skill package: %w", err)
	}
	return &id, nil
}
