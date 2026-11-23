package dispatch

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/flowbyte-com/mpm-core/assignment"
)

// The WORKTREE PROVISIONER.
//
// # WHAT PR 4 DID, AND WHAT WAS MISSING
//
// PR 4 created an EMPTY DIRECTORY per assignment. That is enough to run a
// file-edit-only worker against nothing, and it is not enough to do
// engineering work: there is no revision, no branch, no history and no
// way to review what changed. This file replaces the directory with a
// GENUINE Git worktree, created by the HOST.
//
// # THE WORKER DOES NOT NEED GIT
//
// The worker is file-edit-only. It never runs `git`. The host creates the
// worktree, stages and commits; the worker edits the checked-out files.
// That is why the worktree can be linked to the host repository without
// giving the worker the repository's metadata: the worker only needs the
// files, and the files are all it is bound to.
//
// # THE `.git` POINTER, EXPLICITLY
//
// A linked worktree's `.git` is a FILE containing
// `gitdir: /path/to/repo/.git/worktrees/<name>`. That path is outside the
// worker's namespace, so the worker cannot reach it — but the pointer is
// still handled deliberately:
//
//   - it must be a REGULAR FILE, not a symlink and not a directory (a
//     directory means a full clone, which this design does not silently
//     adopt);
//   - its target must resolve INSIDE the source repository's
//     `.git/worktrees/` directory;
//   - the worktree must report the same common Git directory as the
//     configured repository, the same branch, and a HEAD that contains
//     the authorised base.
//
// A workspace that fails any of those checks is REFUSED, not adopted.
// The recovery path re-runs them before reusing a worktree, so a tampered
// pointer is an operator-visible refusal rather than a silent reuse.

const (
	// WorktreeBranchPrefix is the branch each assignment's work lives on.
	// It is derived from the assignment id, so recovery finds the same
	// branch and two assignments cannot share one.
	WorktreeBranchPrefix = "mpm/assignment/"

	// worktreeGitTimeout bounds one host-side Git command. Provisioning
	// runs before the runtime and must not hang a dispatch pass.
	worktreeGitTimeout = 60 * time.Second
)

// Bounded shapes. The assignment id and the base SHA are the only two
// inputs to the path and the branch, so they are validated before either
// is built.
var (
	assignmentIDShape = regexp.MustCompile(`^at-[0-9a-f]{16}$`)
	baseSHAShape      = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
)

// WorktreeProvisioner creates and verifies the authorised worktree of one
// implementation assignment.
//
// It is HOST-side. The worker never runs it and never sees the source
// repository.
type WorktreeProvisioner struct {
	// Root is the operator-configured workspace root. Every worktree is a
	// direct child of it.
	Root string
	// Repository is the operator-configured source repository. It must be
	// a non-bare Git checkout.
	Repository string
}

// Ensure returns the verified worktree of one assignment, creating it when
// it does not exist.
//
// The result is the worktree's absolute path. The same assignment, run
// twice, gets the same path: the workspace is keyed by the assignment id,
// and a retry must not find a different tree.
func (p *WorktreeProvisioner) Ensure(a *assignment.Assignment) (string, error) {
	if a == nil {
		return "", fmt.Errorf("dispatch: there is no assignment to provision a worktree for")
	}
	if a.Role == assignment.RoleReviewer {
		return "", fmt.Errorf("dispatch: a reviewer has no worktree; it examines the configured review repository")
	}
	if !assignmentIDShape.MatchString(a.ID) {
		return "", fmt.Errorf("dispatch: the assignment id %q is not the shape a worktree path is built from", a.ID)
	}
	if !baseSHAShape.MatchString(a.BaseSHA) {
		return "", fmt.Errorf(
			"dispatch: the assignment %s names no exact base revision (%q); a worktree is created from one",
			a.ID, a.BaseSHA)
	}
	if strings.TrimSpace(p.Root) == "" {
		return "", fmt.Errorf("dispatch: no workspace root is configured")
	}
	if strings.TrimSpace(p.Repository) == "" {
		return "", fmt.Errorf("dispatch: no source repository is configured; a worktree needs one")
	}
	if !filepath.IsAbs(p.Root) || !filepath.IsAbs(p.Repository) {
		return "", fmt.Errorf("dispatch: the workspace root and the repository must be absolute paths")
	}

	root, err := ensureDirectory(p.Root)
	if err != nil {
		return "", err
	}
	repo, err := filepath.EvalSymlinks(p.Repository)
	if err != nil {
		return "", fmt.Errorf("dispatch: resolve the source repository %q: %w", p.Repository, err)
	}
	if within(repo, root) || within(root, repo) {
		return "", fmt.Errorf(
			"dispatch: the workspace root %q and the repository %q must not contain one another",
			root, repo)
	}
	if err := p.requireRepository(repo); err != nil {
		return "", err
	}

	workspace := filepath.Join(root, a.ID)
	info, statErr := os.Lstat(workspace)
	switch {
	case statErr == nil:
		return p.verifyExisting(a, workspace, root, repo, info)
	case os.IsNotExist(statErr):
		return p.create(a, workspace, root, repo)
	default:
		return "", fmt.Errorf("dispatch: inspect the workspace %q: %w", workspace, statErr)
	}
}

// create builds the worktree when the path does not exist.
func (p *WorktreeProvisioner) create(a *assignment.Assignment, workspace, root, repo string) (string, error) {
	branch := WorktreeBranchPrefix + a.ID

	// THE BASE REVISION MUST EXIST. A typo in an operator's base SHA is
	// refused here, before Git is asked to check anything out.
	if _, err := p.git(repo, "cat-file", "-e", a.BaseSHA+"^{commit}"); err != nil {
		return "", fmt.Errorf("dispatch: the base revision %s is not in %s: %w", a.BaseSHA, repo, err)
	}

	// A LEFTOVER BRANCH WITHOUT ITS WORKTREE IS AN OPERATOR DECISION.
	//
	// Reusing it would silently attach a new worktree to a branch whose
	// history the dispatcher did not create; deleting it would destroy
	// work. Neither is the dispatcher's call.
	if _, err := p.git(repo, "show-ref", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		return "", fmt.Errorf(
			"dispatch: the branch %q exists but its worktree %q does not; refusing to guess which one is stale",
			branch, workspace)
	}

	if _, err := p.git(repo, "worktree", "add", "-b", branch, workspace, a.BaseSHA); err != nil {
		return "", fmt.Errorf("dispatch: create the worktree for %s: %w", a.ID, err)
	}

	info, err := os.Lstat(workspace)
	if err != nil {
		return "", fmt.Errorf("dispatch: the worktree for %s was created but cannot be inspected: %w", a.ID, err)
	}
	return p.verifyExisting(a, workspace, root, repo, info)
}

// verifyExisting checks a pre-existing workspace and refuses anything it
// did not create.
func (p *WorktreeProvisioner) verifyExisting(a *assignment.Assignment,
	workspace, root, repo string, info os.FileInfo) (string, error) {
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("dispatch: the workspace %q is a symlink; refusing to adopt it", workspace)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("dispatch: the workspace %q is not a directory; refusing to adopt it", workspace)
	}
	// A SYMLINK ANYWHERE IN THE PATH IS A WAY OUT OF THE ROOT. The
	// resolved path must be the path itself, and it must still be a
	// direct child of the resolved root.
	resolved, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", fmt.Errorf("dispatch: resolve the workspace %q: %w", workspace, err)
	}
	if resolved != workspace {
		return "", fmt.Errorf(
			"dispatch: the workspace %q resolves to %q; a symlinked workspace is never adopted",
			workspace, resolved)
	}
	if filepath.Dir(resolved) != root {
		return "", fmt.Errorf(
			"dispatch: the workspace %q is not a direct child of the configured root %q", resolved, root)
	}

	// THE `.git` POINTER IS A FILE, AND ITS TARGET IS THE REPOSITORY'S
	// WORKTREE METADATA.
	if err := p.verifyDotGit(workspace, repo); err != nil {
		return "", err
	}

	// THE WORKTREE MUST BELONG TO THIS REPOSITORY.
	common, err := p.commonDir(workspace)
	if err != nil {
		return "", err
	}
	repoCommon, err := p.commonDir(repo)
	if err != nil {
		return "", err
	}
	if common != repoCommon {
		return "", fmt.Errorf(
			"dispatch: the workspace %q belongs to %q, and the configured repository is %q",
			workspace, common, repoCommon)
	}

	// AND TO THIS ASSIGNMENT.
	branch, err := p.git(workspace, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", fmt.Errorf("dispatch: read the workspace's branch: %w", err)
	}
	if strings.TrimSpace(branch) != WorktreeBranchPrefix+a.ID {
		return "", fmt.Errorf(
			"dispatch: the workspace %q is on branch %q, and the assignment's branch is %q",
			workspace, strings.TrimSpace(branch), WorktreeBranchPrefix+a.ID)
	}

	// THE AUTHORISED BASE IS AN ANCESTOR OF HEAD. At creation HEAD is the
	// base; after the host commits, HEAD is ahead of it. A worktree whose
	// history does not contain the base is not this assignment's work.
	head, err := p.git(workspace, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("dispatch: read the workspace's HEAD: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(head), a.BaseSHA) {
		if _, err := p.git(workspace, "merge-base", "--is-ancestor", a.BaseSHA, "HEAD"); err != nil {
			return "", fmt.Errorf(
				"dispatch: the workspace %q does not contain the authorised base %s",
				workspace, a.BaseSHA)
		}
	}
	return resolved, nil
}

// verifyDotGit checks the linked-worktree pointer.
func (p *WorktreeProvisioner) verifyDotGit(workspace, repo string) error {
	dotGit := filepath.Join(workspace, ".git")
	info, err := os.Lstat(dotGit)
	if err != nil {
		return fmt.Errorf("dispatch: the workspace %q has no .git entry: %w", workspace, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("dispatch: the workspace %q has a symlinked .git; refusing to adopt it", workspace)
	}
	if info.IsDir() {
		return fmt.Errorf(
			"dispatch: the workspace %q is a full clone (a .git directory), not a linked worktree; refusing to silently adopt it",
			workspace)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("dispatch: the workspace %q has a .git entry that is not a file", workspace)
	}
	raw, err := os.ReadFile(dotGit)
	if err != nil {
		return fmt.Errorf("dispatch: read the workspace's .git pointer: %w", err)
	}
	line := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(line, "gitdir: ") {
		return fmt.Errorf("dispatch: the workspace %q has a malformed .git pointer", workspace)
	}
	target := strings.TrimSpace(strings.TrimPrefix(line, "gitdir: "))
	if target == "" {
		return fmt.Errorf("dispatch: the workspace %q has an empty .git pointer", workspace)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(workspace, target)
	}
	target, err = filepath.EvalSymlinks(target)
	if err != nil {
		return fmt.Errorf("dispatch: resolve the workspace's .git pointer %q: %w", target, err)
	}
	repoCommon, err := p.commonDir(repo)
	if err != nil {
		return err
	}
	worktrees := filepath.Join(repoCommon, "worktrees")
	if !within(worktrees, target) {
		return fmt.Errorf(
			"dispatch: the workspace %q points its .git at %q, which is outside the repository's worktree metadata",
			workspace, target)
	}
	return nil
}

// requireRepository checks that the configured path is a non-bare Git
// checkout.
func (p *WorktreeProvisioner) requireRepository(repo string) error {
	bare, err := p.git(repo, "rev-parse", "--is-bare-repository")
	if err != nil {
		return fmt.Errorf("dispatch: %q is not a Git repository: %w", repo, err)
	}
	if strings.TrimSpace(bare) != "false" {
		return fmt.Errorf("dispatch: %q is a bare repository; a worktree needs a checkout", repo)
	}
	return nil
}

// commonDir returns the repository's common Git directory, resolved.
//
// For a linked worktree the common directory is the MAIN repository's
// `.git`; for the main checkout it is its own `.git`. That is what makes
// "does this worktree belong to this repository?" answerable without
// comparing paths the operator typed.
func (p *WorktreeProvisioner) commonDir(dir string) (string, error) {
	out, err := p.git(dir, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("dispatch: read the common Git directory of %q: %w", dir, err)
	}
	common := strings.TrimSpace(out)
	if !filepath.IsAbs(common) {
		common = filepath.Join(dir, common)
	}
	resolved, err := filepath.EvalSymlinks(common)
	if err != nil {
		return "", fmt.Errorf("dispatch: resolve the common Git directory %q: %w", common, err)
	}
	return resolved, nil
}

// git runs one Git command in dir. It is HOST-side, no shell is involved,
// and its output is bounded by the caller's use (branch names, SHAs,
// short status lines).
func (p *WorktreeProvisioner) git(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), worktreeGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitEnvironment()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("git %s timed out", strings.Join(args, " "))
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// gitEnvironment is the host environment without any ambient Git
// redirection.
//
// `GIT_DIR`, `GIT_WORK_TREE` and their relatives override the repository
// selected by `git -C`. A dispatcher started from a Git hook or a shell
// with one of them set would otherwise create the worktree in a
// repository the operator did not configure — and the verification would
// compare that same redirected repository with itself and pass. The
// configured path must be the only repository the provisioner uses.
func gitEnvironment() []string {
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			name = kv[:i]
		}
		if strings.HasPrefix(name, "GIT_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// ensureDirectory creates the workspace root when it does not exist and
// resolves it.
func ensureDirectory(path string) (string, error) {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return "", fmt.Errorf("dispatch: create the workspace root %q: %w", path, err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("dispatch: resolve the workspace root %q: %w", path, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("dispatch: inspect the workspace root %q: %w", resolved, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("dispatch: the workspace root %q is not a directory", resolved)
	}
	return resolved, nil
}

// within reports whether child is a strict descendant of parent.
func within(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}
