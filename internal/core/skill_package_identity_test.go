// skill_package_identity_test.go — the B1 identity-race regressions.
//
// ─────────────────────────────────────────────────────────────────────────────
// WHAT THESE TESTS ARE FOR
// ─────────────────────────────────────────────────────────────────────────────
//
// The first B1 remediation held the import root OPEN as a directory
// descriptor and issued every inspection and every read relative to that
// descriptor (os.Root). That closed the ORIGINAL exploit — replacing the
// resolved root with a SYMLINK.
//
// It did not close the CLASS, because the identity the anchor compared
// against was re-read from the path at open time:
//
//	expected (os.Lstat, path)  →  actual (os.Root, descriptor-relative)
//
// Both sides of that comparison were observations taken AFTER the
// substitution. If the path was re-pointed at a different REAL
// directory — not a symlink — the two observations agreed with each
// other, and the importer adopted the substituted directory as if the
// operator had named it.
//
// The fix is to CAPTURE the identity at RESOLUTION — before the scanner
// preflight, which is where the importer blocks and where the
// substitution lands — and to compare the opened descriptor against THAT
// capture:
//
//	original resolved identity → scanner preflight → opened descriptor
//	    → identity comparison → confined traversal
//
// Each test below is one shape of substitution. The rendezvous is the
// same FIFO the first remediation used, and the same one the independent
// review used: `planPackageImport` resolves the import root FIRST and
// reads the scanner configuration SECOND, so a FIFO at the toxic-phrase
// path is a rendezvous that lands the substitution in exactly that
// window — every time, not intermittently.
//
// Every fixture is disposable: a t.TempDir() tree and a disposable
// database.
package internal

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// THE RENDEZVOUS
// ─────────────────────────────────────────────────────────────────────────────

// p4EvictPoisonPhraseCache evicts a manager's poison-phrase list from the
// process-wide cache.
//
// It exists for the rendezvous: the FIFO rendezvous only works when the
// importer actually OPENS the phrase file, and the importer only opens
// it when the cache is cold. The P4 tests do not run in parallel, so
// evicting one workspace's entry is contained.
func p4EvictPoisonPhraseCache(t *testing.T, dm *DatabaseManager) {
	t.Helper()
	path := dm.poisonPhrasesPath()
	poisonPhraseMu.Lock()
	defer poisonPhraseMu.Unlock()
	delete(poisonPhraseCache, path)
	delete(poisonPhraseErr, path)
}

// p4IdentityRaceImport runs an import whose root is substituted while the
// importer is blocked in the scanner preflight, and returns the import's
// error.
//
// `swap` is called while the importer waits on the FIFO, which is after
// the root has been resolved and before the anchor is opened — the window
// the whole identity chain exists to protect.
func p4IdentityRaceImport(t *testing.T, dm *DatabaseManager, root string, swap func(t *testing.T)) error {
	t.Helper()

	// The scanner configuration becomes a FIFO: the rendezvous.
	phrases := dm.poisonPhrasesPath()
	_ = os.Remove(phrases)
	if err := syscall.Mkfifo(phrases, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	// The rendezvous needs a COLD poison-phrase cache.
	//
	// `loadPoisonPhrasesAt` caches the phrase list per path. A cache hit
	// means the importer never opens the FIFO at all — it reads the
	// cached list and moves on — so the rendezvous would never happen
	// and the test would measure nothing. The pre-existing row these
	// tests create goes through `SaveSkill`, which scans its content
	// against the phrase list and therefore populates the cache, so the
	// entry is evicted here.
	p4EvictPoisonPhraseCache(t, dm)

	done := make(chan error, 1)
	go func() {
		_, err := dm.ImportSkillPackageFromDir(PackageImportOptions{Path: root})
		done <- err
	}()

	// This open returns exactly when the importer's reader arrives.
	w, err := os.OpenFile(phrases, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open the fifo for writing: %v", err)
	}
	// Let the importer settle into the blocked read.
	time.Sleep(150 * time.Millisecond)

	// THE SUBSTITUTION.
	swap(t)

	// Release the importer. A non-empty phrase list means the loader does
	// not try to seed the defaults back into the FIFO.
	if _, err := w.WriteString("a custom toxic phrase\n"); err != nil {
		t.Fatalf("write to the fifo: %v", err)
	}
	_ = w.Close()

	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		t.Fatal("the import never completed")
		return nil
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// THE ASSERTIONS
// ─────────────────────────────────────────────────────────────────────────────

// p4IdentityRaceCensus is the row census of the tables an import writes.
type p4IdentityRaceCensus struct {
	packages int
	files    int
	blobs    int
	skills   int
}

// p4IdentityRaceCensusOf reads the census.
func p4IdentityRaceCensusOf(t *testing.T, dm *DatabaseManager) p4IdentityRaceCensus {
	t.Helper()
	c := p4IdentityRaceCensus{}
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM skill_packages`).Scan(&c.packages); err != nil {
		t.Fatalf("count the package rows: %v", err)
	}
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM skill_package_files`).Scan(&c.files); err != nil {
		t.Fatalf("count the file rows: %v", err)
	}
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM skill_package_blobs`).Scan(&c.blobs); err != nil {
		t.Fatalf("count the blob rows: %v", err)
	}
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE collection = 'skills'`).Scan(&c.skills); err != nil {
		t.Fatalf("count the skill rows: %v", err)
	}
	return c
}

// p4IdentityRaceOutcome asserts that a substitution was refused, that
// nothing from outside the resolved root was stored, and that the rows
// that existed before the import are untouched.
//
// `before` is the census taken before the import; the census after it
// must be IDENTICAL — a refused import writes nothing at all.
//
// `preExistingID` is the id of a row created before the import; its
// content, metadata, weight and reinforcement count must be exactly what
// they were.
func p4IdentityRaceOutcome(t *testing.T, dm *DatabaseManager, err error, before p4IdentityRaceCensus, preExistingID, preExistingContent string) {
	t.Helper()

	if err == nil {
		t.Fatal("the import SUCCEEDED: the substitution escaped the import root")
	}
	t.Logf("the import was refused: %v (reason %q)", err, PackageImportReasonOf(err))

	// The refusal carries a STABLE reason code, and it is the IDENTITY
	// check that refused it — not an incidental failure such as a
	// missing directory.
	if got := PackageImportReasonOf(err); got != PackageImportReasonChangedDuringRead {
		t.Errorf("the refusal reason = %q, want %q", got, PackageImportReasonChangedDuringRead)
	}

	// Nothing survives: the census is IDENTICAL to the one taken before
	// the import. No package, file, blob or projection row was added,
	// and none was removed.
	after := p4IdentityRaceCensusOf(t, dm)
	if after != before {
		t.Errorf("the refused import changed the row census: before=%+v after=%+v", before, after)
	}

	// And specifically: the outside file was not stored under any path.
	var n int
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM skill_package_files WHERE path = ?`,
		"stolen.md").Scan(&n); err != nil {
		t.Fatalf("count the stolen file row: %v", err)
	}
	if n != 0 {
		t.Errorf("the file from OUTSIDE the resolved root was stored as a package file")
	}

	// And no blob carries the outside content.
	var blobs int
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM skill_package_blobs WHERE content LIKE ?`,
		"%THIS CAME FROM OUTSIDE THE ROOT%").Scan(&blobs); err != nil {
		t.Fatalf("count the outside blobs: %v", err)
	}
	if blobs != 0 {
		t.Errorf("%d blob(s) carry the outside content", blobs)
	}

	// And no projected skill carries the outside body.
	var skills int
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE content LIKE ?`,
		"%THIS CAME FROM OUTSIDE THE ROOT%").Scan(&skills); err != nil {
		t.Fatalf("count the outside projections: %v", err)
	}
	if skills != 0 {
		t.Errorf("%d projected skill row(s) carry the outside content", skills)
	}

	// The pre-existing row is untouched.
	if preExistingID == "" {
		return
	}
	var content, meta string
	var weight, reinforcement int64
	if err := dm.db.QueryRow(`
		SELECT content, COALESCE(metadata,'{}'), weight, reinforcement_count
		FROM memories WHERE id = ?`, preExistingID).
		Scan(&content, &meta, &weight, &reinforcement); err != nil {
		t.Fatalf("read the pre-existing row: %v", err)
	}
	if content != preExistingContent {
		t.Errorf("the refused import MODIFIED the pre-existing row:\n%s", content)
	}
	t.Logf("the pre-existing row is unchanged (weight=%d reinforcement_count=%d)", weight, reinforcement)
}

// p4IdentityRacePreexisting creates a row that the refused import must
// leave untouched, and returns its id and content.
func p4IdentityRacePreexisting(t *testing.T, dm *DatabaseManager) (string, string) {
	t.Helper()
	content := p4SkillDoc("preexisting", "1.0.0", "a row that existed before the import\n")
	id, err := dm.SaveSkill("preexisting", "1.0.0", content, "the-original-author", false)
	if err != nil {
		t.Fatalf("save the pre-existing skill: %v", err)
	}
	return id, content
}

// p4IdentityRaceOutsidePkg writes the substituted package — a valid
// `SKILL.md` plus a `stolen.md` — into `dir`.
//
// The fixture is a COMPLETE, importable package, so an import that
// reaches it does not fail for an unrelated reason (a missing
// entrypoint, a missing directory). The refusal these tests assert must
// come from the identity check alone.
func p4IdentityRaceOutsidePkg(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	entry := p4SkillDoc("escaped", "1.0.0", "THIS CAME FROM OUTSIDE THE ROOT\n")
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(entry), 0o600); err != nil {
		t.Fatalf("write the outside entrypoint: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stolen.md"), []byte("# stolen\n"), 0o600); err != nil {
		t.Fatalf("write the outside file: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// CASE 1 — the root replaced by a DIFFERENT REAL DIRECTORY
// ─────────────────────────────────────────────────────────────────────────────

// TestP4_B1_ARootReplacedByADifferentRealDirectoryIsRefused is the
// non-symlink shape of the identity race.
//
// The substituted directory is a REAL directory with the same shape as
// the operator's. The path still names a directory, every later
// observation of that path agrees with every other, and nothing about
// the filesystem says "this was swapped" — so the ONLY thing that can
// detect the substitution is a comparison against the identity CAPTURED
// at resolution.
func TestP4_B1_ARootReplacedByADifferentRealDirectoryIsRefused(t *testing.T) {
	dm := p3DM(t)

	base := t.TempDir()
	root := filepath.Join(base, "pkg")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	entry := p4SkillDoc("original", "1.0.0", "the ORIGINAL body\n")
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte(entry), 0o600); err != nil {
		t.Fatalf("write the original entrypoint: %v", err)
	}

	// The substituted package.
	evil := filepath.Join(base, "evil")
	p4IdentityRaceOutsidePkg(t, evil)

	preID, preContent := p4IdentityRacePreexisting(t, dm)
	before := p4IdentityRaceCensusOf(t, dm)

	err := p4IdentityRaceImport(t, dm, root, func(t *testing.T) {
		// THE SUBSTITUTION: the root becomes a DIFFERENT REAL
		// DIRECTORY. No symlink is involved anywhere.
		if err := os.Rename(root, filepath.Join(base, "pkg-original")); err != nil {
			t.Fatalf("rename the root away: %v", err)
		}
		if err := os.Rename(evil, root); err != nil {
			t.Fatalf("rename the substituted directory into place: %v", err)
		}
	})

	p4IdentityRaceOutcome(t, dm, err, before, preID, preContent)
}

// ─────────────────────────────────────────────────────────────────────────────
// CASE 2 — the ANCESTOR replaced by a DIFFERENT REAL DIRECTORY
// ─────────────────────────────────────────────────────────────────────────────

// TestP4_B1_AnAncestorReplacedByADifferentRealDirectoryIsRefused replaces
// the ANCESTOR of the import root.
//
// The root's own path (`…/parent/pkg`) is untouched; what changes is the
// directory above it. The resolved path therefore still resolves, still
// names a directory, and still contains a `SKILL.md` — the substitution
// is invisible to every path-based observation.
func TestP4_B1_AnAncestorReplacedByADifferentRealDirectoryIsRefused(t *testing.T) {
	dm := p3DM(t)

	base := t.TempDir()
	parent := filepath.Join(base, "parent")
	root := filepath.Join(parent, "pkg")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	entry := p4SkillDoc("original", "1.0.0", "the ORIGINAL body\n")
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte(entry), 0o600); err != nil {
		t.Fatalf("write the original entrypoint: %v", err)
	}

	// The substituted ANCESTOR — a real directory whose `pkg/` subtree
	// matches the operator's exactly, so the substituted path resolves
	// to a complete, importable package.
	evilParent := filepath.Join(base, "evilparent")
	p4IdentityRaceOutsidePkg(t, filepath.Join(evilParent, "pkg"))

	preID, preContent := p4IdentityRacePreexisting(t, dm)
	before := p4IdentityRaceCensusOf(t, dm)

	err := p4IdentityRaceImport(t, dm, root, func(t *testing.T) {
		// THE SUBSTITUTION: the ancestor becomes a DIFFERENT REAL
		// DIRECTORY that holds a matching `pkg/` subtree.
		if err := os.Rename(parent, filepath.Join(base, "parent-original")); err != nil {
			t.Fatalf("rename the ancestor away: %v", err)
		}
		if err := os.Rename(evilParent, parent); err != nil {
			t.Fatalf("rename the substituted ancestor into place: %v", err)
		}
	})

	p4IdentityRaceOutcome(t, dm, err, before, preID, preContent)
}

// ─────────────────────────────────────────────────────────────────────────────
// CASE 3 — the ANCESTOR replaced by a SYMLINK to a matching outside tree
// ─────────────────────────────────────────────────────────────────────────────

// TestP4_B1_AnAncestorReplacedByASymlinkToAMatchingOutsideTreeIsRefused
// is the shape the review reported: the ancestor becomes a SYMLINK whose
// target holds a valid `pkg/` subtree.
//
// The target is a complete package, so the substituted path resolves,
// resolves to a directory, and contains an importable `SKILL.md`. A
// refusal caused by a missing directory would not prove anything here —
// this fixture makes the substitution reach the identity check.
func TestP4_B1_AnAncestorReplacedByASymlinkToAMatchingOutsideTreeIsRefused(t *testing.T) {
	dm := p3DM(t)

	base := t.TempDir()
	parent := filepath.Join(base, "parent")
	root := filepath.Join(parent, "pkg")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	entry := p4SkillDoc("original", "1.0.0", "the ORIGINAL body\n")
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte(entry), 0o600); err != nil {
		t.Fatalf("write the original entrypoint: %v", err)
	}

	// The outside tree, with a MATCHING `pkg/` subtree.
	outside := filepath.Join(base, "outside")
	p4IdentityRaceOutsidePkg(t, filepath.Join(outside, "pkg"))

	preID, preContent := p4IdentityRacePreexisting(t, dm)
	before := p4IdentityRaceCensusOf(t, dm)

	err := p4IdentityRaceImport(t, dm, root, func(t *testing.T) {
		// THE SUBSTITUTION: the ancestor becomes a symlink to the
		// outside tree, which holds a matching `pkg/` subtree.
		if err := os.Rename(parent, filepath.Join(base, "parent-original")); err != nil {
			t.Fatalf("rename the ancestor away: %v", err)
		}
		if err := os.Symlink(outside, parent); err != nil {
			t.Fatalf("symlink the ancestor: %v", err)
		}
	})

	p4IdentityRaceOutcome(t, dm, err, before, preID, preContent)
}

// ─────────────────────────────────────────────────────────────────────────────
// THE STANDALONE `SKILL.md` — the same race, on the one-file shape
// ─────────────────────────────────────────────────────────────────────────────

// TestP4_B1_AStandaloneSkillFileReplacedDuringThePreflightIsRefused
// covers the one-file package.
//
// The anchor for a standalone `SKILL.md` is the file's PARENT directory,
// so the parent is the thing that must be verified. The file is
// substituted in place — same name, same parent path, different inode —
// and the refusal must come from the identity comparison.
func TestP4_B1_AStandaloneSkillFileReplacedDuringThePreflightIsRefused(t *testing.T) {
	dm := p3DM(t)

	base := t.TempDir()
	dir := filepath.Join(base, "pkg")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	entry := p4SkillDoc("original", "1.0.0", "the ORIGINAL body\n")
	target := filepath.Join(dir, packageEntrypointName)
	if err := os.WriteFile(target, []byte(entry), 0o600); err != nil {
		t.Fatalf("write the original entrypoint: %v", err)
	}

	// The substituted file: the same NAME in the same directory, but a
	// different inode.
	stolen := filepath.Join(base, "stolen-entrypoint.md")
	entry2 := p4SkillDoc("escaped", "1.0.0", "THIS CAME FROM OUTSIDE THE ROOT\n")
	if err := os.WriteFile(stolen, []byte(entry2), 0o600); err != nil {
		t.Fatalf("write the substituted entrypoint: %v", err)
	}

	preID, preContent := p4IdentityRacePreexisting(t, dm)

	err := p4IdentityRaceImport(t, dm, target, func(t *testing.T) {
		// THE SUBSTITUTION: the file is replaced in place by a
		// different inode carrying the outside body.
		if err := os.Rename(target, filepath.Join(base, "entrypoint-original")); err != nil {
			t.Fatalf("rename the entrypoint away: %v", err)
		}
		if err := os.Rename(stolen, target); err != nil {
			t.Fatalf("rename the substituted entrypoint into place: %v", err)
		}
	})

	if err == nil {
		t.Fatal("the import SUCCEEDED: the substituted entrypoint was accepted")
	}
	t.Logf("the import was refused: %v (reason %q)", err, PackageImportReasonOf(err))
	if got := PackageImportReasonOf(err); got != PackageImportReasonChangedDuringRead {
		t.Errorf("the refusal reason = %q, want %q", got, PackageImportReasonChangedDuringRead)
	}

	p4AssertNoPackageRowsStored(t, dm, "standalone identity race")

	// No projected skill carries the outside body.
	var skills int
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE content LIKE ?`,
		"%THIS CAME FROM OUTSIDE THE ROOT%").Scan(&skills); err != nil {
		t.Fatalf("count the outside projections: %v", err)
	}
	if skills != 0 {
		t.Errorf("%d projected skill row(s) carry the outside content", skills)
	}

	// The pre-existing row is untouched.
	var content string
	if err := dm.db.QueryRow(`SELECT content FROM memories WHERE id = ?`, preID).Scan(&content); err != nil {
		t.Fatalf("read the pre-existing row: %v", err)
	}
	if content != preContent {
		t.Errorf("the refused import MODIFIED the pre-existing row:\n%s", content)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// THE IDENTITY CHAIN ITSELF
// ─────────────────────────────────────────────────────────────────────────────

// TestP4_B1_TheIdentityIsCapturedBeforeTheScannerPreflight pins the
// ORDER of the chain, which is the thing the fix rests on.
//
// The identity must be captured BEFORE the scanner preflight. If it were
// captured after, the capture would be a second observation of the path
// and the substitution would be invisible — which is exactly the defect
// this remediation closes.
//
// The test observes the order through the FIFO: the importer blocks in
// the preflight, and the capture must already have happened by then.
func TestP4_B1_TheIdentityIsCapturedBeforeTheScannerPreflight(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "pkg")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"),
		[]byte(p4SkillDoc("original", "1.0.0", "body\n")), 0o600); err != nil {
		t.Fatalf("write the entrypoint: %v", err)
	}

	// The resolution captures the identity.
	resolved, identity, err := resolvePackageImportRoot(root)
	if err != nil {
		t.Fatalf("resolve the root: %v", err)
	}
	if identity.root == nil {
		t.Fatal("the resolution captured no identity")
	}
	t.Logf("the captured identity: name=%q dir=%v", identity.root.Name(), identity.root.IsDir())

	// The root is now replaced by a DIFFERENT REAL DIRECTORY — the
	// substitution the capture must detect.
	evil := filepath.Join(base, "evil")
	p4IdentityRaceOutsidePkg(t, evil)
	if err := os.Rename(root, filepath.Join(base, "pkg-original")); err != nil {
		t.Fatalf("rename the root away: %v", err)
	}
	if err := os.Rename(evil, root); err != nil {
		t.Fatalf("rename the substituted directory into place: %v", err)
	}

	// The anchor opens the SUBSTITUTED directory and compares it with
	// the CAPTURED identity. They disagree, so it refuses.
	_, err = openPackageImportAnchor(resolved, identity)
	if err == nil {
		t.Fatal("the anchor opened the SUBSTITUTED directory")
	}
	if got := PackageImportReasonOf(err); got != PackageImportReasonChangedDuringRead {
		t.Errorf("the refusal reason = %q, want %q", got, PackageImportReasonChangedDuringRead)
	}
	t.Logf("the anchor refused the substituted directory: %v", err)

	// And the message names the substitution, not something incidental.
	if !strings.Contains(err.Error(), "replaced") {
		t.Errorf("the refusal does not name the substitution: %v", err)
	}
}
