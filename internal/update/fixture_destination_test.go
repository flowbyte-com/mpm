package update

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------
// The fixture's destination boundary
// ---------------------------------------------------------------------
//
// The copy's SOURCE side refuses to read through a symlink. The
// DESTINATION side has the same hazard pointed the other way: a write
// that follows a symlink lands wherever the link points.
//
// So the destination is written the way the source is read — every
// component opened relative to the handle above it, with O_NOFOLLOW,
// and the final component created with O_EXCL.
//
// These tests pin that, from the outside: the observable claim is that
// a file OUTSIDE the fixture's destination tree is byte-identical
// afterwards, whatever shape the destination was poisoned into.

// destinationSentinel is the content of the file outside the fixture
// tree that must survive every rejected copy.
//
// It is deliberately distinctive: the proof is about CONTENT, so the
// content has to be recognisable.
const destinationSentinel = "OPERATOR-OWNED-SENTINEL-BYTES\n"

// TestFixtureCopyRefusesADestinationFileThatIsASymlinkToAnExternalFile
// proves the final destination filename cannot redirect a write.
//
// The destination already holds a symlink at the tracked file's name,
// pointing at a file OUTSIDE the fixture tree. A write that follows
// the link overwrites the operator's file; the copy must refuse.
func TestFixtureCopyRefusesADestinationFileThatIsASymlinkToAnExternalFile(t *testing.T) {
	work := t.TempDir()

	external := filepath.Join(work, "operator-private")
	if err := os.MkdirAll(external, 0755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(external, "owned.txt")
	if err := os.WriteFile(sentinel, []byte(destinationSentinel), 0600); err != nil {
		t.Fatal(err)
	}

	repo := pathRepo(t, work, map[string]string{"README.md": "# fixture\n"})

	dst := filepath.Join(work, "bundle", "checkout")
	if err := os.MkdirAll(dst, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dst, "README.md")
	if err := os.Symlink(sentinel, link); err != nil {
		t.Fatal(err)
	}

	err := copySourceTreeFrom(repo, dst)
	if err == nil {
		t.Error("the copy wrote through a destination symlink")
	}
	assertFileHasBytes(t, sentinel, destinationSentinel)
	// The link is still a link: the copy did not replace it either.
	fi, lerr := os.Lstat(link)
	if lerr != nil {
		t.Fatalf("the destination symlink is gone: %v", lerr)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("the copy replaced the destination symlink with a regular file")
	}
}

// TestFixtureCopyRefusesADestinationFileThatIsASymlinkToAnInternalFile
// proves the refusal is about SYMLINKS, not only about escape.
//
// The symlink points INSIDE the destination tree. Following it would
// not reach an operator's file — it would still be a write through a
// pointer the fixture did not create and cannot vouch for, and it
// would overwrite a file the fixture did not write.
func TestFixtureCopyRefusesADestinationFileThatIsASymlinkToAnInternalFile(t *testing.T) {
	work := t.TempDir()

	repo := pathRepo(t, work, map[string]string{"README.md": "# fixture\n"})

	dst := filepath.Join(work, "bundle", "checkout")
	if err := os.MkdirAll(dst, 0755); err != nil {
		t.Fatal(err)
	}
	internal := filepath.Join(dst, "other.txt")
	if err := os.WriteFile(internal, []byte(destinationSentinel), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(internal, filepath.Join(dst, "README.md")); err != nil {
		t.Fatal(err)
	}

	err := copySourceTreeFrom(repo, dst)
	if err == nil {
		t.Error("the copy wrote through a destination symlink")
	}
	assertFileHasBytes(t, internal, destinationSentinel)
}

// TestFixtureCopyRefusesAnExistingDestinationFile proves the copy
// never overwrites a file that is already there.
//
// The fixture copies into a tree it has just created, so a file that
// is already at a tracked path means the destination is not the one
// the fixture made — a reused tree, or a poisoned one. The copy
// refuses it rather than overwriting whatever it finds.
func TestFixtureCopyRefusesAnExistingDestinationFile(t *testing.T) {
	work := t.TempDir()

	repo := pathRepo(t, work, map[string]string{"README.md": "# fixture\n"})

	dst := filepath.Join(work, "bundle", "checkout")
	if err := os.MkdirAll(dst, 0755); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(dst, "README.md")
	if err := os.WriteFile(existing, []byte(destinationSentinel), 0600); err != nil {
		t.Fatal(err)
	}

	err := copySourceTreeFrom(repo, dst)
	if err == nil {
		t.Error("the copy overwrote a file that was already in the destination")
	}
	assertFileHasBytes(t, existing, destinationSentinel)
}

// TestFixtureCopyRefusesADestinationDirectoryThatIsASymlink proves an
// intermediate destination component cannot redirect the write.
//
// The destination holds a symlink at the tracked DIRECTORY's name,
// pointing at a directory outside the fixture tree. The tracked file
// inside it would be written into the operator's directory.
func TestFixtureCopyRefusesADestinationDirectoryThatIsASymlink(t *testing.T) {
	work := t.TempDir()

	external := filepath.Join(work, "operator-private")
	if err := os.MkdirAll(external, 0755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(external, "tracked.txt")
	if err := os.WriteFile(sentinel, []byte(destinationSentinel), 0600); err != nil {
		t.Fatal(err)
	}

	repo := pathRepo(t, work, map[string]string{"sub/tracked.txt": "original\n"})

	dst := filepath.Join(work, "bundle", "checkout")
	if err := os.MkdirAll(dst, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(dst, "sub")); err != nil {
		t.Fatal(err)
	}

	err := copySourceTreeFrom(repo, dst)
	if err == nil {
		t.Error("the copy wrote through a destination directory symlink")
	}
	assertFileHasBytes(t, sentinel, destinationSentinel)
}

// TestFixtureCopyRefusesADestinationRootThatIsASymlink proves the
// destination ROOT cannot be redirected.
//
// The destination itself is a symlink to a directory outside the
// fixture tree. Every write would land there, and the fixture would
// believe it had written into its own scratch.
func TestFixtureCopyRefusesADestinationRootThatIsASymlink(t *testing.T) {
	work := t.TempDir()

	external := filepath.Join(work, "operator-private")
	if err := os.MkdirAll(external, 0755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(external, "README.md")
	if err := os.WriteFile(sentinel, []byte(destinationSentinel), 0600); err != nil {
		t.Fatal(err)
	}

	repo := pathRepo(t, work, map[string]string{"README.md": "# fixture\n"})

	realDst := filepath.Join(work, "bundle", "real-checkout")
	if err := os.MkdirAll(realDst, 0755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(work, "bundle", "checkout")
	if err := os.Symlink(external, dst); err != nil {
		t.Fatal(err)
	}

	err := copySourceTreeFrom(repo, dst)
	if err == nil {
		t.Error("the copy wrote through a destination root symlink")
	}
	assertFileHasBytes(t, sentinel, destinationSentinel)
}

// TestFixtureCopyRefusesADestinationThatIsAnAliasIntoTheSource proves
// the source/destination overlap rules still hold on the destination
// side.
//
// The destination is an ALIAS for a directory inside the source: its
// name says nothing about where it points, and the copy would read
// and write the same tree.
func TestFixtureCopyRefusesADestinationThatIsAnAliasIntoTheSource(t *testing.T) {
	work := t.TempDir()
	repo := pathRepo(t, work, map[string]string{"README.md": "# fixture\n"})

	if err := os.MkdirAll(filepath.Join(repo, "nested"), 0755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(work, "alias-into-repo")
	if err := os.Symlink(filepath.Join(repo, "nested"), alias); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(alias, "checkout")

	err := copySourceTreeFrom(repo, dst)
	if err == nil {
		t.Error("the copy accepted a destination that resolves inside the source")
	}
	if !strings.Contains(err.Error(), "inside the source") {
		t.Errorf("the refusal does not explain itself: %v", err)
	}
}

// TestFixtureCopyIntoANewDestination proves the ordinary case still
// works after the destination rules were tightened.
//
// A copier that refuses everything would pass every refusal test
// above. This is the test that says the refusals are selective: a
// tracked file is copied, byte for byte, into a fresh destination.
func TestFixtureCopyIntoANewDestination(t *testing.T) {
	work := t.TempDir()
	files := map[string]string{
		"README.md":            "# fixture\n",
		"cmd/mpm/main.go":      "package main\n",
		"agent_installation/x": "alpha\n",
	}
	repo := pathRepo(t, work, files)

	dst := filepath.Join(work, "bundle", "checkout")
	if err := copySourceTreeFrom(repo, dst); err != nil {
		t.Fatalf("the fixture copy into a new destination failed: %v", err)
	}
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(name)))
		if err != nil {
			t.Errorf("the copy does not carry %s: %v", name, err)
			continue
		}
		if string(got) != want {
			t.Errorf("the copy of %s carries %q, want %q", name, got, want)
		}
	}
}

// assertFileHasBytes proves a file still carries the given content.
//
// It is the proof that an operator's file was not overwritten: the
// CONTENT is what would have been clobbered, so the content is what
// is checked.
func assertFileHasBytes(t *testing.T, path, want string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Errorf("the file %s could not be read: %v", path, err)
		return
	}
	if string(raw) != want {
		t.Errorf("the file %s carries %q, want %q", path, raw, want)
	}
}
