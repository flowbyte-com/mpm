package update

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

// ---------------------------------------------------------------------
// The fixture's copy boundary
// ---------------------------------------------------------------------
//
// # THE BOUNDARY IS EXPLICIT: THE TRACKED FILE SET
//
// A directory walk has no boundary. It follows whatever it finds, and
// when the destination lives INSIDE the source — which is what happens
// when TMPDIR is inside the checkout — the walk descends into the
// directory it is writing and copies its own output, over and over,
// until the filesystem is full.
//
// The tracked file set cannot contain the destination: the destination
// is untracked by construction, so the copy is finite by construction,
// whatever TMPDIR is.
//
// The tracked file set also EXCLUDES, by construction, everything the
// copy must not carry: the repository's own metadata (a .git directory
// or a linked worktree's .git FILE), the compiled plugin output (the
// build's product, not the source's), an installed dependency tree
// and the build directory. A fresh clone has exactly this shape, and
// the fixture is a fresh clone by definition.
//
// # THE BOUNDARY IS ALSO EXPLICIT ABOUT SYMLINKS
//
// The file set is a set of NAMES. The bytes behind a name may live
// somewhere else: a tracked directory can be replaced in the working
// tree by a symlink, and then "sub/tracked.txt" names a file in
// whatever that symlink points at.
//
// So the copy never resolves a path and then opens it. It OPENS each
// component of the path, relative to the directory it already holds
// open, with O_NOFOLLOW:
//
//   - an intermediate component that is a symlink cannot be entered
//     (openat returns ELOOP);
//   - a final component that is a symlink cannot be opened (openat
//     returns ELOOP);
//   - a component that has been REPLACED between the plan and the copy
//     cannot redirect the read, because the check and the open are the
//     same syscall — there is no window between them.
//
// The containment of the effective path is therefore structural, not a
// check that can be raced: every byte read comes from a file reached
// by walking down from the authorised source root, one non-symlink
// component at a time.
//
// And it is MEASURED as well as structured: the effective path of the
// file that was actually opened is read back from /proc/self/fd and
// checked against the resolved source root. The first gate is the
// construction; the second is the observation of the result.
//
// What this does NOT claim: the source ROOT itself is resolved once
// (EvalSymlinks) and then opened; if the root is swapped between that
// resolution and that open, the copy reads the tree that is at the
// resolved path when it is opened. The root is the fixture's own
// source, not an operator's directory, and the fixture creates it.
//
// An unsafe path is REFUSED, not skipped: a copy that silently omitted
// a symlinked path would report success while carrying neither the
// file nor the reason.
//
// # THE DESTINATION SIDE HAS THE SAME HAZARD, POINTED THE OTHER WAY
//
// A write that follows a symlink lands wherever the link points. So
// the destination is written the way the source is read:
//
//   - the destination root is opened as a directory handle;
//   - every intermediate component is opened relative to the handle
//     above it, with O_DIRECTORY|O_NOFOLLOW, so a component that is a
//     symlink cannot be entered;
//   - the final component is created with O_CREAT|O_EXCL|O_NOFOLLOW:
//     it cannot be a symlink, and it cannot already exist.
//
// The final open is CREATE-ONLY. The fixture copies into a tree it has
// just created, so a file that is already at a tracked path means the
// destination is not the one the fixture made — a reused tree, or a
// poisoned one — and the copy refuses it rather than overwriting
// whatever it finds.
//
// The check and the open are the same syscall at every step, so a
// component cannot be swapped for a symlink between them.
//
// What this does NOT claim: the destination root is created with
// os.MkdirAll, which resolves the root's own ANCESTORS. The root is
// the fixture's own scratch directory, created by the fixture, so its
// ancestry is not attacker-controlled; and the root itself is opened
// with O_NOFOLLOW, so a destination that IS a symlink is refused.
//
// The destination root's handle is opened once and HELD for the whole
// copy. Every write is relative to that handle, so a destination path
// that is replaced while the copy runs cannot redirect a write: the
// writes land in the directory the copy opened, or they fail.

// errTrackedFileAbsent marks a tracked path that is not present in the
// working tree.
//
// It is the ONLY condition a copy skips. A tracked file the working
// tree does not have is simply absent (the fixture copies the tree as
// it stands); a symlink, a wrong file type or an unreadable path is
// refused, because those are conditions a copy must not paper over.
var errTrackedFileAbsent = errors.New("the tracked path is not present in the working tree")

// sourceRoot is an open handle on the authorised source directory.
//
// The handle, not the path, is the authority: every tracked file is
// opened relative to it, so the copy cannot be redirected outside the
// source by anything that happens to the path afterwards.
type sourceRoot struct {
	fd int
	// real is the resolved path the handle refers to.
	real string
}

// close releases the source root's handle.
func (r *sourceRoot) close() {
	if r.fd >= 0 {
		_ = unix.Close(r.fd)
		r.fd = -1
	}
}

// copySourceTreeFrom copies src into dst.
//
// It copies the tracked file set of the git checkout at src, refusing
// any path whose bytes are not reachable through non-symlink
// components of that checkout. See the file comment above for the
// boundary this enforces.
func copySourceTreeFrom(src, dst string) error {
	files, err := planSourceCopy(src, dst)
	if err != nil {
		return err
	}
	root, err := openSourceRoot(src)
	if err != nil {
		return err
	}
	defer root.close()

	dest, err := openDestinationRoot(dst)
	if err != nil {
		return err
	}
	defer dest.close()

	for _, rel := range files {
		raw, mode, err := readTrackedFile(root, rel)
		if err != nil {
			if errors.Is(err, errTrackedFileAbsent) {
				// The working tree does not have this tracked
				// path. That is not a safety problem, it is
				// simply absent: the fixture copies the tree
				// as it stands.
				continue
			}
			return err
		}
		if err := writeFixtureFile(dest, rel, raw, mode); err != nil {
			return err
		}
	}
	return nil
}

// openSourceRoot opens the authorised source directory.
//
// The path is resolved FIRST, so the handle refers to a real
// directory, and the resolved path is what the copy's containment
// checks are made against.
func openSourceRoot(src string) (*sourceRoot, error) {
	abs, err := filepath.Abs(src)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("the fixture source %s cannot be resolved: %w", abs, err)
	}
	fd, err := unix.Open(real, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("the fixture source %s cannot be opened: %w", real, err)
	}
	return &sourceRoot{fd: fd, real: real}, nil
}

// readTrackedFile reads the bytes of one tracked path.
//
// Every component is opened relative to the directory handle above it,
// with O_NOFOLLOW, so no component can be a symlink and no component
// can be swapped for one between the check and the read.
func readTrackedFile(root *sourceRoot, rel string) ([]byte, os.FileMode, error) {
	fd, err := openTrackedFile(root, rel)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = unix.Close(fd) }()

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, 0, fmt.Errorf("the fixture source path %s cannot be inspected: %w", rel, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, 0, fmt.Errorf("the fixture source path %s is not a regular file; refusing to copy it", rel)
	}
	if err := verifyEffectivePath(root, fd, rel); err != nil {
		return nil, 0, err
	}

	var buf bytes.Buffer
	chunk := make([]byte, 64*1024)
	for {
		n, err := unix.Read(fd, chunk)
		if n > 0 {
			buf.Write(chunk[:n])
		}
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return nil, 0, fmt.Errorf("the fixture source path %s cannot be read: %w", rel, err)
		}
		if n == 0 {
			break
		}
	}
	return buf.Bytes(), os.FileMode(st.Mode & 0o777), nil
}

// verifyEffectivePath checks that the file just opened really lives
// INSIDE the authorised source root.
//
// The openat traversal above already makes a symlinked component
// impossible, but the requirement is stated in terms of the EFFECTIVE
// path, and the effective path is observable: /proc/self/fd names the
// file that was actually opened, after every symlink has been
// resolved. This is the second, independent gate — the first is
// structural, this one is a measurement.
//
// It fails CLOSED: a path that cannot be verified is not copied. The
// repository targets Linux (systemd units, unix syscalls), where
// /proc/self/fd is always present.
func verifyEffectivePath(root *sourceRoot, fd int, rel string) error {
	effective, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
	if err != nil {
		return fmt.Errorf("the fixture source path %s cannot be verified to be inside %s: %w", rel, root.real, err)
	}
	// A file unlinked while it was open has no path any more. The
	// kernel says so in the link itself.
	effective = strings.TrimSuffix(effective, " (deleted)")
	if !resolvedPathWithin(root.real, effective) {
		return fmt.Errorf("the fixture source path %s resolves to %s, which is outside the fixture source %s; refusing to copy it", rel, effective, root.real)
	}
	return nil
}

// openTrackedFile opens the final component of a tracked path.
//
// The intermediate components are opened one at a time, each with
// O_NOFOLLOW|O_DIRECTORY, so a symlinked component is refused rather
// than traversed. The final component is opened with O_NOFOLLOW too:
// a tracked FILE replaced by a symlink is refused, not followed.
//
// O_NONBLOCK keeps a FIFO from blocking the copy: a path that is not
// a regular file is refused by the caller's fstat, and refusing it
// must not first wait for a writer that may never come.
func openTrackedFile(root *sourceRoot, rel string) (int, error) {
	parts := strings.Split(rel, "/")
	dirFD := root.fd
	// The intermediate handles are closed as the walk descends; the
	// root's handle belongs to the caller and is never closed here.
	defer func() {
		if dirFD != root.fd {
			_ = unix.Close(dirFD)
		}
	}()
	for _, comp := range parts[:len(parts)-1] {
		next, err := unix.Openat(dirFD, comp, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return -1, trackedPathError(rel, comp, dirFD, err, true)
		}
		if dirFD != root.fd {
			_ = unix.Close(dirFD)
		}
		dirFD = next
	}
	name := parts[len(parts)-1]
	fd, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, trackedPathError(rel, name, dirFD, err, false)
	}
	return fd, nil
}

// trackedPathError explains why a tracked path could not be opened.
//
// The three shapes that matter are told apart: an ABSENT path is
// skippable, a SYMLINK is refused, and anything else is refused with
// the reason the kernel gave.
//
// The kernel reports ENOTDIR — not ELOOP — when a component that must
// be a directory is a symlink, because O_DIRECTORY|O_NOFOLLOW refuses
// the link without following it. The component is therefore inspected
// (without following it) to say WHICH case this is: a symlink is
// named as a symlink, and anything else keeps the kernel's reason.
func trackedPathError(rel, comp string, dirFD int, err error, intermediate bool) error {
	switch {
	case errors.Is(err, unix.ENOENT):
		return fmt.Errorf("%s: %w", rel, errTrackedFileAbsent)
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR):
		if isSymlinkAt(dirFD, comp) {
			if intermediate {
				return fmt.Errorf("the fixture source path %s traverses the directory %q, which is a symlink; refusing to copy through it", rel, comp)
			}
			return fmt.Errorf("the fixture source path %s is a symlink; refusing to copy it", rel)
		}
		if intermediate {
			return fmt.Errorf("the fixture source path %s traverses %q, which is not a directory; refusing to copy it", rel, comp)
		}
		return fmt.Errorf("the fixture source path %s cannot be opened: %w", rel, err)
	default:
		return fmt.Errorf("the fixture source path %s cannot be opened: %w", rel, err)
	}
}

// isSymlinkAt reports whether the component is a symlink.
//
// The component is inspected WITHOUT being followed: the point is to
// name the shape of the path, not to read what it points at.
func isSymlinkAt(dirFD int, comp string) bool {
	var st unix.Stat_t
	if err := unix.Fstatat(dirFD, comp, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return false
	}
	return st.Mode&unix.S_IFMT == unix.S_IFLNK
}

// destinationRoot is an open handle on the fixture's destination tree.
//
// The handle, not the path, is the authority for every write. It is
// opened ONCE for the whole copy, so a destination path that is
// replaced while the copy runs cannot redirect a write: the writes go
// to the directory the copy opened, or they fail.
//
// A destination whose root has been unlinked underneath the copy fails
// the write rather than continuing: the kernel refuses to create a
// file in a directory that has been removed. That is what keeps the
// recursion tests' tripwire effective — it replaces the destination
// with a regular file, and the copy stops.
type destinationRoot struct {
	fd int
}

// close releases the destination root's handle.
func (d *destinationRoot) close() {
	if d.fd >= 0 {
		_ = unix.Close(d.fd)
		d.fd = -1
	}
}

// openDestinationRoot creates the destination tree if it does not
// exist, and opens it as a directory handle.
//
// The root is opened with O_NOFOLLOW: a destination that is a symlink
// is refused, because a symlinked destination is exactly how a write
// leaves the tree the fixture created.
//
// The ancestors of the root are resolved by the kernel exactly as
// filepath.EvalSymlinks resolves them, and the root's own final
// component is the one O_NOFOLLOW refuses, so the handle names the
// same directory the path names.
func openDestinationRoot(dst string) (*destinationRoot, error) {
	abs, err := filepath.Abs(dst)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0755); err != nil {
		return nil, fmt.Errorf("the fixture destination %s cannot be created: %w", abs, err)
	}
	fd, err := unix.Open(abs, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("the fixture destination %s cannot be opened as a directory: %w", abs, err)
	}
	return &destinationRoot{fd: fd}, nil
}

// writeFixtureFile writes one copied file into the destination.
//
// The destination is written the way the source is read: every
// component is opened relative to the directory handle above it, with
// O_NOFOLLOW, so no component can be a symlink and none can be swapped
// for one between the check and the open.
//
// The final component is created with O_EXCL, so the copy never
// overwrites anything that is already there.
func writeFixtureFile(dest *destinationRoot, rel string, raw []byte, mode os.FileMode) error {
	parts := strings.Split(rel, "/")
	parentFD, err := openDestinationDir(dest, parts[:len(parts)-1], rel)
	if err != nil {
		return err
	}
	if parentFD != dest.fd {
		defer func() { _ = unix.Close(parentFD) }()
	}
	return createFixtureFile(parentFD, parts[len(parts)-1], rel, raw, mode)
}

// openDestinationDir opens the directory that holds the final
// component of a tracked path, creating it if it does not exist.
//
// Each component is opened relative to the handle above it, with
// O_DIRECTORY|O_NOFOLLOW: a component that is a symlink cannot be
// entered, so a write cannot be redirected through one.
//
// A component that does not exist is created with mkdirat, which
// cannot create a symlink, and is then opened the same way. The open
// still refuses a symlink, so a component swapped in between the two
// is caught.
func openDestinationDir(dest *destinationRoot, parts []string, rel string) (int, error) {
	rootFD := dest.fd
	dirFD := rootFD
	for _, comp := range parts {
		next, err := unix.Openat(dirFD, comp, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil && errors.Is(err, unix.ENOENT) {
			if mkerr := unix.Mkdirat(dirFD, comp, 0755); mkerr != nil && !errors.Is(mkerr, unix.EEXIST) {
				if dirFD != rootFD {
					_ = unix.Close(dirFD)
				}
				return -1, fmt.Errorf("the fixture destination path %s cannot create the directory %q: %w", rel, comp, mkerr)
			}
			next, err = unix.Openat(dirFD, comp, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		if err != nil {
			terr := destinationPathError(rel, comp, dirFD, err)
			if dirFD != rootFD {
				_ = unix.Close(dirFD)
			}
			return -1, terr
		}
		if dirFD != rootFD {
			_ = unix.Close(dirFD)
		}
		dirFD = next
	}
	return dirFD, nil
}

// createFixtureFile creates the final component of a tracked path and
// writes the copied bytes into it.
//
// The open is CREATE-ONLY and NOFOLLOW:
//
//   - O_EXCL means the file cannot already exist. With O_CREAT, a
//     path that names a symlink fails with EEXIST whatever the link
//     points at, so a symlink at the final filename is refused rather
//     than written through;
//   - O_NOFOLLOW is the second, redundant gate on the same property.
//
// The fixture copies into a tree it has just created, so a file that is
// already there means the destination is not the one the fixture made,
// and the copy refuses it rather than overwriting whatever it finds.
func createFixtureFile(parentFD int, name, rel string, raw []byte, mode os.FileMode) error {
	fd, err := unix.Openat(parentFD, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(mode.Perm()))
	if err != nil {
		if errors.Is(err, unix.EEXIST) {
			if isSymlinkAt(parentFD, name) {
				return fmt.Errorf("the fixture destination path %s is a symlink; refusing to write through it", rel)
			}
			return fmt.Errorf("the fixture destination path %s already exists; refusing to write over it", rel)
		}
		return fmt.Errorf("the fixture destination path %s cannot be created: %w", rel, err)
	}
	if err := writeAll(fd, raw); err != nil {
		// A half-written file is worse than no file: remove it, so a
		// failed copy does not leave a truncated one behind.
		_ = unix.Unlinkat(parentFD, name, 0)
		_ = unix.Close(fd)
		return fmt.Errorf("the fixture destination path %s cannot be written: %w", rel, err)
	}
	if err := unix.Close(fd); err != nil {
		return fmt.Errorf("the fixture destination path %s cannot be closed: %w", rel, err)
	}
	return nil
}

// writeAll writes every byte of buf to fd.
func writeAll(fd int, buf []byte) error {
	for len(buf) > 0 {
		n, err := unix.Write(fd, buf)
		if n > 0 {
			buf = buf[n:]
		}
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

// destinationPathError explains why a destination component could not
// be opened.
//
// A component that is a symlink is named as one: the kernel reports
// ENOTDIR — not ELOOP — when a component that must be a directory is a
// symlink, because O_DIRECTORY|O_NOFOLLOW refuses the link without
// following it.
func destinationPathError(rel, comp string, dirFD int, err error) error {
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
		if isSymlinkAt(dirFD, comp) {
			return fmt.Errorf("the fixture destination path %s traverses the symlink %q; refusing to write through it", rel, comp)
		}
		return fmt.Errorf("the fixture destination path %s traverses %q, which is not a directory; refusing to write into it", rel, comp)
	}
	return fmt.Errorf("the fixture destination path %s cannot be opened: %w", rel, err)
}

// planSourceCopy enumerates exactly which files a fixture copy
// contains.
//
// It fails closed: a source that is not a git checkout has no bounded
// file set, and a copy without a boundary is the defect this replaces.
func planSourceCopy(src, dst string) ([]string, error) {
	absSrc, err := filepath.Abs(src)
	if err != nil {
		return nil, err
	}
	absDst, err := filepath.Abs(dst)
	if err != nil {
		return nil, err
	}
	if absSrc == absDst {
		return nil, fmt.Errorf("the fixture destination %s is the source itself", absDst)
	}
	if err := refuseOverlappingCopy(absSrc, absDst); err != nil {
		return nil, err
	}
	raw, err := runGitRaw(absSrc, "ls-files", "-z", "--", ".")
	if err != nil {
		return nil, fmt.Errorf("the fixture source %s is not a git checkout, so its file set cannot be bounded: %w", absSrc, err)
	}
	var files []string
	for _, name := range strings.Split(raw, "\x00") {
		if name == "" {
			continue
		}
		rel := filepath.ToSlash(filepath.Clean(name))
		if rel == "." || rel == ".." {
			continue
		}
		if hasParentElement(rel) {
			return nil, fmt.Errorf("the fixture source %s tracks the path %q, which climbs out of the source; refusing the copy", absSrc, name)
		}
		if filepath.Base(rel) == ".git" {
			// The repository's own metadata never travels: a linked
			// worktree's .git is a FILE pointing at the REAL
			// repository, and `git init` beside it would
			// reinitialize THAT repository.
			continue
		}
		// A tracked file INSIDE the destination would mean the copy
		// writes into a tree it is reading. Refuse rather than guess.
		if pathWithinDir(absDst, filepath.Join(absSrc, filepath.FromSlash(rel))) {
			return nil, fmt.Errorf("the fixture source %s tracks %s, which is inside the fixture destination %s; refusing a copy that would read and write the same tree", absSrc, rel, absDst)
		}
		files = append(files, rel)
	}
	sort.Strings(files)
	return files, nil
}

// hasParentElement reports whether a relative path climbs out of its
// own tree.
func hasParentElement(rel string) bool {
	for _, part := range strings.Split(rel, "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

// refuseOverlappingCopy refuses a source and destination that overlap.
//
// The comparison is made on RESOLVED paths, because an alias is
// exactly how a source and a destination overlap without their names
// saying so. Two shapes are refused:
//
//   - the destination resolves INSIDE the source (through an alias, or
//     through a path that leaves the source): the copy would read and
//     write the same tree;
//   - the source resolves INSIDE the destination: the copy would
//     overwrite the tree it reads.
//
// One shape is ALLOWED: a destination that is lexically inside the
// source, which is what TMPDIR inside a checkout looks like. That
// shape is safe because the copy's file set is the TRACKED set, and
// the destination is untracked by construction.
func refuseOverlappingCopy(absSrc, absDst string) error {
	realSrc, err := filepath.EvalSymlinks(absSrc)
	if err != nil {
		return fmt.Errorf("the fixture source %s cannot be resolved: %w", absSrc, err)
	}
	realDst, err := resolveDestinationPath(absDst)
	if err != nil {
		return fmt.Errorf("the fixture destination %s cannot be resolved: %w", absDst, err)
	}
	if realSrc == realDst {
		return fmt.Errorf("the fixture source %s and the destination %s resolve to the same directory (%s); refusing the copy", absSrc, absDst, realSrc)
	}
	dstInsideSrc := resolvedPathWithin(realSrc, realDst)
	srcInsideDst := resolvedPathWithin(realDst, realSrc)
	lexicalDstInsideSrc := pathWithinDir(absSrc, absDst)
	if lexicalDstInsideSrc && !dstInsideSrc {
		return fmt.Errorf("the fixture destination %s is inside the source %s but resolves to %s, which is outside it; refusing to write through an alias", absDst, absSrc, realDst)
	}
	if dstInsideSrc && !lexicalDstInsideSrc {
		return fmt.Errorf("the fixture destination %s resolves to %s, which is inside the source %s; refusing a copy that would read and write the same tree through an alias", absDst, realDst, realSrc)
	}
	if srcInsideDst {
		return fmt.Errorf("the fixture source %s resolves to %s, which is inside the destination %s; refusing a copy that would overwrite the tree it reads", absSrc, realSrc, absDst)
	}
	return nil
}

// resolveDestinationPath resolves a destination that may not exist yet.
//
// The nearest EXISTING ancestor is resolved and the remaining
// components are appended: the components that do not exist cannot be
// symlinks, so the resolved path is exact.
func resolveDestinationPath(absDst string) (string, error) {
	rest := ""
	cur := absDst
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(resolved, rest), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			// No existing ancestor: the path itself is the best
			// answer there is.
			return absDst, nil
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// resolvedPathWithin reports whether the RESOLVED target lies
// strictly inside the RESOLVED dir.
//
// Both paths must already be resolved. A lexical comparison of
// unresolved paths says nothing about where a symlink points, which is
// exactly the question this answers.
func resolvedPathWithin(dir, target string) bool {
	rel, err := filepath.Rel(dir, target)
	if err != nil {
		return false
	}
	if rel == "." || rel == ".." {
		return false
	}
	return !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// runGitRaw runs a git command and returns its RAW output.
//
// The output is not trimmed: `git ls-files -z` is a NUL-separated
// list, and trimming it would corrupt the last entry.
func runGitRaw(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %v in %s failed: %v", args, dir, err)
	}
	return string(out), nil
}
