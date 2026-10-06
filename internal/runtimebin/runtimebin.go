// Package runtimebin resolves the `mpm` binary that MPM's own helper
// processes should execute.
//
// # Why this exists
//
// Several MPM binaries shell out to `mpm call ...` rather than linking
// against the core library: the critic runs hunts through the CLI so a
// finding is persisted by exactly the code path an operator would use,
// and the telemetry observer cross-joins the provenance DB the same way.
// Each of those call sites previously passed a bare "mpm" and let exec
// resolve it through PATH.
//
// A bare name is only safe if PATH is trustworthy. Under systemd a user
// unit's PATH is the manager's, not the operator's shell's, so "mpm"
// resolves to whichever `mpm` came first there — a stale developer
// build, an unrelated project, or nothing at all. The failure is silent:
// the helper appears to work and is writing into a different substrate.
//
// The scheduler already pins this via MPM_BIN (see
// internal/scheduler/mpm_bin.go). That resolver serves a process whose
// installed unit always sets the variable, so it can keep an ad-hoc
// bare-name branch for `mpm-scheduler` run by hand. The helpers here
// have no such guarantee, and they run unattended, so they get a
// fail-closed contract instead.
//
// # Precedence
//
//  1. An explicitly supplied path (ExecCLI.MPMPath, `mpm-telemetry
//     observe --mpm`). An operator who names a binary gets that binary.
//  2. MPM_BIN, the same variable the scheduler unit sets.
//  3. The sibling `mpm` beside the running executable. A non-standard
//     PREFIX install keeps every binary together, so this is a real
//     install layout rather than a convenience — and it is the only
//     step that can work without configuration.
//  4. Failure, with a message naming what was tried.
//
// PATH is never consulted. There is deliberately no final bare-"mpm"
// fallback: it would reintroduce the exact ambient-state dependency this
// package exists to remove, and it would make every misconfiguration
// here invisible rather than loud.
//
// missing evidence > false evidence
// holds here too — a helper that cannot find its binary should say so,
// not improvise one.

package runtimebin

import (
	"fmt"
	"os"
	"path/filepath"
)

// BinEnv is the environment variable that pins the `mpm` binary. Shared
// with the scheduler's unit template so one variable declares where every
// MPM subprocess binary lives.
const BinEnv = "MPM_BIN"

// BinName is the filename resolved within a sibling directory.
const BinName = "mpm"

// Resolver locates the mpm binary for a helper process.
type Resolver struct {
	// Explicit is a caller-supplied path that takes precedence over
	// everything else. Set from ExecCLI.MPMPath or --mpm. An empty
	// value means "not supplied", so an explicitly empty flag falls
	// through to the normal precedence rather than resolving to "".
	Explicit string

	// SelfPath is the path of the running executable, used for sibling
	// resolution. Defaults to os.Executable() when empty.
	SelfPath string

	// Env is the environment to read MPM_BIN from. Defaults to
	// os.Getenv when nil. Tests inject a map to avoid mutating the
	// process environment.
	Env []string
}

// Resolve returns an absolute path to a usable mpm binary.
//
// Every candidate must pass validation: it must be an explicit path,
// must exist, must be a regular file (not a directory), and must carry
// an execute bit. A configured candidate that fails validation is an
// error — it never falls through to the next step, because an operator
// who named a binary and got it wrong needs to find out rather than
// have a different binary silently substituted.
func (r *Resolver) Resolve() (string, error) {
	if r.Explicit != "" {
		return validate(r.Explicit, fmt.Sprintf("the explicitly configured --mpm/ExecCLI path"))
	}

	if env := r.getenv(); env != "" {
		return validate(env, fmt.Sprintf("%s", BinEnv))
	}

	if sibling, err := r.siblingCandidate(); err == nil {
		if _, verr := validate(sibling, "the sibling of the running binary"); verr == nil {
			return sibling, nil
		}
	}

	return "", fmt.Errorf(
		"cannot determine which %s binary to run: %s is unset, and no usable "+
			"%s was found beside this executable. Set %s to the installed binary, "+
			"pass --mpm explicitly, or reinstall so that %s and %s share a prefix. "+
			"Refusing to fall back to a PATH lookup — a PATH-ordered `mpm` may be a "+
			"different substrate entirely, and running it would hide the misconfiguration",
		BinName, BinEnv, BinName, BinEnv, BinName, BinName)
}

func (r *Resolver) getenv() string {
	if r.Env != nil {
		for _, kv := range r.Env {
			if len(kv) > len(BinEnv)+1 && kv[:len(BinEnv)+1] == BinEnv+"=" {
				return kv[len(BinEnv)+1:]
			}
		}
		return ""
	}
	return os.Getenv(BinEnv)
}

// siblingCandidate returns <dir-of-self>/mpm, or an error when the
// running executable's location cannot be determined.
func (r *Resolver) siblingCandidate() (string, error) {
	self := r.SelfPath
	if self == "" {
		var err error
		self, err = os.Executable()
		if err != nil {
			return "", err
		}
	}
	// Resolve symlinks so a binary reached through a symlinked prefix
	// still finds its real siblings.
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	return filepath.Join(filepath.Dir(self), BinName), nil
}

// validate checks that path is usable as an executable binary.
func validate(path, origin string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("%s is empty", origin)
	}
	if filepath.Base(path) == path {
		return "", fmt.Errorf("%s=%q is a bare name, not a path; PATH ordering would "+
			"decide which binary runs", origin, path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("%s=%q: %w", origin, path, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s=%q is a directory, not an executable", origin, path)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s=%q is not a regular file (mode %s)", origin, path, info.Mode())
	}
	if info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%s=%q is not executable (mode %s)", origin, path, info.Mode().Perm())
	}
	return path, nil
}

// Resolve is the package-level convenience for callers with no explicit
// preference.
func Resolve() (string, error) {
	return (&Resolver{}).Resolve()
}
