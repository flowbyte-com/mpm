package internal

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"golang.org/x/mod/semver"
)

// SaveSkillProjectionInTx writes a projected skill row inside the CALLER's
// transaction.
//
// ─────────────────────────────────────────────────────────────────────────────
// TRANSACTION OWNERSHIP
//
// The caller owns the transaction. This function NEVER begins one, NEVER
// commits one and NEVER rolls one back: it writes through the transaction
// it is handed and returns. A caller that wants the write to persist
// commits; a caller that hits a later failure rolls back, and the
// projection AND the prior versions' is_latest changes disappear together.
//
// That is the whole point of the primitive. A future importer writes the
// projected skill row, the package manifest row, the file rows and the
// content-addressed blob rows in ONE transaction, so a failure anywhere
// leaves nothing behind — and the collector can never observe a blob row
// whose referencing file row is not yet committed (design §9, §3.4 E5).
//
// The write goes through `saveMemoryRow` reached via the `txNode` adapter,
// which is the substrate's canonical tx-aware INSERT primitive: the
// scanner guarantees, the content hash, the FTS indexing and the artifact
// provenance row all land inside the caller's transaction, and none of them
// is re-derived here.
//
// ─────────────────────────────────────────────────────────────────────────────
// WHAT IS PRESERVED
//
// This is a THIN EXTRACTION of the new-row path of SaveSkillWithInvocation,
// not a new writer. It keeps:
//
//   - the frontmatter parse and the secret/poison scan, at the same
//     boundary, so a rejected projection never reaches the database;
//   - the frontmatter name/version agreement check;
//   - the skill id formation (`skill:<name>-v<semver>`), so the id space
//     is unchanged;
//   - the semver comparison against the other live versions of the name,
//     and the is_latest flag it derives;
//   - the metadata shape, including the version mirror the semver
//     comparison reads back;
//   - the invocation attribution and the artifact provenance row.
//
// ─────────────────────────────────────────────────────────────────────────────
// THE READ-BACK ASSERTION
//
// `saveMemoryRow` performs its read-back assertion for standalone writes
// and DEFERS it for a transactional one: an uncommitted row is invisible
// to a read outside the transaction, so the caller's commit is the point
// at which the write is proven to have landed. That is the substrate's
// established contract for tx writers and this primitive inherits it.
//
// The caller MUST pass a transaction. A nil transaction is refused rather
// than silently writing outside the caller's transaction, because a write
// that escapes the transaction is exactly the failure this primitive
// exists to prevent.
func (dm *DatabaseManager) SaveSkillProjectionInTx(tx *sql.Tx, name, version, content, authorAgent string, ac ActiveContext) (string, error) {
	fm, err := dm.validateSkillProjection(name, version, content)
	if err != nil {
		return "", err
	}
	return dm.saveSkillProjectionInTx(tx, name, version, content, authorAgent, ac, fm)
}

// validateSkillProjection parses and scans a skill projection and checks
// that its frontmatter agrees with the requested name and version.
//
// The scanner runs here, at the boundary, so a projection carrying a
// secret or a poison phrase never reaches the database — from any caller,
// in any transaction.
//
// The error messages are the legacy ones, unchanged, so the existing
// callers' expectations hold.
func (dm *DatabaseManager) validateSkillProjection(name, version, content string) (SkillFrontmatter, error) {
	skill, _, scanErrs, err := validateSkillFrontmatterAndScanAt(content, dm.poisonPhrasesPath())
	if err != nil {
		return SkillFrontmatter{}, fmt.Errorf("save skill: %w", err)
	}
	if len(scanErrs) > 0 {
		return SkillFrontmatter{}, fmt.Errorf("save skill: scanner blocked content: %v", scanErrs)
	}
	fm := skill.Frontmatter
	if fm.Name != name {
		return SkillFrontmatter{}, fmt.Errorf("frontmatter name %q does not match arg %q", fm.Name, name)
	}
	if fm.Version != version {
		return SkillFrontmatter{}, fmt.Errorf("frontmatter version %q does not match arg %q", fm.Version, version)
	}
	return fm, nil
}

// saveSkillProjectionInTx is the implementation of the tx-aware write.
//
// It takes an ALREADY-VALIDATED frontmatter, so the legacy path — which
// validates once for both of its branches — does not scan the projection
// twice. The public entry point is SaveSkillProjectionInTx.
func (dm *DatabaseManager) saveSkillProjectionInTx(tx *sql.Tx, name, version, content, authorAgent string, ac ActiveContext, fm SkillFrontmatter) (string, error) {
	if tx == nil {
		return "", fmt.Errorf("save skill: a caller-owned transaction is required")
	}

	id, err := SkillIDForNameAndVersion(name, version)
	if err != nil {
		return "", fmt.Errorf("save skill: %w", err)
	}

	// The txNode adapter is what turns the caller's transaction into the
	// DBNode the shared write primitives take.
	node := &txNode{tx: tx, dm: dm}

	// The existence check runs INSIDE the caller's transaction, so the id
	// is observed against the same snapshot the INSERT will land in.
	//
	// The check includes tombstones. A soft-deleted skill still owns its
	// id — the row is preserved for audit — so an INSERT against a
	// tombstoned id would collide on the primary key. Refusing here says
	// "already exists" instead of surfacing a constraint violation.
	var existing string
	err = node.QueryRowTracked(`SELECT id FROM memories WHERE id = ?`, id).Scan(&existing)
	switch {
	case err == nil:
		return "", fmt.Errorf("skill %s already exists; pass force=true to overwrite", id)
	case !errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("lookup existing skill: %w", err)
	}

	// is_latest is semver-driven, not save-order-driven. We find the
	// current max-version row for this name within the transaction and
	// only flip it when the incoming version strictly exceeds it. Saving
	// an older version (e.g. v1 after v2) inserts with is_latest=false
	// and leaves the higher existing row untouched.
	otherRows, qerr := loadActiveSkillRowsTx(tx, name, id)
	if qerr != nil {
		return "", qerr
	}
	otherMax := highestSemver(otherRows)
	newIsLatest := true
	if otherMax.id != "" {
		// semver.Compare requires the 'v' prefix on both arguments. The
		// DB stores version without the prefix (YAML convention); we add
		// it here. Prepending unconditionally is safe: the frontmatter
		// parser already validated the semver shape, and an empty version
		// would have failed upstream.
		cmp := semver.Compare("v"+version, "v"+otherMax.version)
		switch {
		case cmp > 0:
			// Incoming version strictly higher — flip every other row
			// whose semver is below the incoming version to false. This
			// handles the unequal-width case where the lexically-largest
			// id is NOT the semver-max.
			for _, r := range otherRows {
				if semver.Compare("v"+r.version, "v"+version) < 0 {
					if _, err := node.ExecTracked(`
						UPDATE memories
						SET metadata = json_set(COALESCE(metadata, '{}'), '$.is_latest', 0)
						WHERE id = ? AND deleted_at IS NULL
					`, 5, r.id); err != nil {
						return "", fmt.Errorf("flip prior is_latest: %w", err)
					}
				}
			}
		case cmp == 0:
			// The existence check above should have caught this.
			// Defensive.
			return "", fmt.Errorf("save skill: version %s already exists for %s (id=%s)", version, name, otherMax.id)
		default:
			// cmp < 0: incoming version is older than current max — keep
			// the higher existing row as latest, insert this one as
			// not-latest.
			newIsLatest = false
		}
	}

	tags := skillRowTags(fm.Domain)
	metadata := skillRowMetadata(version, content, authorAgent, newIsLatest)

	// The INSERT lands in the caller's transaction, through the shared
	// scanner+INSERT primitive. Skills don't carry embeddings, so IVF
	// assignment is a no-op here.
	if _, err := saveMemoryRow(node, dm, id, "skills", content, "", tags, metadata, nil, true, 5, "", "", "", "", ac); err != nil {
		return "", fmt.Errorf("insert skill: %w", err)
	}
	return id, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// RESURRECTION
// ─────────────────────────────────────────────────────────────────────────────

// ResurrectSkillProjectionInTx revives a SOFT-DELETED skill row inside
// the CALLER's transaction, writing the projection in place.
//
// ─────────────────────────────────────────────────────────────────────────────
// WHY THIS EXISTS, AND WHY IT IS NOT THE LEGACY FORCE PATH
//
// A soft shred (mpm skill delete) stamps deleted_at on the skill row and
// preserves the row for audit. The row still owns its id, so a new-row
// INSERT against it would collide on the primary key.
//
// The PACKAGE IMPORT needs to revive such a row: §5.7 says a re-import
// of the same package_hash resurrects the skill, and says so
// EXPLICITLY — "the import path is not the legacy force path (§5.8.1),
// so R1's refusal does not apply to it".
//
// This primitive is therefore NOT the legacy force path. The difference
// is material:
//
//   - the legacy force path REPLACES memories.content under the same id
//     for ANY skill, which is exactly what would give one packaged id two
//     documents. It is refused against a packaged skill (R1).
//   - this primitive revives a row that carries NO package, or revives a
//     packaged row whose package_hash is UNCHANGED — so the id never
//     ends up carrying two documents.
//
// The caller owns the transaction, exactly as for SaveSkillProjectionInTx:
// this function never begins, commits or rolls back one.
//
// The row is revived with deleted_at = NULL, and the content, the
// metadata's is_latest / version / content_hash and the author are
// written. The weight and the reinforcement_count are NOT touched: they
// encode the skill's retrieval trajectory, and a revival must not
// destroy it.
//
// The row's ARTIFACT PROVENANCE is refreshed in the same transaction. The
// resurrection REPLACES the content, so the row's previous attribution
// describes a writer who no longer wrote it; leaving it in place would be
// false attribution — the failure refreshArtifactProvenance exists to
// prevent. The refresh is tx-scoped (RecordArtifactProvenanceUpsert), not
// refreshArtifactProvenance, because the latter opens its own
// transaction and would contend with the caller's.
func (dm *DatabaseManager) ResurrectSkillProjectionInTx(tx *sql.Tx, name, version, content, authorAgent string, ac ActiveContext) (string, error) {
	if tx == nil {
		return "", fmt.Errorf("save skill: a caller-owned transaction is required")
	}

	// The projection is validated and SCANNED before anything is
	// written, exactly as in the new-row path: a projection carrying a
	// secret or a poison phrase never reaches the database, from any
	// caller, in any transaction.
	if _, err := dm.validateSkillProjection(name, version, content); err != nil {
		return "", err
	}

	id, err := SkillIDForNameAndVersion(name, version)
	if err != nil {
		return "", fmt.Errorf("save skill: %w", err)
	}

	// The row must exist AND be soft-deleted. A live row is refused:
	// reviving a live row would be the in-place overwrite R1 forbids.
	var deletedAt sql.NullInt64
	err = tx.QueryRow(`SELECT deleted_at FROM memories WHERE id = ?`, id).Scan(&deletedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("save skill: %s does not exist, so there is nothing to resurrect", id)
	case err != nil:
		return "", fmt.Errorf("save skill: lookup existing skill: %w", err)
	}
	if !deletedAt.Valid {
		return "", fmt.Errorf("save skill: %s is not soft-deleted", id)
	}

	// is_latest is semver-driven, exactly as in the new-row path: the
	// incoming version is compared against the other LIVE versions of the
	// name, and the rows it outranks are flipped inside the same
	// transaction.
	otherRows, err := loadActiveSkillRowsTx(tx, name, id)
	if err != nil {
		return "", err
	}
	otherMax := highestSemver(otherRows)
	newIsLatest := true
	if otherMax.id != "" {
		cmp := semver.Compare("v"+version, "v"+otherMax.version)
		switch {
		case cmp > 0:
			for _, r := range otherRows {
				if semver.Compare("v"+r.version, "v"+version) < 0 {
					if _, err := tx.Exec(`
						UPDATE memories
						SET metadata = json_set(COALESCE(metadata, '{}'), '$.is_latest', 0)
						WHERE id = ? AND deleted_at IS NULL
					`, r.id); err != nil {
						return "", fmt.Errorf("flip prior is_latest: %w", err)
					}
				}
			}
		case cmp == 0:
			return "", fmt.Errorf("save skill: version %s already exists for %s (id=%s)", version, name, otherMax.id)
		default:
			newIsLatest = false
		}
	}

	// The content and the metadata are written in place, and the
	// tombstone is cleared. The weight and the reinforcement_count are
	// deliberately absent from the SET clause: SQLite preserves the
	// columns it is not told to change, and those two encode the skill's
	// retrieval trajectory.
	if _, err := tx.Exec(`
		UPDATE memories
		SET content = ?,
		    metadata = json_set(COALESCE(metadata, '{}'),
		        '$.is_latest', ?,
		        '$.author', ?,
		        '$.version', ?,
		        '$.content_hash', ?),
		    deleted_at = NULL,
		    updated_at = CAST(strftime('%s','now') AS INTEGER)
		WHERE id = ? AND deleted_at IS NOT NULL
	`, content, boolToInt(newIsLatest), authorAgent, version, contentHash(content), id); err != nil {
		return "", fmt.Errorf("resurrect skill: %w", err)
	}

	// ── The provenance is refreshed, INSIDE this transaction. ──────────
	//
	// The content of this row has just been REPLACED. The row's
	// artifact_provenance row therefore describes a writer who no longer
	// wrote it — which is exactly the false attribution the substrate's
	// refreshArtifactProvenance exists to prevent ("the row describing
	// the FIRST writer while the artifact's content belongs to the LATEST
	// one").
	//
	// The legacy force path calls refreshArtifactProvenance for the same
	// in-place rewrite. It cannot be called HERE: it opens its OWN
	// transaction, which would contend with the caller's already-open
	// write transaction on the same *sql.DB. The tx-scoped upsert is used
	// instead, so the content change and the attribution change commit
	// together or not at all.
	if err := dm.refreshSkillProvenanceInTx(tx, id, ac); err != nil {
		return "", err
	}
	return id, nil
}

// errSkillProvenanceRefresh marks a failure to refresh a skill's artifact
// provenance.
//
// It is a distinct error so the import path can classify it separately
// from a frontmatter refusal: the document was fine, but the attribution
// could not be written, so the resurrection is rolled back rather than
// reported as a successful resurrection with a stale writer.
var errSkillProvenanceRefresh = errors.New("skill provenance refresh failed")

// refreshSkillProvenanceInTx rewrites the artifact provenance of a skill
// row inside the CALLER's transaction.
//
// The attribution must describe the writer of the CURRENT content. A
// resurrection replaces the content in place, so the previous attribution
// is obsolete and is overwritten rather than retained.
//
// The identity resolution is the substrate's own:
//
//   - a caller that supplies an ActiveContext with a provenance identity
//     gets THAT identity — the invocation that produced the new content;
//   - a caller that supplies an EMPTY ActiveContext gets the AMBIENT
//     identity (the operator's), which is accurate: the content was
//     rewritten by an operator action, not by the agent that wrote the
//     original document.
//
// An empty context does NOT leave the previous attribution in place. The
// upsert's refresh clause overwrites every identity column, so the
// obsolete writer is cleared and the row says "unknown" rather than
// naming somebody who did not write the content.
//
// A failure is RETURNED, not logged and swallowed: the caller must not
// report a successful resurrection whose attribution is wrong.
func (dm *DatabaseManager) refreshSkillProvenanceInTx(tx *sql.Tx, id string, ac ActiveContext) error {
	if tx == nil {
		return fmt.Errorf("save skill: a caller-owned transaction is required")
	}
	if id == "" {
		return nil
	}

	prov := dm.provenanceForWrite(ac)
	if prov == nil {
		// No identity could be resolved at all. The row's attribution is
		// still obsolete, so it is replaced with the explicit "unknown"
		// identity rather than left naming the previous writer.
		prov = &EffectiveProvenance{ActorKind: "unknown"}
	}

	res := dm.RecordArtifactProvenanceUpsert(tx, id, artifactTypeFromCollection("skills"), prov, true)
	if !res.Recorded {
		reason := res.ValidationReason
		if reason == "" {
			reason = res.SQLError
		}
		return fmt.Errorf("%w: %s could not be refreshed: %s", errSkillProvenanceRefresh, id, reason)
	}
	return nil
}

// skillRowTags builds the tag list a skill row carries.
//
// Tags: ["skill", <domain>] — the domain is omitted when empty so the
// SKILL domain taxonomy doesn't get polluted with empty strings.
func skillRowTags(domain string) []string {
	tags := []string{"skill"}
	if domain != "" {
		tags = append(tags, domain)
	}
	return tags
}

// skillRowMetadata builds the metadata map a skill row carries.
//
// The shape is the legacy one, unchanged:
//
//   - is_latest — the semver-derived flag;
//   - author — the agent that wrote the row;
//   - promoted_at — intentionally JSON null (not absent) so the field
//     shape is stable across reads; the promote task json_sets it to a
//     real timestamp without having to first insert the key;
//   - decay_floor_days — the retention floor;
//   - content_hash — the hex SHA-256 of the content;
//   - version — the version mirror, so the semver-compare path can read
//     the version back without re-parsing the frontmatter (the id also
//     encodes it, but json_extract on a JSON column is cleaner than
//     string surgery on skill:<name>-v<version>).
func skillRowMetadata(version, content, authorAgent string, isLatest bool) map[string]interface{} {
	return map[string]interface{}{
		"is_latest":        isLatest,
		"author":           authorAgent,
		"promoted_at":      nil,
		"decay_floor_days": 90,
		"content_hash":     contentHash(content),
		"version":          version,
	}
}

// marshalSkillRowMetadata serialises a skill row's metadata. The shape is
// a fixed map of scalars, so the marshal cannot fail in practice; the
// error is still surfaced rather than swallowed.
func marshalSkillRowMetadata(metadata map[string]interface{}) (string, error) {
	metaJSON, err := json.Marshal(metadata)
	if err != nil {
		return "", fmt.Errorf("marshal metadata: %w", err)
	}
	return string(metaJSON), nil
}
