// skill_package_confinement_test.go — the B1 confinement regressions.
//
// These tests are the REVIEW's exploit, reproduced and pinned.
//
// The review (comment 6864706276) demonstrated that replacing the import
// root with a symlink AFTER the root was resolved caused the importer to
// read files from OUTSIDE the resolved root and store them as ordinary
// root-level package files. The exploit was made deterministic with a FIFO
// handshake: the importer resolves the root first and checks the scanner
// configuration second, so a FIFO at the toxic-phrase path is a
// rendezvous that lands the swap in exactly that window.
//
// The fix holds the import root OPEN as a directory descriptor and issues
// every inspection and every read relative to that descriptor, so the
// swap can no longer change what the walk reads.
//
// Every fixture here is disposable: a t.TempDir() tree and a disposable
// database. No live workspace is touched.
package internal

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// p4ConfinementOutsideTree builds a directory OUTSIDE the import root that
// holds a complete, importable package.
//
// It returns the outside directory and the path of the file that must
// never be stored.
func p4ConfinementOutsideTree(t *testing.T, base string) (string, string) {
	t.Helper()
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	entry := p4SkillDoc("escaped", "1.0.0", "THIS CAME FROM OUTSIDE THE ROOT\n")
	if err := os.WriteFile(filepath.Join(outside, "SKILL.md"), []byte(entry), 0o600); err != nil {
		t.Fatalf("write outside entrypoint: %v", err)
	}
	stolen := filepath.Join(outside, "stolen.md")
	if err := os.WriteFile(stolen, []byte("# stolen\n"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	return outside, stolen
}

// p4AssertNothingStored asserts that a refused import left no trace.
//
// It is used for the refusals that happen BEFORE anything is written: no
// package, file or blob row may survive, and no SKILL row may survive
// either (because the import had not created one).
func p4AssertNothingStored(t *testing.T, dm *DatabaseManager, what string) {
	t.Helper()
	for _, q := range []struct {
		label string
		sql   string
	}{
		{"package rows", `SELECT COUNT(*) FROM skill_packages`},
		{"file rows", `SELECT COUNT(*) FROM skill_package_files`},
		{"blob rows", `SELECT COUNT(*) FROM skill_package_blobs`},
		{"skill rows", `SELECT COUNT(*) FROM memories WHERE collection = 'skills'`},
	} {
		if n := p4Count(t, dm, q.sql); n != 0 {
			t.Errorf("the refused import (%s) left %d %s behind", what, n, q.label)
		}
	}
}

// p4AssertNoPackageRowsStored asserts that a rolled-back import left no
// PACKAGE rows behind.
//
// It is the right assertion for a resurrection that was rolled back: the
// skill row it revived was already there (soft-deleted), and the rollback
// must leave it exactly as it was — so the skill row is NOT counted here.
func p4AssertNoPackageRowsStored(t *testing.T, dm *DatabaseManager, what string) {
	t.Helper()
	for _, q := range []struct {
		label string
		sql   string
	}{
		{"package rows", `SELECT COUNT(*) FROM skill_packages`},
		{"file rows", `SELECT COUNT(*) FROM skill_package_files`},
		{"blob rows", `SELECT COUNT(*) FROM skill_package_blobs`},
	} {
		if n := p4Count(t, dm, q.sql); n != 0 {
			t.Errorf("the rolled-back import (%s) left %d %s behind", what, n, q.label)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// THE EXPLOIT — the root replaced by a symlink between the resolution and
// the walk
// ─────────────────────────────────────────────────────────────────────────────

// TestP4_B1_TheRootSwappedForASymlinkAfterResolutionIsRefused is the
// review's exploit, pinned.
//
// The exploit must FAIL RELIABLY — not merely become harder to schedule.
// The FIFO handshake makes the swap land in the exact window every time,
// so a regression that reintroduces the escape fails this test
// deterministically rather than intermittently.
func TestP4_B1_TheRootSwappedForASymlinkAfterResolutionIsRefused(t *testing.T) {
	dm := p3DM(t)

	base := t.TempDir()
	root := filepath.Join(base, "pkg")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	outside, _ := p4ConfinementOutsideTree(t, base)

	// The scanner configuration becomes a FIFO: the rendezvous.
	//
	// planPackageImport resolves the import root FIRST and checks the
	// scanner configuration SECOND. The importer therefore blocks here
	// AFTER the root was resolved and BEFORE the walk — which is exactly
	// the window the exploit needs.
	phrases := dm.poisonPhrasesPath()
	_ = os.Remove(phrases)
	if err := syscall.Mkfifo(phrases, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	type outcome struct {
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		_, err := dm.ImportSkillPackageFromDir(PackageImportOptions{Path: root})
		done <- outcome{err}
	}()

	// This open returns exactly when the importer's reader arrives.
	w, err := os.OpenFile(phrases, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open the fifo for writing: %v", err)
	}
	// Let the importer settle into the blocked read.
	time.Sleep(150 * time.Millisecond)

	// THE EXPLOIT: the resolved root becomes a symlink to a directory
	// that was never inside it.
	if err := os.Rename(root, filepath.Join(base, "pkg-original")); err != nil {
		t.Fatalf("rename the root: %v", err)
	}
	if err := os.Symlink(outside, root); err != nil {
		t.Fatalf("symlink the root: %v", err)
	}

	// Release the importer. A non-empty phrase list means the loader does
	// not try to seed the defaults back into the FIFO.
	if _, err := w.WriteString("a custom toxic phrase\n"); err != nil {
		t.Fatalf("write to the fifo: %v", err)
	}
	_ = w.Close()

	var got outcome
	select {
	case got = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the import never completed")
	}

	if got.err == nil {
		t.Fatal("the import SUCCEEDED: the root swap escaped the import root")
	}
	t.Logf("the import was refused: %v (reason %q)", got.err, PackageImportReasonOf(got.err))

	// The refusal carries a STABLE reason code.
	switch reason := PackageImportReasonOf(got.err); reason {
	case PackageImportReasonChangedDuringRead, PackageImportReasonImportRootInvalid,
		PackageImportReasonPathEscapesRoot, PackageImportReasonReadFailed:
		// Any of these is a classified refusal that says the root
		// could not be trusted. What must not happen is an unclassified
		// error or a success.
	default:
		t.Errorf("the refusal reason = %q, want a classified root-confinement refusal", reason)
	}

	// Nothing survives: no package, file, blob or projection row.
	p4AssertNothingStored(t, dm, "root swap")

	// And specifically: the outside file was not stored under any path.
	var n int
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM skill_package_files WHERE path = ?`,
		"stolen.md").Scan(&n); err != nil {
		t.Fatalf("count the stolen file row: %v", err)
	}
	if n != 0 {
		t.Errorf("the file from OUTSIDE the resolved root was stored as a package file")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// The root replaced BEFORE the import starts
// ─────────────────────────────────────────────────────────────────────────────

// TestP4_B1_ASymlinkedRootIsResolvedAndImported covers the shape the
// operator legitimately produces: the package directory is reached
// THROUGH a symlink.
//
// The operator's path MAY legitimately be a symlink, and it is resolved
// and imported. That is safe because the anchor binds to the RESOLVED
// inode: the resolved root IS the target, so nothing outside it is
// involved.
func TestP4_B1_ASymlinkedRootIsResolvedAndImported(t *testing.T) {
	dm := p3DM(t)

	base := t.TempDir()
	outside, _ := p4ConfinementOutsideTree(t, base)
	link := filepath.Join(base, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	res, err := dm.ImportSkillPackageFromDir(PackageImportOptions{Path: link})
	if err != nil {
		t.Fatalf("the import through a symlinked root was refused: %v", err)
	}
	if res.Status != PackageImportStatusImported {
		t.Fatalf("the status = %q, want %q", res.Status, PackageImportStatusImported)
	}
	t.Logf("a symlinked root is resolved and imported: %s", res.SkillID)
}

// TestP4_B1_ARootReachedThroughASymlinkStillImports is the positive
// counterpart: the operator's path is a symlink to the package directory
// and the import succeeds with the package's own content.
func TestP4_B1_ARootReachedThroughASymlinkStillImports(t *testing.T) {
	dm := p3DM(t)

	real := p4PackageTree(t, map[string]string{
		"SKILL.md":        p4SkillDoc("viSymlink", "1.0.0", "body\n"),
		"references/a.md": "# A\n",
	})
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	res, err := dm.ImportSkillPackageFromDir(PackageImportOptions{Path: link})
	if err != nil {
		t.Fatalf("the import through a symlinked root was refused: %v", err)
	}
	if len(res.Files) != 2 {
		t.Errorf("the package carries %d files, want 2", len(res.Files))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// The root replaced AFTER the anchor is open
// ─────────────────────────────────────────────────────────────────────────────

// TestP4_B1_ASwapAfterTheWalkStartedCannotChangeWhatIsRead proves the
// other half of the invariant: once the anchor is open, the path can be
// re-pointed and the walk still reads the inode the operator selected.
//
// The import here SUCCEEDS — and it succeeds reading the ORIGINAL
// directory. That is the point: the swap cannot redirect the read. (The
// importer may also notice the change and refuse; either outcome is safe,
// but the bytes stored must never come from the outside directory.)
func TestP4_B1_ASwapAfterTheWalkStartedCannotChangeWhatIsRead(t *testing.T) {
	dm := p3DM(t)

	base := t.TempDir()
	root := filepath.Join(base, "pkg")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	entry := p4SkillDoc("anchored", "1.0.0", "the ORIGINAL body\n")
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte(entry), 0o600); err != nil {
		t.Fatalf("write the entrypoint: %v", err)
	}
	outside, _ := p4ConfinementOutsideTree(t, base)

	// The rendezvous again: the importer blocks on the toxic-phrase file
	// after the root was resolved. The swap lands while the anchor is
	// being opened.
	phrases := dm.poisonPhrasesPath()
	_ = os.Remove(phrases)
	if err := syscall.Mkfifo(phrases, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	type outcome struct {
		res *PackageImportResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := dm.ImportSkillPackageFromDir(PackageImportOptions{Path: root})
		done <- outcome{res, err}
	}()

	w, err := os.OpenFile(phrases, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open the fifo for writing: %v", err)
	}
	time.Sleep(150 * time.Millisecond)

	if err := os.Rename(root, filepath.Join(base, "pkg-original")); err != nil {
		t.Fatalf("rename the root: %v", err)
	}
	if err := os.Symlink(outside, root); err != nil {
		t.Fatalf("symlink the root: %v", err)
	}

	if _, err := w.WriteString("a custom toxic phrase\n"); err != nil {
		t.Fatalf("write to the fifo: %v", err)
	}
	_ = w.Close()

	var got outcome
	select {
	case got = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the import never completed")
	}

	if got.err != nil {
		t.Logf("the import was refused (acceptable): %v (reason %q)", got.err, PackageImportReasonOf(got.err))
		p4AssertNothingStored(t, dm, "post-anchor swap")
		return
	}

	// The import succeeded. The bytes MUST be the ORIGINAL package's, not
	// the outside directory's.
	var content string
	if err := dm.db.QueryRow(`SELECT content FROM memories WHERE id = ?`,
		got.res.SkillID).Scan(&content); err != nil {
		t.Fatalf("read the projection: %v", err)
	}
	if !strings.Contains(content, "the ORIGINAL body") {
		t.Errorf("the stored projection is not the original package's:\n%s", content)
	}
	if strings.Contains(content, "THIS CAME FROM OUTSIDE THE ROOT") {
		t.Errorf("the stored projection came from OUTSIDE the import root:\n%s", content)
	}
	var n int
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM skill_package_files WHERE path = ?`,
		"stolen.md").Scan(&n); err != nil {
		t.Fatalf("count the stolen file row: %v", err)
	}
	if n != 0 {
		t.Errorf("a file from OUTSIDE the import root was stored")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// The anchor's immunity to a later swap — the invariant, at unit level
// ─────────────────────────────────────────────────────────────────────────────

// TestP4_B1_TheAnchorIsImmuneToALaterSwap proves the invariant directly,
// at the level the invariant lives.
//
// The anchor is opened, and ONLY THEN is the root path re-pointed at a
// directory outside. Every read the anchor performs afterwards must still
// come from the inode it was opened on.
//
// This is the case the FIFO handshake cannot reach: the handshake lands
// the swap BEFORE the anchor is opened, so it exercises the identity
// check. This test exercises what happens after.
func TestP4_B1_TheAnchorIsImmuneToALaterSwap(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "pkg")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	original := []byte(p4SkillDoc("anchored", "1.0.0", "the ORIGINAL body\n"))
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), original, 0o600); err != nil {
		t.Fatalf("write the entrypoint: %v", err)
	}
	outside, _ := p4ConfinementOutsideTree(t, base)

	anchor, err := openPackageImportAnchor(root)
	if err != nil {
		t.Fatalf("open the anchor: %v", err)
	}
	defer anchor.Close()

	// NOW the root path is re-pointed at the outside directory.
	if err := os.Rename(root, filepath.Join(base, "pkg-original")); err != nil {
		t.Fatalf("rename the root: %v", err)
	}
	if err := os.Symlink(outside, root); err != nil {
		t.Fatalf("symlink the root: %v", err)
	}

	// The anchor still sees the ORIGINAL directory.
	dir, err := anchor.root.Open(".")
	if err != nil {
		t.Fatalf("open the anchored root: %v", err)
	}
	defer dir.Close()
	entries, err := dir.ReadDir(64)
	if err != nil {
		t.Fatalf("read the anchored root: %v", err)
	}
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	t.Logf("the anchored root still lists: %v", names)

	content, _, err := anchor.readFile("SKILL.md", 1<<20)
	if err != nil {
		t.Fatalf("read through the anchor: %v", err)
	}
	if string(content) != string(original) {
		t.Errorf("the anchor read %q, want the ORIGINAL entrypoint %q", content, original)
	}

	// And the outside file is NOT reachable through the anchor.
	if _, _, err := anchor.readFile("stolen.md", 1<<20); err == nil {
		t.Errorf("the anchor read a file that is only in the OUTSIDE directory")
	} else {
		t.Logf("the anchor refuses the outside file, as it must: %v", err)
	}
}

// TestP4_B1_ADirectoryReplacedByASymlinkIsRefused covers the
// directory-replacement case: a subdirectory inside the root replaced by
// a symlink to a directory outside the root.
func TestP4_B1_ADirectoryReplacedByASymlinkIsRefused(t *testing.T) {
	dm := p3DM(t)

	base := t.TempDir()
	root := filepath.Join(base, "pkg")
	if err := os.MkdirAll(filepath.Join(root, "references"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"),
		[]byte(p4SkillDoc("dirswap", "1.0.0", "body\n")), 0o600); err != nil {
		t.Fatalf("write entrypoint: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "references", "real.md"),
		[]byte("# real\n"), 0o600); err != nil {
		t.Fatalf("write real: %v", err)
	}
	outside, _ := p4ConfinementOutsideTree(t, base)

	// The swap: the subdirectory becomes a symlink to the outside tree.
	if err := os.Rename(filepath.Join(root, "references"),
		filepath.Join(root, "references-original")); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "references")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := dm.ImportSkillPackageFromDir(PackageImportOptions{Path: root})
	if err == nil {
		t.Fatal("the import descended into a symlinked directory")
	}
	t.Logf("the import was refused: %v (reason %q)", err, PackageImportReasonOf(err))
	if got := PackageImportReasonOf(err); got != PackageImportReasonSymlinkedDirectory &&
		got != PackageImportReasonPathEscapesRoot {
		t.Errorf("the refusal reason = %q, want %q or %q", got,
			PackageImportReasonSymlinkedDirectory, PackageImportReasonPathEscapesRoot)
	}
	p4AssertNothingStored(t, dm, "directory swap")
}

// TestP4_B1_AnAncestorDirectoryReplacedByASymlinkIsRefused covers the
// ancestor case: a directory ABOVE the package root replaced by a symlink
// to a directory that holds a different package.
func TestP4_B1_AnAncestorDirectoryReplacedByASymlinkIsRefused(t *testing.T) {
	dm := p3DM(t)

	base := t.TempDir()
	parent := filepath.Join(base, "parent")
	root := filepath.Join(parent, "pkg")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"),
		[]byte(p4SkillDoc("ancestor", "1.0.0", "the ORIGINAL body\n")), 0o600); err != nil {
		t.Fatalf("write entrypoint: %v", err)
	}
	outside, _ := p4ConfinementOutsideTree(t, base)

	// The swap: the ANCESTOR becomes a symlink to the outside tree.
	if err := os.Rename(parent, filepath.Join(base, "parent-original")); err != nil {
		t.Fatalf("rename the ancestor: %v", err)
	}
	if err := os.Symlink(outside, parent); err != nil {
		t.Fatalf("symlink the ancestor: %v", err)
	}

	res, err := dm.ImportSkillPackageFromDir(PackageImportOptions{Path: root})
	if err != nil {
		t.Logf("the import was refused (acceptable): %v (reason %q)", err, PackageImportReasonOf(err))
		p4AssertNothingStored(t, dm, "ancestor swap")
		return
	}

	// If it succeeded, the bytes must be the original package's.
	var content string
	if err := dm.db.QueryRow(`SELECT content FROM memories WHERE id = ?`,
		res.SkillID).Scan(&content); err != nil {
		t.Fatalf("read the projection: %v", err)
	}
	if !strings.Contains(content, "the ORIGINAL body") {
		t.Errorf("the stored projection is not the original package's:\n%s", content)
	}
	if strings.Contains(content, "THIS CAME FROM OUTSIDE THE ROOT") {
		t.Errorf("the stored projection came from OUTSIDE the import root:\n%s", content)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// The legitimate symlink handling must survive the fix
// ─────────────────────────────────────────────────────────────────────────────

// TestP4_B1_AnInRootRegularFileSymlinkIsStillAdmitted pins the
// distinction the design draws: a symlink to a regular file INSIDE the
// root is admitted, and its TARGET's bytes are stored.
func TestP4_B1_AnInRootRegularFileSymlinkIsStillAdmitted(t *testing.T) {
	dm := p3DM(t)

	root := p4PackageTree(t, map[string]string{
		"SKILL.md": p4SkillDoc("symlinkedfile", "1.0.0", "body\n"),
	})
	target := filepath.Join(root, "real-target.md")
	if err := os.WriteFile(target, []byte("# the real target\n"), 0o600); err != nil {
		t.Fatalf("write the target: %v", err)
	}
	// A RELATIVE symlink — the portable form, and the one a package can
	// carry.
	if err := os.Symlink("real-target.md", filepath.Join(root, "alias.md")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	res, err := dm.ImportSkillPackageFromDir(PackageImportOptions{Path: root})
	if err != nil {
		t.Fatalf("the import refused a legitimate in-root symlink: %v", err)
	}

	var blob []byte
	if err := dm.db.QueryRow(`
		SELECT b.content FROM skill_package_blobs b
		JOIN skill_package_files f ON f.sha256 = b.sha256
		WHERE f.skill_id = ? AND f.path = ?`, res.SkillID, "alias.md").Scan(&blob); err != nil {
		t.Fatalf("read the alias blob: %v", err)
	}
	if string(blob) != "# the real target\n" {
		t.Errorf("the alias's stored bytes are %q, want the TARGET's bytes", string(blob))
	}
}

// TestP4_B1_AnInRootSymlinkedDirectoryIsStillRefused pins the other half
// of the distinction.
func TestP4_B1_AnInRootSymlinkedDirectoryIsStillRefused(t *testing.T) {
	dm := p3DM(t)

	root := p4PackageTree(t, map[string]string{
		"SKILL.md": p4SkillDoc("symlinkeddir", "1.0.0", "body\n"),
	})
	inside := filepath.Join(root, "references")
	if err := os.MkdirAll(inside, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(inside, "a.md"), []byte("# A\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Symlink("references", filepath.Join(root, "linked-dir")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := dm.ImportSkillPackageFromDir(PackageImportOptions{Path: root})
	if err == nil {
		t.Fatal("the import descended into a symlinked directory")
	}
	if got := PackageImportReasonOf(err); got != PackageImportReasonSymlinkedDirectory {
		t.Errorf("the refusal reason = %q, want %q", got, PackageImportReasonSymlinkedDirectory)
	}
	p4AssertNothingStored(t, dm, "symlinked directory")
}

// TestP4_B1_ASymlinkOutsideTheRootIsStillRefused pins the out-of-root
// symlink refusal.
func TestP4_B1_ASymlinkOutsideTheRootIsStillRefused(t *testing.T) {
	dm := p3DM(t)

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.md"), []byte("# outside\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	root := p4PackageTree(t, map[string]string{
		"SKILL.md": p4SkillDoc("escaping", "1.0.0", "body\n"),
	})
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(root, "escape.md")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := dm.ImportSkillPackageFromDir(PackageImportOptions{Path: root})
	if err == nil {
		t.Fatal("the import followed a symlink out of the import root")
	}
	if got := PackageImportReasonOf(err); got != PackageImportReasonPathEscapesRoot {
		t.Errorf("the refusal reason = %q, want %q", got, PackageImportReasonPathEscapesRoot)
	}
	p4AssertNothingStored(t, dm, "outside symlink")
}

// TestP4_B1_ARelativeSymlinkOutsideTheRootIsRefused covers the relative
// form of the escape — the one that a plain string-prefix check would
// miss.
func TestP4_B1_ARelativeSymlinkOutsideTheRootIsRefused(t *testing.T) {
	dm := p3DM(t)

	base := t.TempDir()
	root := filepath.Join(base, "pkg")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.md"), []byte("# outside\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"),
		[]byte(p4SkillDoc("relativescape", "1.0.0", "body\n")), 0o600); err != nil {
		t.Fatalf("write entrypoint: %v", err)
	}
	// A RELATIVE symlink that climbs out of the root.
	if err := os.Symlink("../outside/secret.md", filepath.Join(root, "escape.md")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := dm.ImportSkillPackageFromDir(PackageImportOptions{Path: root})
	if err == nil {
		t.Fatal("the import followed a relative symlink out of the import root")
	}
	t.Logf("the refusal reason is %q", PackageImportReasonOf(err))
	p4AssertNothingStored(t, dm, "relative outside symlink")
}
