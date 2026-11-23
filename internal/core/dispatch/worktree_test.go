package dispatch

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core/assignment"
)

// The WORKTREE PROVISIONER's contract.
//
// These are host-side tests against real Git repositories. They are
// deliberately adversarial: every refusal below is a shape a stale,
// forged or shared workspace could take, and none of them may be
// adopted.

func newGitRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q")
	run("config", "user.email", "worktree-test@example.invalid")
	run("config", "user.name", "Worktree Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-q", "-m", "the base revision")
	return dir, run("rev-parse", "HEAD")
}

func worktreeAssignment(id, base string) *assignment.Assignment {
	return &assignment.Assignment{
		ID:      id,
		Role:    assignment.RoleImplementer,
		BaseSHA: base,
	}
}

func newProvisioner(t *testing.T, repo, base string) (*WorktreeProvisioner, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "workspaces")
	return &WorktreeProvisioner{Root: root, Repository: repo}, base
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestTheProvisionerCreatesAWorktreeAtTheBase(t *testing.T) {
	repo, base := newGitRepo(t)
	p, _ := newProvisioner(t, repo, base)
	id := "at-0000000000000001"

	workspace, err := p.Ensure(worktreeAssignment(id, base))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if want := filepath.Join(p.Root, id); workspace != want {
		t.Errorf("the worktree is %q, want %q", workspace, want)
	}
	if got := gitOut(t, workspace, "rev-parse", "HEAD"); got != base {
		t.Errorf("the worktree HEAD is %s, want the base %s", got, base)
	}
	if got := gitOut(t, workspace, "symbolic-ref", "--short", "HEAD"); got != WorktreeBranchPrefix+id {
		t.Errorf("the worktree branch is %q, want %q", got, WorktreeBranchPrefix+id)
	}

	// THE POINTER IS A REGULAR FILE, AND ITS TARGET IS THE REPOSITORY'S
	// METADATA — OUTSIDE THE WORKER'S TREE.
	info, err := os.Lstat(filepath.Join(workspace, ".git"))
	if err != nil {
		t.Fatalf("stat the pointer: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("the .git entry is not a regular file: %s", info.Mode())
	}
	target := dotGitTarget(t, workspace)
	if within(workspace, target) {
		t.Errorf("the .git target %q is inside the worker's tree", target)
	}
	common := filepath.Join(repo, ".git", "worktrees")
	if !within(common, target) {
		t.Errorf("the .git target %q is not inside %q", target, common)
	}
}

func dotGitTarget(t *testing.T, workspace string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(workspace, ".git"))
	if err != nil {
		t.Fatalf("read the pointer: %v", err)
	}
	line := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(line, "gitdir: ") {
		t.Fatalf("malformed pointer: %q", line)
	}
	target := strings.TrimSpace(strings.TrimPrefix(line, "gitdir: "))
	if !filepath.IsAbs(target) {
		target = filepath.Join(workspace, target)
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatalf("resolve the pointer: %v", err)
	}
	return resolved
}

func TestTheProvisionerReusesTheSameWorktree(t *testing.T) {
	repo, base := newGitRepo(t)
	p, _ := newProvisioner(t, repo, base)
	id := "at-0000000000000002"
	a := worktreeAssignment(id, base)

	first, err := p.Ensure(a)
	if err != nil {
		t.Fatalf("the first provision: %v", err)
	}
	second, err := p.Ensure(a)
	if err != nil {
		t.Fatalf("the second provision: %v", err)
	}
	if first != second {
		t.Errorf("the worktree moved from %q to %q", first, second)
	}
	// Exactly ONE worktree for the assignment's branch.
	list := gitOut(t, repo, "worktree", "list", "--porcelain")
	count := 0
	for _, line := range strings.Split(list, "\n") {
		if strings.TrimSpace(line) == "branch refs/heads/"+WorktreeBranchPrefix+id {
			count++
		}
	}
	if count != 1 {
		t.Errorf("the repository lists %d worktrees for the branch, want 1\n%s", count, list)
	}
}

func TestTheProvisionerRefusesAnUnexpectedDirectory(t *testing.T) {
	repo, base := newGitRepo(t)
	p, _ := newProvisioner(t, repo, base)
	id := "at-0000000000000003"
	if err := os.MkdirAll(filepath.Join(p.Root, id), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Ensure(worktreeAssignment(id, base)); err == nil {
		t.Error("a bare pre-existing directory was adopted as a worktree")
	}
}

func TestTheProvisionerRefusesAnUnexpectedFile(t *testing.T) {
	repo, base := newGitRepo(t)
	p, _ := newProvisioner(t, repo, base)
	id := "at-0000000000000004"
	if err := os.MkdirAll(p.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.Root, id), []byte("not a worktree"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Ensure(worktreeAssignment(id, base)); err == nil {
		t.Error("a pre-existing file was adopted as a worktree")
	}
}

func TestTheProvisionerRefusesASymlinkedWorkspace(t *testing.T) {
	repo, base := newGitRepo(t)
	p, _ := newProvisioner(t, repo, base)
	id := "at-0000000000000005"
	if err := os.MkdirAll(p.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(p.Root, id)); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	if _, err := p.Ensure(worktreeAssignment(id, base)); err == nil {
		t.Error("a symlinked workspace was adopted")
	}
}

func TestTheProvisionerRefusesAFullClone(t *testing.T) {
	repo, base := newGitRepo(t)
	p, _ := newProvisioner(t, repo, base)
	id := "at-0000000000000006"
	if err := os.MkdirAll(p.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "clone", "-q", repo, filepath.Join(p.Root, id))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone: %v: %s", err, out)
	}
	_, err := p.Ensure(worktreeAssignment(id, base))
	if err == nil {
		t.Fatal("a full clone was adopted as a linked worktree")
	}
	if !strings.Contains(err.Error(), "full clone") {
		t.Errorf("the refusal does not name the full clone: %v", err)
	}
}

func TestTheProvisionerRefusesAnotherRepository(t *testing.T) {
	repo, base := newGitRepo(t)
	other, otherBase := newGitRepo(t)
	p, _ := newProvisioner(t, repo, base)
	id := "at-0000000000000007"
	if err := os.MkdirAll(p.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	// A worktree of ANOTHER repository at this assignment's path, on the
	// branch the assignment would use.
	cmd := exec.Command("git", "-C", other, "worktree", "add", "-b",
		WorktreeBranchPrefix+id, filepath.Join(p.Root, id), otherBase)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make the foreign worktree: %v: %s", err, out)
	}
	if _, err := p.Ensure(worktreeAssignment(id, base)); err == nil {
		t.Error("a worktree of another repository was adopted")
	}
}

func TestTheProvisionerRefusesTheWrongBranch(t *testing.T) {
	repo, base := newGitRepo(t)
	p, _ := newProvisioner(t, repo, base)
	id := "at-0000000000000008"
	workspace, err := p.Ensure(worktreeAssignment(id, base))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	gitOut(t, workspace, "checkout", "-q", "-b", "another-branch")
	if _, err := p.Ensure(worktreeAssignment(id, base)); err == nil {
		t.Error("a worktree on another branch was adopted")
	}
}

func TestTheProvisionerRefusesAStaleBase(t *testing.T) {
	repo, base := newGitRepo(t)
	p, _ := newProvisioner(t, repo, base)
	id := "at-0000000000000009"
	workspace, err := p.Ensure(worktreeAssignment(id, base))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	// An UNRELATED commit: a fresh root with no history in common.
	gitOut(t, repo, "checkout", "-q", "--orphan", "orphan")
	if err := os.WriteFile(filepath.Join(repo, "OTHER.md"), []byte("# other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitOut(t, repo, "add", "OTHER.md")
	gitOut(t, repo, "commit", "-q", "-m", "unrelated")
	unrelated := gitOut(t, repo, "rev-parse", "HEAD")
	gitOut(t, workspace, "reset", "--hard", unrelated)

	if _, err := p.Ensure(worktreeAssignment(id, base)); err == nil {
		t.Error("a worktree that does not contain the base was adopted")
	}
}

func TestTheProvisionerRefusesALeftoverBranch(t *testing.T) {
	repo, base := newGitRepo(t)
	p, _ := newProvisioner(t, repo, base)
	id := "at-000000000000000a"
	gitOut(t, repo, "branch", WorktreeBranchPrefix+id, base)
	if _, err := p.Ensure(worktreeAssignment(id, base)); err == nil {
		t.Error("a leftover branch was attached to a new worktree")
	}
}

func TestTheProvisionerRefusesATamperedPointer(t *testing.T) {
	repo, base := newGitRepo(t)
	p, _ := newProvisioner(t, repo, base)
	id := "at-000000000000000b"
	workspace, err := p.Ensure(worktreeAssignment(id, base))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".git"),
		[]byte("gitdir: /tmp/not-the-repository\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Ensure(worktreeAssignment(id, base)); err == nil {
		t.Error("a tampered .git pointer was adopted")
	}
}

func TestTheProvisionerRefusesAnAssignmentIDThatIsNotAPathShape(t *testing.T) {
	repo, base := newGitRepo(t)
	p, _ := newProvisioner(t, repo, base)
	for _, id := range []string{
		"at-000000000000000",     // 15 hex
		"at-00000000000000000",   // 17 hex
		"at-000000000000000G",    // not hex
		"../at-0000000000000001", // traversal
		"at-0000000000000001/x",  // separator
		"",                       // empty
	} {
		if _, err := p.Ensure(worktreeAssignment(id, base)); err == nil {
			t.Errorf("the id %q was accepted", id)
		}
	}
}

func TestTheProvisionerRefusesAReviewer(t *testing.T) {
	repo, base := newGitRepo(t)
	p, _ := newProvisioner(t, repo, base)
	a := worktreeAssignment("at-000000000000000c", base)
	a.Role = assignment.RoleReviewer
	if _, err := p.Ensure(a); err == nil {
		t.Error("a reviewer was provisioned a worktree")
	}
}

func TestTheProvisionerRefusesAWorkspaceRootInsideTheRepository(t *testing.T) {
	repo, base := newGitRepo(t)
	root := filepath.Join(repo, "workspaces")
	p := &WorktreeProvisioner{Root: root, Repository: repo}
	if _, err := p.Ensure(worktreeAssignment("at-000000000000000d", base)); err == nil {
		t.Error("a workspace root inside the source repository was accepted")
	}
	// AND THE REFUSAL LEFT NOTHING BEHIND: the containment checks run
	// before the root is created.
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("the refused configuration created the root %q (stat: %v)", root, err)
	}
}

func TestTheProvisionerRefusesARootEqualToTheRepository(t *testing.T) {
	repo, base := newGitRepo(t)
	p := &WorktreeProvisioner{Root: repo, Repository: repo}
	if _, err := p.Ensure(worktreeAssignment("at-0000000000000010", base)); err == nil {
		t.Error("a workspace root equal to the source repository was accepted")
	}
	// No worktree inside the source checkout.
	if _, err := os.Stat(filepath.Join(repo, "at-0000000000000010")); !os.IsNotExist(err) {
		t.Errorf("the refusal created a worktree inside the source repository (stat: %v)", err)
	}
}

func TestTheProvisionerRefusesAMovedWorktree(t *testing.T) {
	repo, base := newGitRepo(t)
	p, _ := newProvisioner(t, repo, base)
	first := worktreeAssignment("at-0000000000000011", base)
	workspace, err := p.Ensure(first)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	// The worktree is MOVED. Its .git pointer is still valid, but the
	// metadata's backlink still names the old workspace.
	moved := filepath.Join(p.Root, "at-0000000000000012")
	if err := os.Rename(workspace, moved); err != nil {
		t.Fatalf("move the worktree: %v", err)
	}
	second := worktreeAssignment("at-0000000000000012", base)
	if _, err := p.Ensure(second); err == nil {
		t.Error("a moved worktree was adopted")
	}
}

func TestTheProvisionerRefusesAPointerToTheMainGitDirectory(t *testing.T) {
	repo, base := newGitRepo(t)
	p, _ := newProvisioner(t, repo, base)
	id := "at-0000000000000013"
	workspace, err := p.Ensure(worktreeAssignment(id, base))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	// A pointer to the MAIN .git exists (so EvalSymlinks succeeds) but
	// is not inside .git/worktrees/.
	pointer := "gitdir: " + filepath.Join(repo, ".git") + "\n"
	if err := os.WriteFile(filepath.Join(workspace, ".git"), []byte(pointer), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Ensure(worktreeAssignment(id, base)); err == nil {
		t.Error("a pointer to the main .git directory was adopted")
	}
}

func TestTheProvisionerRefusesAMissingBase(t *testing.T) {
	repo, _ := newGitRepo(t)
	p, _ := newProvisioner(t, repo, strings.Repeat("f", 40))
	if _, err := p.Ensure(worktreeAssignment("at-000000000000000e", strings.Repeat("f", 40))); err == nil {
		t.Error("a base revision that is not in the repository was accepted")
	}
}

// ── integration with the dispatcher ─────────────────────────────────────

// TestTheDispatcherHandsTheWorkerARealWorktree proves the launch does not
// receive an empty directory.
//
// The fake runtime reports the workspace it was handed; that path must be
// a real linked worktree at the assignment's base, on its branch, with
// its Git metadata outside the worker's tree.
func TestTheDispatcherHandsTheWorkerARealWorktree(t *testing.T) {
	store, projectID := testStore(t)
	root := dispatchRoot(t, store, projectID)
	child := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:wt:integration")

	cfg := testConfig(t)
	engine, fake := testEngine(t, store, cfg)
	fake.launchState = okState("mpm-worker", "succeeded")

	if _, err := engine.RunOnce(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	workspace := fake.workspace
	if workspace == "" {
		t.Fatal("the runtime was handed no workspace")
	}
	if got := gitOut(t, workspace, "rev-parse", "HEAD"); got != child.BaseSHA {
		t.Errorf("the worktree HEAD is %s, want the base %s", got, child.BaseSHA)
	}
	if got := gitOut(t, workspace, "symbolic-ref", "--short", "HEAD"); got != WorktreeBranchPrefix+child.ID {
		t.Errorf("the worktree branch is %q, want %q", got, WorktreeBranchPrefix+child.ID)
	}
	target := dotGitTarget(t, workspace)
	if within(workspace, target) {
		t.Errorf("the worker's tree contains its own Git metadata: %s", target)
	}
}

// TestARecreatedDispatcherFindsTheSameWorktree proves recovery identity:
// a second Engine over the same configuration and store resolves the same
// worktree, with the same base and branch.
func TestARecreatedDispatcherFindsTheSameWorktree(t *testing.T) {
	store, projectID := testStore(t)
	root := dispatchRoot(t, store, projectID)
	child := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:wt:restart")
	cfg := testConfig(t)

	first, err := NewEngine(store, cfg, func(context.Context, *assignment.Assignment, string) (
		assignment.Runtime, assignment.ReviewRuntime, error) {
		return newFakeRuntime(), newFakeRuntime(), nil
	})
	if err != nil {
		t.Fatalf("build the first engine: %v", err)
	}
	before, err := first.workspaceFor(child)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	second, err := NewEngine(store, cfg, func(context.Context, *assignment.Assignment, string) (
		assignment.Runtime, assignment.ReviewRuntime, error) {
		return newFakeRuntime(), newFakeRuntime(), nil
	})
	if err != nil {
		t.Fatalf("build the second engine: %v", err)
	}
	after, err := second.workspaceFor(child)
	if err != nil {
		t.Fatalf("re-provision after the restart: %v", err)
	}
	if before != after {
		t.Errorf("the worktree moved from %q to %q across a dispatcher restart", before, after)
	}
	if got := gitOut(t, after, "rev-parse", "HEAD"); got != child.BaseSHA {
		t.Errorf("the recovered worktree HEAD is %s, want the base %s", got, child.BaseSHA)
	}
}

// TestALiveLeaseDoesNotProvisionAWorkspace proves a second claimant does
// not touch the first claimant's workspace.
//
// The claim is refused BEFORE the provisioner runs, so a stale or second
// writer cannot create a worktree, adopt one, or stage work in it.
func TestALiveLeaseDoesNotProvisionAWorkspace(t *testing.T) {
	store, projectID := testStore(t)
	root := dispatchRoot(t, store, projectID)
	child := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:wt:lease")

	if _, err := store.Claim(assignment.ClaimParams{
		ID: child.ID, ExpectedEpoch: 0, Owner: "another-writer", LeaseSeconds: 1800,
		Actor: assignment.Actor{Address: "another-writer", Kind: "agent"},
	}); err != nil {
		t.Fatalf("the first claim: %v", err)
	}

	cfg := testConfig(t)
	engine, fake := testEngine(t, store, cfg)
	fake.launchState = okState("mpm-worker", "succeeded")
	report, err := engine.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if fake.Launches() != 0 {
		t.Errorf("the dispatcher launched %d session(s) under a live lease", fake.Launches())
	}
	outcome, ok := outcomeFor(report, child.ID)
	if !ok {
		t.Fatalf("the report has no outcome: %+v", report.Outcomes)
	}
	if outcome.Reason != ReasonLeaseHeld {
		t.Errorf("the outcome is %q, want %q", outcome.Reason, ReasonLeaseHeld)
	}
	entries, err := os.ReadDir(cfg.WorkspaceRoot)
	if err == nil && len(entries) != 0 {
		t.Errorf("the refused claim provisioned %d workspace entr(ies)", len(entries))
	}
	if err != nil && !os.IsNotExist(err) {
		t.Errorf("read the workspace root: %v", err)
	}
}

// TestTheProvisionerIgnoresAnAmbientGitDir proves the configured path is
// the only repository the provisioner uses.
//
// `GIT_DIR` overrides `git -C`. A dispatcher started from a hook or a
// shell with it set would otherwise create the worktree in another
// repository, and the verification would compare that repository with
// itself and pass.
func TestTheProvisionerIgnoresAnAmbientGitDir(t *testing.T) {
	repo, base := newGitRepo(t)
	other, _ := newGitRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)

	p, _ := newProvisioner(t, repo, base)
	workspace, err := p.Ensure(worktreeAssignment("at-000000000000000f", base))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	target := dotGitTarget(t, workspace)
	want := filepath.Join(repo, ".git", "worktrees")
	if !within(want, target) {
		t.Errorf("the worktree's metadata is %q, want it inside %q (the configured repository)", target, want)
	}
}

// TestANewEngineRefusesAResolvedRootInsideTheRepository proves the
// pre-claim check sees through a symlinked root.
//
// The lexical configuration check compares the strings, so a symlink that
// resolves into the repository passes it. Ensure would catch it — after
// the claim, leaving a live lease and no attempt.
func TestANewEngineRefusesAResolvedRootInsideTheRepository(t *testing.T) {
	repo, _ := newGitRepo(t)
	sub := filepath.Join(repo, "worktrees")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(sub, link); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	store, _ := testStore(t)
	cfg := testConfig(t)
	cfg.WorkspaceRoot = link
	cfg.RepositoryPath = repo
	if _, err := NewEngine(store, cfg, func(context.Context, *assignment.Assignment, string) (
		assignment.Runtime, assignment.ReviewRuntime, error) {
		return newFakeRuntime(), nil, nil
	}); err == nil {
		t.Error("a symlinked workspace root resolving inside the repository was accepted before the claim")
	}
}

// TestANewEngineRefusesABareRepository proves the pre-claim repository
// check covers the bare shape.
func TestANewEngineRefusesABareRepository(t *testing.T) {
	bare := filepath.Join(t.TempDir(), "bare.git")
	cmd := exec.Command("git", "init", "-q", "--bare", bare)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("init --bare: %v: %s", err, out)
	}
	store, _ := testStore(t)
	cfg := testConfig(t)
	cfg.RepositoryPath = bare
	if _, err := NewEngine(store, cfg, func(context.Context, *assignment.Assignment, string) (
		assignment.Runtime, assignment.ReviewRuntime, error) {
		return newFakeRuntime(), nil, nil
	}); err == nil {
		t.Error("a bare repository was accepted before the claim")
	}
}

// TestTheProvisionerRefusesMetadataOutsideTheWorktreeDirectory isolates
// the `within` rule.
//
// The fake metadata is a real moved metadata directory: it carries a
// valid commondir, a backlink to this workspace, the assignment's branch
// and the base HEAD. Every other rule passes; only the refusal to read
// metadata outside `<repo>/.git/worktrees/` rejects it.
func TestTheProvisionerRefusesMetadataOutsideTheWorktreeDirectory(t *testing.T) {
	repo, base := newGitRepo(t)
	p, _ := newProvisioner(t, repo, base)
	id := "at-0000000000000014"
	workspace, err := p.Ensure(worktreeAssignment(id, base))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	pointer := dotGitTarget(t, workspace)
	fakeMeta := filepath.Join(p.Root, "fake-meta")
	if err := os.Rename(pointer, fakeMeta); err != nil {
		t.Fatalf("move the metadata: %v", err)
	}
	// The pointer now names the moved metadata; the backlink names this
	// workspace; commondir is relative to the new location.
	if err := os.WriteFile(filepath.Join(workspace, ".git"),
		[]byte("gitdir: "+fakeMeta+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeMeta, "gitdir"),
		[]byte(filepath.Join(workspace, ".git")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(fakeMeta, filepath.Join(repo, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeMeta, "commondir"), []byte(rel+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The common dir really is the repository's, and the branch/HEAD
	// really are the assignment's. This is what makes "every other rule
	// passes" a measured fact rather than a comment.
	common := gitOut(t, workspace, "rev-parse", "--git-common-dir")
	if !filepath.IsAbs(common) {
		common = filepath.Join(workspace, common)
	}
	gotCommon, err := filepath.EvalSymlinks(common)
	if err != nil {
		t.Fatalf("resolve the common dir %q: %v", common, err)
	}
	wantCommon, err := filepath.EvalSymlinks(filepath.Join(repo, ".git"))
	if err != nil {
		t.Fatalf("resolve the repository's git dir: %v", err)
	}
	if gotCommon != wantCommon {
		t.Fatalf("the fake metadata's common dir is %q, want %q", gotCommon, wantCommon)
	}
	if got := gitOut(t, workspace, "symbolic-ref", "--short", "HEAD"); got != WorktreeBranchPrefix+id {
		t.Fatalf("the fake metadata's branch is %q, want %q", got, WorktreeBranchPrefix+id)
	}
	if got := gitOut(t, workspace, "rev-parse", "HEAD"); got != base {
		t.Fatalf("the fake metadata's HEAD is %q, want the base %q", got, base)
	}
	_, err = p.Ensure(worktreeAssignment(id, base))
	if err == nil {
		t.Fatal("metadata outside .git/worktrees was adopted")
	}
	if !strings.Contains(err.Error(), "outside the repository's worktree metadata") {
		t.Errorf("refused, but not by the metadata rule: %v", err)
	}
}

// TestTheProvisionerRefusesASameIDMove isolates the backlink rule.
//
// The moved workspace has the SAME assignment id, so the branch, the
// common dir, the base and the metadata location all pass. Only the
// backlink reveals that the metadata belongs to the old path.
func TestTheProvisionerRefusesASameIDMove(t *testing.T) {
	repo, base := newGitRepo(t)
	first := &WorktreeProvisioner{Root: filepath.Join(t.TempDir(), "one"), Repository: repo}
	id := "at-0000000000000015"
	original, err := first.Ensure(worktreeAssignment(id, base))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	second := &WorktreeProvisioner{Root: filepath.Join(t.TempDir(), "two"), Repository: repo}
	if err := os.MkdirAll(second.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(second.Root, id)
	if err := os.Rename(original, moved); err != nil {
		t.Fatalf("move the workspace: %v", err)
	}
	_, err = second.Ensure(worktreeAssignment(id, base))
	if err == nil {
		t.Fatal("a same-id moved workspace was adopted")
	}
	if !strings.Contains(err.Error(), "points back at") {
		t.Errorf("refused, but not by the backlink rule: %v", err)
	}
}

// TestANewEngineRefusesAWorkspaceRootThatIsAFile proves the pre-claim
// check refuses an existing root that is not a directory.
//
// Ensure would fail on it after the claim, leaving a live lease and no
// attempt, so MaxDispatchAttempts could never bound the retry.
func TestANewEngineRefusesAWorkspaceRootThatIsAFile(t *testing.T) {
	repo, _ := newGitRepo(t)
	file := filepath.Join(t.TempDir(), "root-file")
	if err := os.WriteFile(file, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, _ := testStore(t)
	cfg := testConfig(t)
	cfg.WorkspaceRoot = file
	cfg.RepositoryPath = repo
	if _, err := NewEngine(store, cfg, func(context.Context, *assignment.Assignment, string) (
		assignment.Runtime, assignment.ReviewRuntime, error) {
		return newFakeRuntime(), nil, nil
	}); err == nil {
		t.Error("a workspace root that is a regular file was accepted before the claim")
	}
}

// TestANewEngineRefusesAResolvedRootEqualToTheRepository proves the
// pre-claim check sees a symlinked root that resolves exactly to the
// repository.
func TestANewEngineRefusesAResolvedRootEqualToTheRepository(t *testing.T) {
	repo, _ := newGitRepo(t)
	link := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(repo, link); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	store, _ := testStore(t)
	cfg := testConfig(t)
	cfg.WorkspaceRoot = link
	cfg.RepositoryPath = repo
	if _, err := NewEngine(store, cfg, func(context.Context, *assignment.Assignment, string) (
		assignment.Runtime, assignment.ReviewRuntime, error) {
		return newFakeRuntime(), nil, nil
	}); err == nil {
		t.Error("a symlinked workspace root resolving to the repository was accepted before the claim")
	}
}

// TestANewEngineRefusesADanglingSymlinkRoot proves the pre-claim root
// check refuses a root that is a symlink to a nonexistent target.
//
// os.Stat alone follows the link and reports "no such file", so the root
// looks creatable. Ensure would then fail on it after the claim, leaving
// a live lease and no attempt.
func TestANewEngineRefusesADanglingSymlinkRoot(t *testing.T) {
	repo, _ := newGitRepo(t)
	link := filepath.Join(t.TempDir(), "dangling-root")
	if err := os.Symlink(filepath.Join(t.TempDir(), "does-not-exist"), link); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	store, _ := testStore(t)
	cfg := testConfig(t)
	cfg.WorkspaceRoot = link
	cfg.RepositoryPath = repo
	if _, err := NewEngine(store, cfg, func(context.Context, *assignment.Assignment, string) (
		assignment.Runtime, assignment.ReviewRuntime, error) {
		return newFakeRuntime(), nil, nil
	}); err == nil {
		t.Error("a dangling symlink workspace root was accepted before the claim")
	}
}
