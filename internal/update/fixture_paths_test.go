package update

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// ---------------------------------------------------------------------
// The fixture copy's path rules
// ---------------------------------------------------------------------
//
// The copy's file set is the TRACKED file set. A tracked path is a
// NAME, and the bytes behind a name may live somewhere else: the
// working tree can replace a tracked directory with a symlink, and
// then "sub/tracked.txt" names a file in whatever that symlink points
// at.
//
// These tests pin the rule that follows from the fixture's contract:
// the copy reads only through non-symlink components of the source
// root, and it REFUSES a path it cannot read that way. An unsafe path
// is refused, not skipped: a copy that silently dropped it would
// report success while carrying neither the file nor the reason.
//
// Every test here is bounded: the repositories are a handful of small
// files, and the external targets are a single sentinel file.

// pathRepo creates a git repository holding the given files, committed.
//
// It is a REAL repository: the copy's boundary is the tracked file set,
// and a directory that is not a checkout has no bounded file set.
func pathRepo(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		full := filepath.Join(repo, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(files[name]), 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "t"},
		{"add", "-A"},
		{"commit", "--quiet", "-m", "fixture"},
	} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...) //nolint:gosec
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	return repo
}

// pathRepoHead returns the repository's HEAD.
func pathRepoHead(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "rev-parse", "HEAD") //nolint:gosec
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse failed: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// assertTreeLacksBytes proves that no file under dir carries the
// given bytes.
//
// It is the proof that an operator's file was not copied: the
// sentinel's CONTENT is what leaked, so the content is what is
// checked.
func assertTreeLacksBytes(t *testing.T, dir string, needle []byte) {
	t.Helper()
	_ = filepath.Walk(dir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		if bytes.Contains(raw, needle) {
			t.Errorf("the copy carried the sentinel bytes into %s", path)
		}
		return nil
	})
}

// TestFixtureCopyRefusesATrackedDirectoryReplacedByAnExternalSymlink
// proves the copy does not read through a symlinked directory into
// operator-owned data.
//
// The tracked set says "sub/tracked.txt". The working tree replaces
// the directory "sub" with a symlink to a directory OUTSIDE the
// checkout. Reading the tracked path by name would read the
// operator's file; the copy must refuse.
func TestFixtureCopyRefusesATrackedDirectoryReplacedByAnExternalSymlink(t *testing.T) {
	work := t.TempDir()
	external := filepath.Join(work, "operator-private")
	if err := os.MkdirAll(external, 0755); err != nil {
		t.Fatal(err)
	}
	sentinel := []byte("OPERATOR-PRIVATE-DIRECTORY-BYTES\n")
	if err := os.WriteFile(filepath.Join(external, "tracked.txt"), sentinel, 0600); err != nil {
		t.Fatal(err)
	}

	repo := pathRepo(t, work, map[string]string{"sub/tracked.txt": "original\n"})
	if err := os.RemoveAll(filepath.Join(repo, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(repo, "sub")); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(work, "bundle", "checkout")
	err := copySourceTreeFrom(repo, dst)
	if err == nil {
		t.Errorf("the copy accepted a tracked directory replaced by a symlink to an external directory")
	} else if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("the refusal does not name the symlink: %v", err)
	}
	assertTreeLacksBytes(t, dst, sentinel)
	if _, err := os.Stat(filepath.Join(dst, "sub", "tracked.txt")); err == nil {
		t.Error("the copy wrote the external file into the destination")
	}
}

// TestFixtureCopyRefusesATrackedDirectoryReplacedByAnInternalSymlink
// proves the rule is about SYMLINKS, not only about containment.
//
// The symlink points INSIDE the checkout, so a containment check
// alone would accept it. The fixture's contract is stricter: no
// symlink is ever followed, because a symlink is a pointer the
// fixture did not create and cannot vouch for.
func TestFixtureCopyRefusesATrackedDirectoryReplacedByAnInternalSymlink(t *testing.T) {
	work := t.TempDir()
	repo := pathRepo(t, work, map[string]string{
		"sub/tracked.txt": "original\n",
		"inside/kept.txt": "kept\n",
	})
	// The symlink's target holds a file the index does not track: the
	// bytes that would leak if the link were followed.
	internal := filepath.Join(repo, "inside")
	internalBytes := []byte("INTERNAL-SYMLINK-BYTES\n")
	if err := os.WriteFile(filepath.Join(internal, "tracked.txt"), internalBytes, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(repo, "sub")); err != nil {
		t.Fatal(err)
	}
	// A RELATIVE symlink: it resolves inside the checkout.
	if err := os.Symlink("inside", filepath.Join(repo, "sub")); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(work, "bundle", "checkout")
	err := copySourceTreeFrom(repo, dst)
	if err == nil {
		t.Errorf("the copy accepted a tracked directory replaced by a symlink inside the checkout")
	}
	assertTreeLacksBytes(t, dst, internalBytes)
	if _, err := os.Stat(filepath.Join(dst, "sub", "tracked.txt")); err == nil {
		t.Error("the copy wrote the symlink's target into the destination")
	}
}

// TestFixtureCopyRefusesATrackedFileReplacedByASymlink proves the
// FINAL component is checked too: a tracked file replaced by a
// symlink is refused, not followed.
func TestFixtureCopyRefusesATrackedFileReplacedByASymlink(t *testing.T) {
	work := t.TempDir()
	external := filepath.Join(work, "operator-private")
	if err := os.MkdirAll(external, 0755); err != nil {
		t.Fatal(err)
	}
	sentinel := []byte("OPERATOR-PRIVATE-FILE-BYTES\n")
	if err := os.WriteFile(filepath.Join(external, "secret.txt"), sentinel, 0600); err != nil {
		t.Fatal(err)
	}

	repo := pathRepo(t, work, map[string]string{"notes.txt": "original\n"})
	if err := os.Remove(filepath.Join(repo, "notes.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(external, "secret.txt"), filepath.Join(repo, "notes.txt")); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(work, "bundle", "checkout")
	err := copySourceTreeFrom(repo, dst)
	if err == nil {
		t.Errorf("the copy accepted a tracked file replaced by a symlink")
	}
	assertTreeLacksBytes(t, dst, sentinel)
	if _, err := os.Stat(filepath.Join(dst, "notes.txt")); err == nil {
		t.Error("the copy wrote the symlink's target into the destination")
	}
}

// TestFixtureCopyRefusesASymlinkChainOfIntermediateComponents proves
// a chain of symlinked components is refused.
//
// The tracked path crosses TWO symlinked components: "one" points at a
// directory inside the checkout, and "two" (inside it) points at the
// operator's directory. Resolving the path by name would read the
// operator's file through both links.
func TestFixtureCopyRefusesASymlinkChainOfIntermediateComponents(t *testing.T) {
	work := t.TempDir()
	external := filepath.Join(work, "operator-private")
	if err := os.MkdirAll(external, 0755); err != nil {
		t.Fatal(err)
	}
	sentinel := []byte("OPERATOR-CHAIN-BYTES\n")
	if err := os.WriteFile(filepath.Join(external, "tracked.txt"), sentinel, 0600); err != nil {
		t.Fatal(err)
	}

	repo := pathRepo(t, work, map[string]string{"chain/one/two/tracked.txt": "original\n"})
	if err := os.MkdirAll(filepath.Join(repo, "chain-real", "one"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(repo, "chain")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "chain"), 0755); err != nil {
		t.Fatal(err)
	}
	// The first link points inside the checkout (ABSOLUTE, so the link
	// resolves the same way whoever follows it)...
	if err := os.Symlink(filepath.Join(repo, "chain-real", "one"), filepath.Join(repo, "chain", "one")); err != nil {
		t.Fatal(err)
	}
	// ... and the second points at the operator's directory.
	if err := os.Symlink(external, filepath.Join(repo, "chain-real", "one", "two")); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(work, "bundle", "checkout")
	err := copySourceTreeFrom(repo, dst)
	if err == nil {
		t.Errorf("the copy accepted a tracked path whose intermediate components are symlinks")
	}
	assertTreeLacksBytes(t, dst, sentinel)
	if _, err := os.Stat(filepath.Join(dst, "chain", "one", "two", "tracked.txt")); err == nil {
		t.Error("the copy wrote the symlink chain's target into the destination")
	}
}

// TestFixtureCopyCarriesOrdinaryTrackedFiles proves the ordinary case
// still works: a tracked regular file is copied, byte for byte.
//
// A copier that refuses everything would pass every refusal test
// above. This is the test that says the refusals are selective.
func TestFixtureCopyCarriesOrdinaryTrackedFiles(t *testing.T) {
	work := t.TempDir()
	files := map[string]string{
		"README.md":            "# fixture\n",
		"cmd/mpm/main.go":      "package main\n",
		"agent_installation/a": "alpha\n",
	}
	repo := pathRepo(t, work, files)

	dst := filepath.Join(work, "bundle", "checkout")
	if err := copySourceTreeFrom(repo, dst); err != nil {
		t.Fatalf("the fixture copy of an ordinary checkout failed: %v", err)
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

// TestFixtureCopyOfALinkedWorktreeCarriesNoGitMetadata proves a
// linked worktree — whose ".git" is a FILE pointing at the real
// repository — is copied without its git metadata, and that the real
// repository is untouched.
//
// A copied ".git" file would make the fixture's `git init`
// REINITIALIZE THE REAL REPOSITORY, and the fixture's commits would
// land in the developer's own checkout.
func TestFixtureCopyOfALinkedWorktreeCarriesNoGitMetadata(t *testing.T) {
	work := t.TempDir()
	repo := pathRepo(t, work, map[string]string{"README.md": "# fixture\n"})
	linked := filepath.Join(work, "linked-worktree")
	cmd := exec.Command("git", "-C", repo, "worktree", "add", "--quiet", linked, "-b", "fixture-branch") //nolint:gosec
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("this git cannot create a linked worktree: %v\n%s", err, out)
	}
	// The linked worktree's .git is a FILE, not a directory.
	fi, err := os.Lstat(filepath.Join(linked, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.IsDir() {
		t.Fatalf("the linked worktree's .git is a directory; this host does not produce the linked-worktree shape")
	}
	headBefore := pathRepoHead(t, repo)

	dst := filepath.Join(work, "bundle", "checkout")
	if err := copySourceTreeFrom(linked, dst); err != nil {
		t.Fatalf("the fixture copy of a linked worktree failed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, ".git")); err == nil {
		t.Error("the copy carried the linked worktree's .git file")
	}
	if _, err := os.Stat(filepath.Join(dst, "README.md")); err != nil {
		t.Errorf("the copy does not carry the worktree's files: %v", err)
	}
	if got := pathRepoHead(t, repo); got != headBefore {
		t.Errorf("the original repository's HEAD changed: %q -> %q", headBefore, got)
	}
}

// TestFixtureCopyRefusesASourceAndDestinationThatResolveToTheSameDirectory
// proves the copy refuses to read and write the same tree, whether
// the destination IS the source or an ALIAS for it.
func TestFixtureCopyRefusesASourceAndDestinationThatResolveToTheSameDirectory(t *testing.T) {
	work := t.TempDir()
	repo := pathRepo(t, work, map[string]string{"README.md": "# fixture\n"})

	t.Run("the destination is the source", func(t *testing.T) {
		err := copySourceTreeFrom(repo, repo)
		if err == nil {
			t.Errorf("the copy accepted a destination that is the source itself")
		}
	})

	t.Run("the destination is an alias for the source", func(t *testing.T) {
		alias := filepath.Join(work, "alias-to-repo")
		if err := os.Symlink(repo, alias); err != nil {
			t.Fatal(err)
		}
		err := copySourceTreeFrom(repo, alias)
		if err == nil {
			t.Errorf("the copy accepted a destination that resolves to the source")
		} else if !strings.Contains(err.Error(), "same directory") {
			t.Errorf("the refusal does not explain itself: %v", err)
		}
	})
}

// TestFixtureCopyRefusesOverlappingSourceAndDestination proves the
// overlap rules are made on RESOLVED paths.
//
// A destination whose NAME says nothing about where it points can
// still overlap the source: an alias into the source, or a
// destination that contains the source.
func TestFixtureCopyRefusesOverlappingSourceAndDestination(t *testing.T) {
	work := t.TempDir()
	repo := pathRepo(t, work, map[string]string{"README.md": "# fixture\n"})

	t.Run("the destination resolves inside the source through an alias", func(t *testing.T) {
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
			t.Errorf("the copy accepted a destination that resolves inside the source")
		}
	})

	t.Run("the source resolves inside the destination through an alias", func(t *testing.T) {
		alias := filepath.Join(work, "alias-to-parent")
		if err := os.Symlink(work, alias); err != nil {
			t.Fatal(err)
		}
		err := copySourceTreeFrom(repo, alias)
		if err == nil {
			t.Errorf("the copy accepted a destination that contains the source")
		}
	})
}

// TestFixtureCopyRefusesANonRegularTrackedPath proves the copy
// refuses a tracked path that is not a regular file.
//
// A FIFO would block the copy forever if it were opened for reading;
// a device or a socket is not a source file at all. Neither is
// silently skipped: the copy says so.
func TestFixtureCopyRefusesANonRegularTrackedPath(t *testing.T) {
	work := t.TempDir()
	repo := pathRepo(t, work, map[string]string{"pipe": "placeholder\n"})
	if err := os.Remove(filepath.Join(repo, "pipe")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(repo, "pipe"), 0o644); err != nil {
		t.Skipf("this host cannot create a FIFO: %v", err)
	}

	dst := filepath.Join(work, "bundle", "checkout")
	err := copySourceTreeFrom(repo, dst)
	if err == nil {
		t.Errorf("the copy accepted a tracked path that is not a regular file")
	}
	if _, err := os.Stat(filepath.Join(dst, "pipe")); err == nil {
		t.Error("the copy wrote a non-regular tracked path into the destination")
	}
}

// TestFixtureCopyVerifiesTheEffectivePathIsInsideTheSource proves the
// second gate BITES.
//
// The openat traversal makes a symlinked component impossible, and this
// is the independent measurement on top of it: the effective path of
// the file that was actually opened must lie inside the authorised
// source root. A file outside it is refused.
func TestFixtureCopyVerifiesTheEffectivePathIsInsideTheSource(t *testing.T) {
	work := t.TempDir()
	repo := pathRepo(t, work, map[string]string{"README.md": "# fixture\n"})

	root, err := openSourceRoot(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer root.close()

	outside := filepath.Join(work, "outside.txt")
	if err := os.WriteFile(outside, []byte("outside\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(outside, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	if err := verifyEffectivePath(root, fd, "outside.txt"); err == nil {
		t.Error("the effective-path check accepted a file outside the source root")
	}

	inside, err := unix.Open(filepath.Join(repo, "README.md"), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(inside) }()
	if err := verifyEffectivePath(root, inside, "README.md"); err != nil {
		t.Errorf("the effective-path check refused a file inside the source root: %v", err)
	}
}
