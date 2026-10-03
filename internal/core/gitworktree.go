package internal

import (
	"os/exec"
	"strings"
)

// isInsideGitWorktree reports whether dir lies inside a Git working tree.
//
// It asks Git directly rather than looking for a `.git` entry, because the
// shapes differ by context:
//
//   - a normal checkout has `.git` as a *directory*;
//   - a linked worktree (`git worktree add`) has `.git` as a regular *file*
//     containing a `gitdir:` pointer;
//   - a submodule has a `.git` file too.
//
// A `.git` directory check therefore misses the linked-worktree case, which is
// exactly the shape a checkout managed by an agent or a worktree-per-agent
// setup produces. `git rev-parse --is-inside-work-tree` is authoritative for
// all three, and also resolves nested subdirectories by walking up to the
// repository root.
//
// A missing or non-executable git binary is reported as "not inside a
// worktree" — the answer is used to *refuse* destructive bulk operations, so
// the failure direction is safe.
func isInsideGitWorktree(dir string) bool {
	if dir == "" {
		return false
	}
	cmd := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}
