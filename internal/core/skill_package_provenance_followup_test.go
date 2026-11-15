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
	"time"
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

// TestP6FollowupTheReadIsOneSnapshot pins the invariant DETERMINISTICALLY.
//
// The test holds a read snapshot open, deletes the package CONCURRENTLY
// on another connection, and then runs the combined read INSIDE the held
// snapshot. The snapshot does not see the deletion, so the read returns
// the skill AND its provenance — a consistent pair.
//
// With two independent statements the same interleaving would return the
// skill with NO provenance, because the first statement would have seen
// the package and the second would not.
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

	// The held snapshot still sees the skill and its package.
	skill, provenance, err := readSkillWithPackageProvenanceTx(context.Background(), tx, imported.SkillID, "")
	if err != nil {
		t.Fatalf("the read inside the held snapshot failed: %v", err)
	}
	if skill == nil {
		t.Fatalf("the read inside the held snapshot returned no skill")
	}
	if provenance == nil {
		t.Fatalf("the read inside the held snapshot returned a skill with NO provenance; "+
			"the two reads came from different snapshots")
	}
	if provenance.EntrypointSHA256 != imported.EntrypointSHA256 {
		t.Errorf("the entrypoint hash is %s, want %s", provenance.EntrypointSHA256, imported.EntrypointSHA256)
	}
}

// TestP6FollowupAConcurrentDeletionNeverReturnsABodyWithoutItsProvenance
// pins the invariant under REAL concurrency.
//
// A deleter removes packaged skills (and, by cascade, their packages)
// while a reader reads them, repeatedly. Every read must land on one of
// exactly two answers:
//
//   - the skill is gone — the not-found answer; or
//   - the skill AND its provenance are both present.
//
// A body WITHOUT its provenance is never an acceptable answer, because
// the provenance is mandatory for a packaged skill.
func TestP6FollowupAConcurrentDeletionNeverReturnsABodyWithoutItsProvenance(t *testing.T) {
	dm := p3DM(t)

	// Several packages, so the reader keeps meeting packages that are
	// still there while the deleter works through them.
	const count = 8
	ids := make([]string, 0, count)
	for i := 0; i < count; i++ {
		imported := p6ImportPackage(t, dm, "racy"+string(rune('a'+i)), "1.0.0", map[string][]byte{
			"references/templates.md": []byte("the reference body\n"),
		})
		ids = append(ids, imported.SkillID)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// The deleter: shred each packaged skill in turn. The delete of the
	// `memories` row cascades to the package row, so the pair goes
	// together — the same way every real deletion path removes them.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, id := range ids {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
			if _, err := dm.db.Exec(`DELETE FROM memories WHERE id = ?`, id); err != nil {
				return
			}
		}
	}()

	pairs, notFound := 0, 0
	for i := 0; i < 600; i++ {
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
			declaredVersion                          sql.NullString
			version, versionSource                   string
			fileCount                                int
			secretScanned, poisonScanned             sql.NullInt64
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
