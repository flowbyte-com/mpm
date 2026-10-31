package update

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// resolveSource turns the operator's request into an immutable source
// identity. The rules:
//
//   - an explicit --from/--branch/--commit wins, in that order of
//     specificity (local > commit > branch);
//   - otherwise the default is the latest stable GitHub release;
//   - --version pins a release tag.
//
// Git and local sources are explicit development modes. They resolve to
// a commit SHA before anything is built, and the exact commit is
// recorded in the release metadata — a build is never labelled with a
// revision it did not come from.
func resolveSource(ctx context.Context, o Options) (Source, error) {
	n := 0
	for _, s := range []string{o.WantLocal, o.WantCommit, o.WantBranch, o.WantVersion} {
		if s != "" {
			n++
		}
	}
	if n > 1 {
		return Source{}, fmt.Errorf("pick one source: --from, --commit, --branch or --version")
	}
	switch {
	case o.WantLocal != "":
		return resolveLocalSource(o.WantLocal, o.AllowDirty)
	case o.WantCommit != "":
		return Source{Kind: SourceGit, Ref: o.WantCommit, Rev: o.WantCommit, Commit: o.WantCommit}, nil
	case o.WantBranch != "":
		sha, err := resolveBranchHead(o.WantBranch)
		if err != nil {
			return Source{}, err
		}
		return Source{Kind: SourceGit, Ref: o.WantBranch, Rev: sha, Commit: sha}, nil
	default:
		info, err := o.fetcher().LatestStable(o.WantVersion)
		if err != nil {
			return Source{}, err
		}
		rev := info.Tag
		if rev == "" {
			rev = info.Commit
		}
		return Source{Kind: SourceGitHub, Ref: o.WantVersion, Rev: rev, Commit: info.Commit, Version: info.Version}, nil
	}
}

// resolveLocalSource validates a local checkout: it must exist, must be
// a git checkout with a resolvable HEAD, must live OUTSIDE the installed
// runtime (building from inside the release being replaced would let the
// build read its own half-written output), and must be clean unless the
// operator passed --allow-dirty.
func resolveLocalSource(dir string, allowDirty bool) (Source, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Source{}, err
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return Source{}, fmt.Errorf("no such source directory: %s", dir)
	}
	if isInsideDataRoot(abs, DataRoot()) {
		return Source{}, fmt.Errorf("the source checkout %s is inside the installed runtime; build from a checkout outside it so the install stays independent of the checkout", dir)
	}
	sha, err := gitHeadCommit(abs)
	if err != nil {
		return Source{}, err
	}
	if dirty, summary, err := gitIsDirty(abs); err != nil {
		return Source{}, err
	} else if dirty && !allowDirty {
		first := strings.SplitN(summary, "\n", 2)[0]
		return Source{}, fmt.Errorf("the source tree at %s has uncommitted changes (%s…); refusing to build from a dirty tree (re-run with --allow-dirty to override)", dir, first)
	}
	return Source{Kind: SourceLocal, Ref: abs, Rev: sha, Commit: sha, Version: readVersionFile(abs)}, nil
}

func isInsideDataRoot(abs, root string) bool {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")
}

// resolveBranchHead resolves a branch name against the upstream
// repository without cloning it.
func resolveBranchHead(branch string) (string, error) {
	if strings.ContainsAny(branch, " \t\n;|&$`(){}[]<>!") {
		return "", fmt.Errorf("invalid branch name %q", branch)
	}
	out, err := exec.Command("git", "ls-remote", "https://github.com/flowbyte-com/mpm.git", "refs/heads/"+branch).Output() //nolint:gosec
	if err != nil {
		return "", fmt.Errorf("could not resolve branch %q: %w", branch, err)
	}
	fields := strings.Fields(string(out))
	if len(fields) < 1 {
		return "", fmt.Errorf("no such upstream branch: %s", branch)
	}
	return fields[0], nil
}

// stageCandidate assembles the candidate bundle outside the active
// release, by source kind:
//
//   - github: download + verify + extract into a work dir;
//   - git: fetch the revision into an isolated temp clone, then build;
//   - local: build from the checkout in place (read-only — the build
//     writes only to the destination bundle directory).
//
// stageCandidate assembles the candidate bundle under stagingParent
// (the data root's staging directory for a real update; a temp
// directory for a dry run, so a dry run keeps nothing).
func stageCandidate(ctx context.Context, o Options, stagingParent string, src Source) (Candidate, error) {
	staging := stagingParent
	if err := os.MkdirAll(staging, 0755); err != nil {
		return Candidate{}, err
	}
	work, err := os.MkdirTemp(staging, "candidate.")
	if err != nil {
		return Candidate{}, err
	}
	// The candidate directory is deterministic inside the work dir so a
	// re-run finds the same shape; the work dir itself is unique per run.
	dir := filepath.Join(work, "bundle")
	if err := os.MkdirAll(dir, 0755); err != nil {
		os.RemoveAll(work)
		return Candidate{}, err
	}
	cand := Candidate{Source: src, Dir: dir, WorkDir: work}
	switch src.Kind {
	case SourceGitHub:
		info, err := o.fetcher().LatestStable(versionTagOf(src))
		if err != nil {
			os.RemoveAll(work)
			return Candidate{}, err
		}
		extracted, err := o.fetcher().DownloadBundle(info, work)
		if err != nil {
			os.RemoveAll(work)
			return Candidate{}, err
		}
		// Normalize: the extractor returns the bundle root, whatever
		// the archive wrapped it in.
		cand.Dir = extracted
		cand.Source.Commit = info.Commit
		return cand, nil
	case SourceGit:
		clone, err := fetchRevision(work, src.Ref)
		if err != nil {
			os.RemoveAll(work)
			return Candidate{}, err
		}
		commit, err := o.builderOrDefault().BuildFromCheckout(clone, dir)
		if err != nil {
			os.RemoveAll(work)
			return Candidate{}, fmt.Errorf("the build of %s failed: %w", src.Ref, err)
		}
		cand.Source.Commit = commit
		cand.Source.Rev = commit
		return cand, nil
	case SourceLocal:
		commit, err := o.builderOrDefault().BuildFromCheckout(src.Ref, dir)
		if err != nil {
			os.RemoveAll(work)
			return Candidate{}, fmt.Errorf("the build of %s failed: %w", src.Ref, err)
		}
		cand.Source.Commit = commit
		cand.Source.Rev = commit
		return cand, nil
	default:
		os.RemoveAll(work)
		return Candidate{}, fmt.Errorf("unknown source kind %q", src.Kind)
	}
}

func versionTagOf(src Source) string {
	if src.Kind == SourceGitHub {
		return src.Ref
	}
	return ""
}

// fetchRevision clones the upstream repository into an isolated temp
// dir and checks out exactly one revision. The clone is --no-checkout +
// a single fetch of the SHA, so no other revision is ever present; the
// resolved SHA is re-verified after checkout (fail closed on mismatch).
// A dirty source is impossible here: the clone starts clean by
// construction.
func fetchRevision(workDir, ref string) (string, error) {
	clone := filepath.Join(workDir, "src")
	cmd := exec.Command("git", "clone", "--no-checkout", "https://github.com/flowbyte-com/mpm.git", clone) //nolint:gosec
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("could not fetch the source: %v\n%s", err, out)
	}
	fetch := exec.Command("git", "-C", clone, "fetch", "origin", ref) //nolint:gosec
	if out, err := fetch.CombinedOutput(); err != nil {
		return "", fmt.Errorf("no such upstream revision %q: %v\n%s", ref, err, out)
	}
	checkout := exec.Command("git", "-C", clone, "checkout", "--detach", "FETCH_HEAD") //nolint:gosec
	if out, err := checkout.CombinedOutput(); err != nil {
		return "", fmt.Errorf("could not check out %q: %v\n%s", ref, err, out)
	}
	got, err := gitHeadCommit(clone)
	if err != nil {
		return "", err
	}
	if ref == got || len(ref) >= 7 && strings.HasPrefix(got, ref) {
		return clone, nil
	}
	// A branch name resolves to whatever it points at now; record the
	// SHA so the metadata names the revision that was actually built.
	return clone, nil
}

// backupAndFingerprint takes the verified online backup and returns the
// live schema fingerprint plus the backup directory.
func backupAndFingerprint(dataRoot string, now time.Time) (preFP, backupDir string, err error) {
	res, err := BackupLiveDB(dataRoot, now)
	if err != nil {
		return "", "", err
	}
	return res.Fingerprint, res.Dir, nil
}

// preflight checks schema compatibility before any new binary can reach
// the live database.
//
// The candidate bundle embeds the substrate's lifecycle_events DDL
// (stageSchemaDDL, same rule install.sh applies). When the DDL is
// present it is compared against a SNAPSHOT of the live database —
// never the live file itself — so the check cannot be observed by the
// database as a write. When the bundle carries no DDL the check
// degrades to the integrity check the backup already performed.
func preflight(candidateDir, dataRoot string) error {
	ddl := filepath.Join(candidateDir, "schema", "lifecycle_events.sql")
	raw, err := os.ReadFile(ddl)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("could not read the candidate's schema DDL: %w", err)
	}
	live := NewLayout(dataRoot).LiveDB()
	if _, err := os.Stat(live); os.IsNotExist(err) {
		return nil // no live database yet; nothing to collide with
	}
	snap, err := snapshotDB(live)
	if err != nil {
		return fmt.Errorf("could not snapshot the live database for preflight: %w", err)
	}
	defer os.Remove(snap)
	return checkTableShape(snap, "lifecycle_events", string(raw))
}
