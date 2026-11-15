// skill_package_provenance_followup_test.go — the P6 follow-up: the
// complete hash contract, and the single-snapshot read.
//
// Design of record: PR #222 §8.2.1 (the provenance object) and the
// operator's P6 follow-up decisions.
//
// Two things are pinned here.
//
// A1 — THE HASH CONTRACT. The provenance object carries THREE hashes and
// says which is which:
//
//   - `entrypoint_sha256` — the RECORDED hash of the ORIGINAL,
//     unchanged `SKILL.md`;
//   - `projection_sha256` — the RECORDED hash of the complete derived
//     projection, obtained from the stored manifest and VALIDATED
//     against the projection being returned;
//   - `body_sha256` — the RECOMPUTED hash of the exact bytes of the
//     `body` field the response returns.
//
// The distinction is not cosmetic. A recorded hash is a claim the store
// makes; a recomputed hash is a measurement the reader takes. Reporting
// one as the other would let a caller believe it had checked a stored
// claim when it had merely observed a fresh value.
//
// A2 — ONE SNAPSHOT. A successful read of a packaged skill cannot return
// its body without its package provenance because a concurrent deletion
// happened between the two queries. The result is either a consistent
// skill/provenance pair, or the not-found answer.
//
// EVERY test here owns its workspace: the fixtures pin MPM_WORKSPACE to a
// disposable directory before the manager is constructed, so no test can
// reach the live database, the live workspace or the operator's checkout.

package internal

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// ─── Fixtures ──────────────────────────────────────────────────────────────

// p6fuWorkspace pins MPM_WORKSPACE to a disposable directory and returns
// it.
//
// It is the testing.TB form of the repository's own workspace fixture,
// so the benchmarks can use it too.
func p6fuWorkspace(tb testing.TB) string {
	tb.Helper()
	ws := tb.TempDir()
	tb.Setenv("MPM_WORKSPACE", ws)
	tb.Setenv("MPM_SHARED_DB", "")
	return ws
}

// p6fuDM opens a database in the disposable workspace, through the
// PRODUCTION path, so the DSN's `_foreign_keys=1` applies and the
// cascade that a skill deletion triggers is enforced.
//
// The workspace root is passed EXPLICITLY. `NewDatabaseManager("")`
// falls through to `config.GetMPMDir()`, which is the ambient
// workspace — and the substrate's DB-safety guard refuses a test that
// opens a database that way.
func p6fuDM(tb testing.TB) *DatabaseManager {
	tb.Helper()
	ws := p6fuWorkspace(tb)
	dm, err := NewDatabaseManager(ws)
	if err != nil {
		tb.Fatalf("NewDatabaseManager: %v", err)
	}
	tb.Cleanup(func() { dm.Close() })
	return dm
}

// p6fuTree writes a package tree into a fresh temporary directory.
func p6fuTree(tb testing.TB, files map[string][]byte) string {
	tb.Helper()
	root := tb.TempDir()
	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			tb.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(full, content, 0o600); err != nil {
			tb.Fatalf("write %s: %v", full, err)
		}
	}
	return root
}

// p6fuImport imports a package built from the given supporting files.
func p6fuImport(tb testing.TB, dm *DatabaseManager, name, version string, files map[string][]byte) *PackageImportResult {
	tb.Helper()
	tree := map[string][]byte{}
	for k, v := range files {
		tree[k] = v
	}
	fm := "---\nname: " + name + "\ndescription: a test skill\nwhen_to_use: when a test needs this exact skill\n"
	if version != "" {
		fm += "version: " + version + "\n"
	}
	tree["SKILL.md"] = []byte(fm + "---\nthe original entrypoint body\n")
	result, err := dm.ImportSkillPackageFromDir(PackageImportOptions{Path: p6fuTree(tb, tree)})
	if err != nil {
		tb.Fatalf("import the package: %v", err)
	}
	return result
}

// p6fuDigest is the lowercase hex SHA-256 of some bytes.
func p6fuDigest(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// p6fuManifestOf reads a package's stored manifest.
func p6fuManifestOf(t *testing.T, dm *DatabaseManager, skillID string) *PackageManifest {
	t.Helper()
	var stored string
	if err := dm.db.QueryRow(`SELECT manifest FROM skill_packages WHERE skill_id = ?`, skillID).Scan(&stored); err != nil {
		t.Fatalf("read the stored manifest: %v", err)
	}
	manifest, _, err := ParsePackageManifest([]byte(stored))
	if err != nil {
		t.Fatalf("parse the stored manifest: %v", err)
	}
	return manifest
}

// ─── A1: the hash contract ─────────────────────────────────────────────────

// TestP6FollowupProvenanceCarriesTheThreeHashes pins that the provenance
// object carries the three hashes, and that each one's ORIGIN says where
// it came from.
//
// The entrypoint's hash and the projection's hash are RECORDED claims;
// the body's hash is a RECOMPUTED measurement.
func TestP6FollowupProvenanceCarriesTheThreeHashes(t *testing.T) {
	dm := p3DM(t)
	imported := p6ImportPackage(t, dm, "hashed", "1.0.0", map[string][]byte{
		"references/templates.md": []byte("the reference body\n"),
	})

	skill, provenance, err := dm.ReadSkillWithPackageProvenance(context.Background(), imported.SkillID, "")
	if err != nil {
		t.Fatalf("ReadSkillWithPackageProvenance: %v", err)
	}
	if provenance == nil {
		t.Fatalf("a packaged skill's read carries no provenance")
	}

	// The entrypoint's hash is the ORIGINAL's, and it is recorded.
	if provenance.EntrypointSHA256 != imported.EntrypointSHA256 {
		t.Errorf("the entrypoint hash is %s, want the original's %s",
			provenance.EntrypointSHA256, imported.EntrypointSHA256)
	}
	if provenance.HashOrigins.EntrypointSHA256 != PackageHashOriginRecorded {
		t.Errorf("the entrypoint hash's origin is %q, want %q",
			provenance.HashOrigins.EntrypointSHA256, PackageHashOriginRecorded)
	}

	// The projection's hash is the RECORDED manifest value.
	manifest := p6fuManifestOf(t, dm, imported.SkillID)
	if provenance.ProjectionSHA256 != manifest.ProjectionSHA256 {
		t.Errorf("the projection hash is %s, want the manifest's recorded %s",
			provenance.ProjectionSHA256, manifest.ProjectionSHA256)
	}
	if provenance.HashOrigins.ProjectionSHA256 != PackageHashOriginRecorded {
		t.Errorf("the projection hash's origin is %q, want %q",
			provenance.HashOrigins.ProjectionSHA256, PackageHashOriginRecorded)
	}

	// The body's hash is RECOMPUTED, and it is the hash of the body the
	// caller receives.
	if provenance.BodySHA256 != p6fuDigest([]byte(skill.Body)) {
		t.Errorf("the body hash is %s, want the hash of the returned body %s",
			provenance.BodySHA256, p6fuDigest([]byte(skill.Body)))
	}
	if provenance.HashOrigins.BodySHA256 != PackageHashOriginRecomputed {
		t.Errorf("the body hash's origin is %q, want %q",
			provenance.HashOrigins.BodySHA256, PackageHashOriginRecomputed)
	}

	// The body is a different document from the original entrypoint, so
	// the two hashes must differ.
	if provenance.BodySHA256 == provenance.EntrypointSHA256 {
		t.Errorf("the body hash and the entrypoint hash are the same value (%s); "+
			"the body is a different document from the original entrypoint",
			provenance.BodySHA256)
	}
}

// TestP6FollowupProvenanceDistinguishesTheOriginalFromTheProjection pins
// that a VERSIONLESS package's three hashes are three DIFFERENT hashes.
//
// A versionless package is the case where the projection differs from the
// original: the projection carries one inserted `version:` line. So the
// original's hash and the projection's hash must differ — and the
// projection's hash must be the RECORDED manifest value, not a fresh
// computation presented as one.
func TestP6FollowupProvenanceDistinguishesTheOriginalFromTheProjection(t *testing.T) {
	dm := p3DM(t)
	imported := p6ImportPackage(t, dm, "versionless", "", map[string][]byte{
		"references/templates.md": []byte("the reference body\n"),
	})

	skill, provenance, err := dm.ReadSkillWithPackageProvenance(context.Background(), imported.SkillID, "")
	if err != nil {
		t.Fatalf("ReadSkillWithPackageProvenance: %v", err)
	}
	if provenance == nil {
		t.Fatalf("a packaged skill's read carries no provenance")
	}

	manifest := p6fuManifestOf(t, dm, imported.SkillID)
	if manifest.ProjectionSHA256 == manifest.EntrypointSHA256 {
		t.Fatalf("the fixture's projection and original hashes are equal (%s); "+
			"a versionless package's projection must differ from its original",
			manifest.ProjectionSHA256)
	}
	if provenance.EntrypointSHA256 != manifest.EntrypointSHA256 {
		t.Errorf("the entrypoint hash is %s, want the original's %s",
			provenance.EntrypointSHA256, manifest.EntrypointSHA256)
	}
	if provenance.ProjectionSHA256 != manifest.ProjectionSHA256 {
		t.Errorf("the projection hash is %s, want the manifest's recorded %s",
			provenance.ProjectionSHA256, manifest.ProjectionSHA256)
	}
	if provenance.EntrypointSHA256 == provenance.ProjectionSHA256 {
		t.Errorf("the entrypoint hash and the projection hash are the same value (%s); "+
			"the original and the projection are different documents",
			provenance.EntrypointSHA256)
	}

	// The body is the projection's body — the part after the
	// frontmatter — and its hash is the recomputed one.
	if provenance.BodySHA256 != p6fuDigest([]byte(skill.Body)) {
		t.Errorf("the body hash is %s, want %s",
			provenance.BodySHA256, p6fuDigest([]byte(skill.Body)))
	}
	if provenance.BodySHA256 == provenance.ProjectionSHA256 {
		t.Errorf("the body hash and the projection hash are the same value (%s)",
			provenance.BodySHA256)
	}
}

// TestP6FollowupProvenanceIsAbsentForAnOrdinarySkill pins that an
// ordinary skill's read carries NO package object — the object is
// mandatory for a packaged skill and absent for everything else.
func TestP6FollowupProvenanceIsAbsentForAnOrdinarySkill(t *testing.T) {
	dm := p3DM(t)
	id, err := dm.SaveSkillWithInvocation("ordinary", "1.0.0",
		p3SkillContent("ordinary", "1.0.0", "an ordinary body\n"), "tester", false, ActiveContext{})
	if err != nil {
		t.Fatalf("save the ordinary skill: %v", err)
	}

	skill, provenance, err := dm.ReadSkillWithPackageProvenance(context.Background(), id, "")
	if err != nil {
		t.Fatalf("ReadSkillWithPackageProvenance: %v", err)
	}
	if skill == nil {
		t.Fatalf("the read returned no skill")
	}
	if provenance != nil {
		t.Fatalf("an ordinary skill's read carries a package object: %+v", provenance)
	}
}

// TestP6FollowupProvenanceReportsAnUnreadableManifestAsUnavailable pins
// the TOLERANT half of the contract.
//
// A stored manifest that does not parse leaves no recorded projection
// hash that could be validated. The read still returns the body — that
// is what `verify` is for — and the provenance says the projection hash
// is UNAVAILABLE rather than carrying an empty string that reads like a
// hash.
func TestP6FollowupProvenanceReportsAnUnreadableManifestAsUnavailable(t *testing.T) {
	dm := p3DM(t)
	imported := p6ImportPackage(t, dm, "broken", "1.0.0", map[string][]byte{
		"references/templates.md": []byte("the reference body\n"),
	})

	if _, err := dm.db.Exec(`UPDATE skill_packages SET manifest = ? WHERE skill_id = ?`,
		"{ this is not a manifest", imported.SkillID); err != nil {
		t.Fatalf("break the stored manifest: %v", err)
	}

	skill, provenance, err := dm.ReadSkillWithPackageProvenance(context.Background(), imported.SkillID, "")
	if err != nil {
		t.Fatalf("the read of a package with a broken manifest failed: %v", err)
	}
	if skill == nil || skill.Body == "" {
		t.Fatalf("the read returned no body")
	}
	if provenance == nil {
		t.Fatalf("a packaged skill's read carries no provenance")
	}
	if provenance.ProjectionSHA256 != "" {
		t.Errorf("the projection hash is %q, want it to be absent", provenance.ProjectionSHA256)
	}
	if provenance.HashOrigins.ProjectionSHA256 != PackageHashOriginUnavailable {
		t.Errorf("the projection hash's origin is %q, want %q",
			provenance.HashOrigins.ProjectionSHA256, PackageHashOriginUnavailable)
	}
	// The body's hash is still recomputed: the body IS known.
	if provenance.BodySHA256 != p6fuDigest([]byte(skill.Body)) {
		t.Errorf("the body hash is %s, want %s", provenance.BodySHA256, p6fuDigest([]byte(skill.Body)))
	}
	if provenance.HashOrigins.BodySHA256 != PackageHashOriginRecomputed {
		t.Errorf("the body hash's origin is %q, want %q",
			provenance.HashOrigins.BodySHA256, PackageHashOriginRecomputed)
	}
}

// TestP6FollowupProvenanceRefusesAProjectionThatContradictsTheManifest
// pins the FAIL-CLOSED half.
//
// A manifest that validates and whose RECORDED projection hash
// disagrees with the projection the store actually holds is a positive
// claim of corruption: the store says one thing and holds another. The
// read refuses it, because reporting a recorded hash that does not
// describe the bytes beside it would be reporting a false claim.
func TestP6FollowupProvenanceRefusesAProjectionThatContradictsTheManifest(t *testing.T) {
	dm := p3DM(t)
	imported := p6ImportPackage(t, dm, "contradicted", "1.0.0", map[string][]byte{
		"references/templates.md": []byte("the reference body\n"),
	})

	// Rewrite the stored projection, leaving the manifest's recorded
	// hash describing the ORIGINAL projection.
	if _, err := dm.db.Exec(`UPDATE memories SET content = ? WHERE id = ?`,
		"---\nname: contradicted\nversion: 1.0.0\ndescription: a test skill\nwhen_to_use: when a test needs this exact skill\n---\na different body\n",
		imported.SkillID); err != nil {
		t.Fatalf("rewrite the stored projection: %v", err)
	}

	_, _, err := dm.ReadSkillWithPackageProvenance(context.Background(), imported.SkillID, "")
	if err == nil {
		t.Fatalf("the read of a contradicted projection succeeded")
	}
	if reason := p6ReadReason(err); reason != PackageReadReasonIntegrityFailure {
		t.Errorf("the refusal's reason is %q, want %q", reason, PackageReadReasonIntegrityFailure)
	}
}

// ─── A2: one snapshot ──────────────────────────────────────────────────────

// TestP6FollowupTheReadIsOneSnapshot pins the invariant DETERMINISTICALLY,
// against the EXPORTED read.
//
// The test holds a read snapshot open, deletes the package CONCURRENTLY
// on another connection, and then runs the EXPORTED
// `ReadSkillWithPackageProvenance` inside the held snapshot — through
// the snapshot seam, so the interleaving is CHOSEN rather than raced
// for.
//
// Two things are asserted, and the second is the load-bearing one:
//
//   - the export read returns the CONSISTENT PAIR — the skill AND its
//     provenance — because the held snapshot does not see the deletion;
//   - the export read really took ONE snapshot: the seam was called.
//
// The second assertion is what makes this a guard rather than a
// demonstration. A regression that turned the exported read back into
// two independent statements would never call the seam, so the test
// would fail on the counter rather than on a race it happened to lose.
//
// With two independent statements the same interleaving would return
// the skill with NO provenance, because the first statement would have
// seen the package and the second would not.
func TestP6FollowupTheReadIsOneSnapshot(t *testing.T) {
	dm := p3DM(t)
	imported := p6ImportPackage(t, dm, "snapshot", "1.0.0", map[string][]byte{
		"references/templates.md": []byte("the reference body\n"),
	})

	// A dedicated connection, so the snapshot below is a snapshot and
	// not a pool checkout.
	conn, err := dm.db.Conn(context.Background())
	if err != nil {
		t.Fatalf("take a connection: %v", err)
	}
	defer conn.Close()

	tx, err := conn.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("begin the read snapshot: %v", err)
	}
	defer tx.Rollback()

	// Establish the snapshot with a read.
	var probe int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM memories WHERE id = ?`, imported.SkillID).Scan(&probe); err != nil {
		t.Fatalf("establish the snapshot: %v", err)
	}

	// Delete the package CONCURRENTLY, on another connection. In WAL
	// mode a writer does not block on a reader, so this commits while
	// the snapshot above is still open.
	if _, err := dm.db.Exec(`DELETE FROM memories WHERE id = ?`, imported.SkillID); err != nil {
		t.Fatalf("delete the skill concurrently: %v", err)
	}

	// The exported read runs inside the HELD snapshot.
	seamCalls := 0
	restore := setSkillReadSnapshotHook(func(ctx context.Context, fn func(*sql.Tx) error) error {
		seamCalls++
		return fn(tx)
	})
	defer restore()

	skill, provenance, err := dm.ReadSkillWithPackageProvenance(context.Background(), imported.SkillID, "")
	if err != nil {
		t.Fatalf("the read inside the held snapshot failed: %v", err)
	}
	if skill == nil {
		t.Fatalf("the read inside the held snapshot returned no skill")
	}
	if provenance == nil {
		t.Fatalf("the read inside the held snapshot returned a skill with NO provenance; " +
			"the two reads came from different snapshots")
	}
	if provenance.EntrypointSHA256 != imported.EntrypointSHA256 {
		t.Errorf("the entrypoint hash is %s, want %s", provenance.EntrypointSHA256, imported.EntrypointSHA256)
	}

	// The load-bearing assertion: the exported read took ONE snapshot.
	// A regression that split it back into two independent reads would
	// never have called the seam.
	if seamCalls != 1 {
		t.Fatalf("the exported read opened %d snapshot(s), want exactly 1; "+
			"the read is not reading the body and the provenance from one snapshot", seamCalls)
	}
}

// TestP6FollowupTheReadReportsNotFoundAfterADeletion pins the OTHER half
// of the invariant, also deterministically and against the exported
// read.
//
// A deletion that has COMMITTED is wholly visible: the read returns the
// ordinary not-found answer, not a body without its provenance.
//
// The snapshot here is a FRESH one — taken after the deletion — so the
// two halves of the read see the same, post-deletion state.
func TestP6FollowupTheReadReportsNotFoundAfterADeletion(t *testing.T) {
	dm := p3DM(t)
	imported := p6ImportPackage(t, dm, "deleted", "1.0.0", map[string][]byte{
		"references/templates.md": []byte("the reference body\n"),
	})

	// The deletion COMMITS before the read starts.
	if _, err := dm.db.Exec(`DELETE FROM memories WHERE id = ?`, imported.SkillID); err != nil {
		t.Fatalf("delete the skill: %v", err)
	}

	seamCalls := 0
	restore := setSkillReadSnapshotHook(func(ctx context.Context, fn func(*sql.Tx) error) error {
		seamCalls++
		return dm.withPackageReadSnapshot(ctx, fn)
	})
	defer restore()

	_, _, err := dm.ReadSkillWithPackageProvenance(context.Background(), imported.SkillID, "")
	if err == nil {
		t.Fatalf("the read of a deleted skill succeeded")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("the read failed with %v, want the not-found answer", err)
	}
	if seamCalls != 1 {
		t.Fatalf("the exported read opened %d snapshot(s), want exactly 1", seamCalls)
	}
}

// p6fuPackageSnapshot captures the rows a packaged skill occupies, so a
// test can put them back.
//
// It captures the SKILL row and the PACKAGE row. The file rows are not
// captured: the reader does not need them to answer "is there a
// package, and what is its provenance", and a restore that put them
// back would make the fixture longer without making the test stronger.
type p6fuPackageSnapshot struct {
	skillID string

	skillID2       string
	skillColl      string
	skillContent   string
	skillTags      string
	skillMeta      string
	skillGlobal    int
	skillPrime     int
	skillWeight    float64
	skillCreatedAt string

	pkgSkillID    string
	pkgName       string
	pkgVersion    string
	pkgDeclared   sql.NullString
	pkgVersionSrc string
	pkgFormat     string
	pkgEntrypoint string
	pkgEntrySHA   string
	pkgManifest   string
	pkgHash       string
	pkgSourceSHA  sql.NullString
	pkgGlobal     int
	pkgCreatedAt  int64
	pkgUpdatedAt  sql.NullInt64
}

// p6fuCapturePackage captures the rows of a packaged skill.
func p6fuCapturePackage(t *testing.T, dm *DatabaseManager, skillID string) p6fuPackageSnapshot {
	t.Helper()
	var snap p6fuPackageSnapshot
	snap.skillID = skillID
	err := dm.db.QueryRow(`
		SELECT id, collection, content, tags, metadata, is_global, is_prime_directive,
		       weight, created_at
		FROM memories WHERE id = ?`, skillID).Scan(
		&snap.skillID2, &snap.skillColl, &snap.skillContent, &snap.skillTags, &snap.skillMeta,
		&snap.skillGlobal, &snap.skillPrime, &snap.skillWeight, &snap.skillCreatedAt)
	if err != nil {
		t.Fatalf("capture the skill row: %v", err)
	}
	err = dm.db.QueryRow(`
		SELECT skill_id, name, version, declared_version, version_source, format,
		       entrypoint, entrypoint_sha256, manifest, package_hash, source_sha256,
		       is_global, created_at, updated_at
		FROM skill_packages WHERE skill_id = ?`, skillID).Scan(
		&snap.pkgSkillID, &snap.pkgName, &snap.pkgVersion, &snap.pkgDeclared,
		&snap.pkgVersionSrc, &snap.pkgFormat, &snap.pkgEntrypoint, &snap.pkgEntrySHA,
		&snap.pkgManifest, &snap.pkgHash, &snap.pkgSourceSHA, &snap.pkgGlobal,
		&snap.pkgCreatedAt, &snap.pkgUpdatedAt)
	if err != nil {
		t.Fatalf("capture the package row: %v", err)
	}
	return snap
}

// delete removes the skill row — and, by the foreign key, the package
// row with it.
// cycle removes the packaged skill and puts it back, ATOMICALLY.
//
// The removal and the restoration happen in ONE transaction, so the
// pair appears and disappears TOGETHER — which is what the foreign
// key's cascade does on a real deletion. Restoring the two rows in
// separate statements would create an intermediate state the substrate
// cannot produce: a skill row WITHOUT its package row. That state is
// what the reader must never observe, and manufacturing it here would
// test the fixture rather than the code.
//
// The two statements inside the transaction are separated in time by
// the transaction's own work, so the reader still meets the "gone"
// state and the "present" state at the boundaries — which is the
// interleaving this test is for.
func (s p6fuPackageSnapshot) cycle(dm *DatabaseManager) error {
	tx, err := dm.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM memories WHERE id = ?`, s.skillID); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO memories (id, collection, content, tags, metadata, is_global,
		                      is_prime_directive, weight, created_at)
		VALUES (?, ?, CAST(? AS TEXT), CAST(? AS TEXT), CAST(? AS TEXT), ?, ?, ?, ?)`,
		s.skillID2, s.skillColl, s.skillContent, s.skillTags, s.skillMeta,
		s.skillGlobal, s.skillPrime, s.skillWeight, s.skillCreatedAt); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO skill_packages (skill_id, name, version, declared_version, version_source,
		                            format, entrypoint, entrypoint_sha256, manifest, package_hash,
		                            source_sha256, is_global, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, CAST(? AS TEXT), ?, ?, ?, ?, ?)`,
		s.pkgSkillID, s.pkgName, s.pkgVersion, s.pkgDeclared, s.pkgVersionSrc,
		s.pkgFormat, s.pkgEntrypoint, s.pkgEntrySHA, s.pkgManifest, s.pkgHash,
		s.pkgSourceSHA, s.pkgGlobal, s.pkgCreatedAt, s.pkgUpdatedAt); err != nil {
		return err
	}
	return tx.Commit()
}

// TestP6FollowupAConcurrentDeletionNeverReturnsABodyWithoutItsProvenance
// pins the invariant under REAL concurrency.
//
// A deleter removes a packaged skill — and, by the cascade, its package
// row — and puts it back, over and over, while a reader reads it. Every
// read must land on one of exactly two answers:
//
//   - the skill is gone — the not-found answer; or
//   - the skill AND its provenance are both present.
//
// A body WITHOUT its provenance is never an acceptable answer, because
// the provenance is mandatory for a packaged skill.
//
// THE DETERMINISTIC GUARD IS ELSEWHERE. This test exercises the REAL
// path — the exported read, taking its own snapshot — so it is a
// probabilistic detector by nature: it can only catch a regression when
// a deletion happens to land in the window between the reader's two
// reads. The DETERMINISTIC guard is
// `TestP6FollowupTheReadIsOneSnapshot`, which drives the exported read
// through the snapshot seam and asserts both that the seam was used —
// so the exported read really took ONE snapshot — and that the result is
// a consistent pair.
//
// This test is the supplementary real-path check, and its iteration
// count is set so that a regression is caught with high probability
// rather than by luck.
func TestP6FollowupAConcurrentDeletionNeverReturnsABodyWithoutItsProvenance(t *testing.T) {
	dm := p3DM(t)

	// Several packages, so the reader keeps meeting packages that are
	// still there while the deleter works through them.
	const count = 4
	ids := make([]string, 0, count)
	for i := 0; i < count; i++ {
		imported := p6ImportPackage(t, dm, "racy"+string(rune('a'+i)), "1.0.0", map[string][]byte{
			"references/templates.md": []byte("the reference body\n"),
		})
		ids = append(ids, imported.SkillID)
	}
	snapshots := make([]p6fuPackageSnapshot, 0, count)
	for _, id := range ids {
		snapshots = append(snapshots, p6fuCapturePackage(t, dm, id))
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// The deleter: remove each packaged skill and put it back, in a
	// TIGHT loop with no sleep, and ATOMICALLY — the pair appears and
	// disappears together, which is what the foreign key's cascade
	// does on a real deletion. The pause the earlier version of this
	// fixture had meant the package was gone for a tiny fraction of the
	// reader's reads, so the race was almost never hit — which is why
	// this fixture now restores the rows it removes.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			for i := range snapshots {
				select {
				case <-stop:
					return
				default:
				}
				_ = snapshots[i].cycle(dm)
			}
		}
	}()

	pairs, notFound := 0, 0
	for i := 0; i < 1500; i++ {
		for _, id := range ids {
			skill, provenance, err := dm.ReadSkillWithPackageProvenance(context.Background(), id, "")
			if err != nil {
				// The not-found answer: the skill was gone for this
				// read. Any other error is a failure.
				if !strings.Contains(err.Error(), "not found") {
					t.Errorf("the read failed with %v, want the not-found answer", err)
				}
				notFound++
				continue
			}
			if skill == nil {
				t.Fatalf("the read returned neither a skill nor an error")
			}
			if provenance == nil {
				t.Fatalf("the read of %s returned a body with NO provenance; "+
					"the skill and its provenance came from different snapshots", id)
			}
			pairs++
		}
	}
	close(stop)
	wg.Wait()

	if pairs == 0 {
		t.Skip("every read met the not-found answer; the race did not interleave")
	}
	t.Logf("%d reads returned a consistent pair, %d returned the not-found answer", pairs, notFound)
}

// ─── The hot path's cost ───────────────────────────────────────────────────

// BenchmarkP6FollowupReadSkillWithProvenance measures the combined read.
//
// The P6 accessor took ONE statement and no transaction, and it was
// deliberately cheap. The combined read takes a transaction, so the
// question this answers is: how much did that cost?
func BenchmarkP6FollowupReadSkillWithProvenance(b *testing.B) {
	p6fuWorkspace(b)
	dm := p6fuDM(b)
	imported := p6fuImport(b, dm, "bench", "1.0.0", map[string][]byte{
		"references/templates.md": []byte("the reference body\n"),
	})
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := dm.ReadSkillWithPackageProvenance(ctx, imported.SkillID, ""); err != nil {
			b.Fatalf("read: %v", err)
		}
	}
}

// BenchmarkP6FollowupReadSkillOldShape measures the shape the follow-up
// REPLACED — the P6 hot path exactly.
//
// The P6 accessor was a SINGLE statement with no transaction, so the
// fair before/after comparison is against that, not against the
// two-call shape of the current accessor.
func BenchmarkP6FollowupReadSkillOldShape(b *testing.B) {
	p6fuWorkspace(b)
	dm := p6fuDM(b)
	imported := p6fuImport(b, dm, "bench", "1.0.0", map[string][]byte{
		"references/templates.md": []byte("the reference body\n"),
	})
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		skill, err := dm.ReadSkill(imported.SkillID, "")
		if err != nil {
			b.Fatalf("read the skill: %v", err)
		}
		// The P6 accessor's statement, verbatim.
		var (
			entrypoint, entrypointSHA, packageHash string
			declaredVersion                        sql.NullString
			version, versionSource                 string
			fileCount                              int
			secretScanned, poisonScanned           sql.NullInt64
		)
		err = dm.db.QueryRowContext(ctx, packageProvenanceQuery, skill.ID).Scan(
			&entrypoint, &entrypointSHA, &packageHash,
			&declaredVersion, &version, &versionSource,
			&fileCount, &secretScanned, &poisonScanned)
		if err != nil {
			b.Fatalf("read the provenance: %v", err)
		}
	}
}

// BenchmarkP6FollowupReadSkillTwoStatements measures the shape the
// combined read replaced: the skill read and the provenance read as two
// independent operations.
//
// It is the comparison the hot-path measurement is stated against.
func BenchmarkP6FollowupReadSkillTwoStatements(b *testing.B) {
	p6fuWorkspace(b)
	dm := p6fuDM(b)
	imported := p6fuImport(b, dm, "bench", "1.0.0", map[string][]byte{
		"references/templates.md": []byte("the reference body\n"),
	})
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		skill, err := dm.ReadSkill(imported.SkillID, "")
		if err != nil {
			b.Fatalf("read the skill: %v", err)
		}
		if _, err := dm.SkillPackageProvenance(ctx, skill.ID); err != nil {
			b.Fatalf("read the provenance: %v", err)
		}
	}
}

// ─── The components of the combined read ───────────────────────────────────
//
// The hot-path table in `docs/SPEC.md` §6.12 quotes the cost of the
// combined read AND its parts. A quoted number that nothing measures
// is a claim, not a measurement, so each part has its own benchmark
// here. The four together account for the difference between the P6
// hot path and the combined read.

// BenchmarkP6FollowupComponentTransaction measures the snapshot itself:
// a BEGIN, a rollback, and the connection checkout they need.
//
// It is the part of the combined read that the P6 hot path did NOT
// pay, because the P6 accessor was a single statement.
func BenchmarkP6FollowupComponentTransaction(b *testing.B) {
	p6fuWorkspace(b)
	dm := p6fuDM(b)
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := dm.withPackageReadSnapshot(ctx, func(tx *sql.Tx) error {
			return nil
		}); err != nil {
			b.Fatalf("open the snapshot: %v", err)
		}
	}
}

// BenchmarkP6FollowupComponentManifestSelect measures the manifest
// statement alone: the `SELECT manifest` the recorded projection hash
// requires.
func BenchmarkP6FollowupComponentManifestSelect(b *testing.B) {
	p6fuWorkspace(b)
	dm := p6fuDM(b)
	imported := p6fuImport(b, dm, "bench", "1.0.0", map[string][]byte{
		"references/templates.md": []byte("the reference body\n"),
	})
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var manifestJSON string
		if err := dm.db.QueryRowContext(ctx, packageManifestQuery, imported.SkillID).
			Scan(&manifestJSON); err != nil {
			b.Fatalf("read the manifest: %v", err)
		}
	}
}

// BenchmarkP6FollowupComponentManifestParse measures the manifest PARSE
// alone: the JSON unmarshal, with no database access.
func BenchmarkP6FollowupComponentManifestParse(b *testing.B) {
	p6fuWorkspace(b)
	dm := p6fuDM(b)
	imported := p6fuImport(b, dm, "bench", "1.0.0", map[string][]byte{
		"references/templates.md": []byte("the reference body\n"),
	})

	var manifestJSON string
	if err := dm.db.QueryRow(packageManifestQuery, imported.SkillID).
		Scan(&manifestJSON); err != nil {
		b.Fatalf("read the manifest: %v", err)
	}
	payload := []byte(manifestJSON)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := ParsePackageManifest(payload); err != nil {
			b.Fatalf("parse the manifest: %v", err)
		}
	}
}

// BenchmarkP6FollowupComponentManifestValidate measures the manifest
// VALIDATION alone: the manifest module's own rules, applied to an
// already-parsed manifest.
//
// It is the check that turns a recorded value into a validated one, so
// its cost is the price of the `recorded` origin.
func BenchmarkP6FollowupComponentManifestValidate(b *testing.B) {
	p6fuWorkspace(b)
	dm := p6fuDM(b)
	imported := p6fuImport(b, dm, "bench", "1.0.0", map[string][]byte{
		"references/templates.md": []byte("the reference body\n"),
	})

	var manifestJSON string
	if err := dm.db.QueryRow(packageManifestQuery, imported.SkillID).
		Scan(&manifestJSON); err != nil {
		b.Fatalf("read the manifest: %v", err)
	}
	manifest, _, err := ParsePackageManifest([]byte(manifestJSON))
	if err != nil {
		b.Fatalf("parse the manifest: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := manifest.Validate(); err != nil {
			b.Fatalf("validate the manifest: %v", err)
		}
	}
}

// TestP6FollowupTheAccessorReportsAnUnknownBodyAsUnavailable pins the
// difference between an EMPTY body and an UNKNOWN one.
//
// The body hash is the one hash the caller can check on its own: it
// covers the exact bytes the response carries. The hash of an EMPTY
// body is well defined — it is the hash of the empty byte string — so
// an empty body is a body with a hash.
//
// An UNKNOWN body is a different thing: a document whose frontmatter
// does not parse has no body to hash at all. Reporting the hash of
// the empty string there would be a claim about a body that was never
// found, and the origin would say "recomputed" about it. So the
// origin is `unavailable` and the hash is empty.
func TestP6FollowupTheAccessorReportsAnUnknownBodyAsUnavailable(t *testing.T) {
	dm := p6fuDM(t)
	imported := p6fuImport(t, dm, "unknownbody", "1.0.0", map[string][]byte{
		"references/templates.md": []byte("the reference body\n"),
	})

	// The stored projection is replaced with a document whose
	// frontmatter does not parse — a state the importer cannot
	// produce, and the one that makes the body unknown.
	//
	// The manifest's RECORDED projection hash is updated with it, so
	// the fixture describes a package that is INTACT: the accessor
	// fails closed on a projection that contradicts the manifest, and
	// this test is about what it reports when the two AGREE and the
	// document still has no body.
	corrupted := "this is not a frontmatter document\n"
	if _, err := dm.db.Exec(`UPDATE memories SET content = ? WHERE id = ?`,
		corrupted, imported.SkillID); err != nil {
		t.Fatalf("corrupt the stored projection: %v", err)
	}
	if _, err := dm.db.Exec(
		`UPDATE skill_packages SET manifest = json_set(manifest, '$.projection_sha256', ?) WHERE skill_id = ?`,
		p6fuDigest([]byte(corrupted)), imported.SkillID); err != nil {
		t.Fatalf("update the manifest's recorded projection hash: %v", err)
	}

	provenance, err := dm.SkillPackageProvenance(context.Background(), imported.SkillID)
	if err != nil {
		t.Fatalf("read the provenance: %v", err)
	}
	if provenance == nil {
		t.Fatalf("the provenance is absent for a packaged skill")
	}
	if provenance.HashOrigins.BodySHA256 != PackageHashOriginUnavailable {
		t.Errorf("the body hash's origin is %q, want %q",
			provenance.HashOrigins.BodySHA256, PackageHashOriginUnavailable)
	}
	if provenance.BodySHA256 != "" {
		t.Errorf("the body hash is %q, want an empty string for an unknown body",
			provenance.BodySHA256)
	}
}

// TestP6FollowupTheAccessorHashesAnEmptyBody pins the other half.
//
// A document whose frontmatter parses but whose body is EMPTY is a
// body with a hash: the hash of the empty byte string. The origin is
// `recomputed`, because the hash really was recomputed over the bytes
// the accessor returned.
func TestP6FollowupTheAccessorHashesAnEmptyBody(t *testing.T) {
	dm := p6fuDM(t)
	imported := p6fuImport(t, dm, "emptybody", "1.0.0", map[string][]byte{
		"references/templates.md": []byte("the reference body\n"),
	})

	// The projection keeps its frontmatter and loses its body.
	fm := "---\nname: emptybody\ndescription: a test skill\nwhen_to_use: when a test needs this exact skill\nversion: 1.0.0\n---\n"
	if _, err := dm.db.Exec(`UPDATE memories SET content = ? WHERE id = ?`,
		fm, imported.SkillID); err != nil {
		t.Fatalf("rewrite the stored projection: %v", err)
	}
	// The manifest's recorded projection hash follows, so the package
	// stays intact and the accessor reports on it.
	if _, err := dm.db.Exec(
		`UPDATE skill_packages SET manifest = json_set(manifest, '$.projection_sha256', ?) WHERE skill_id = ?`,
		p6fuDigest([]byte(fm)), imported.SkillID); err != nil {
		t.Fatalf("update the manifest's recorded projection hash: %v", err)
	}

	provenance, err := dm.SkillPackageProvenance(context.Background(), imported.SkillID)
	if err != nil {
		t.Fatalf("read the provenance: %v", err)
	}
	if provenance == nil {
		t.Fatalf("the provenance is absent for a packaged skill")
	}
	if provenance.HashOrigins.BodySHA256 != PackageHashOriginRecomputed {
		t.Errorf("the body hash's origin is %q, want %q",
			provenance.HashOrigins.BodySHA256, PackageHashOriginRecomputed)
	}
	if provenance.BodySHA256 != p6fuDigest([]byte("")) {
		t.Errorf("the body hash is %q, want the hash of the empty byte string (%q)",
			provenance.BodySHA256, p6fuDigest([]byte("")))
	}
}

// BenchmarkP6FollowupComponentManifestSelectInSnapshot measures the
// manifest `SELECT` issued INSIDE a snapshot.
//
// The same statement on the pool is
// `BenchmarkP6FollowupComponentManifestSelect`. The difference between
// the two is the per-statement cost a transaction adds — and it is the
// part of the combined read's overhead that the other three components
// do not account for.
//
// The statement is the SAME one the combined read issues, so the
// comparison is like for like.
func BenchmarkP6FollowupComponentManifestSelectInSnapshot(b *testing.B) {
	p6fuWorkspace(b)
	dm := p6fuDM(b)
	imported := p6fuImport(b, dm, "bench", "1.0.0", map[string][]byte{
		"references/templates.md": []byte("the reference body\n"),
	})
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		err := dm.withPackageReadSnapshot(ctx, func(tx *sql.Tx) error {
			var manifestJSON string
			if err := tx.QueryRowContext(ctx, packageManifestQuery, imported.SkillID).
				Scan(&manifestJSON); err != nil {
				return err
			}
			return nil
		})
		if err != nil {
			b.Fatalf("read the manifest in the snapshot: %v", err)
		}
	}
}
