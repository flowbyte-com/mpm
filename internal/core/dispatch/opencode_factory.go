package dispatch

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/flowbyte-com/mpm-core/assignment"
	"github.com/flowbyte-com/mpm-core/config"
	oc "github.com/flowbyte-com/mpm-core/opencode"
)

// The environment the runtime needs.
//
// There is ONE: the binary to run. Everything else — the model, the agent
// definitions, the provider credentials and the tool permissions — is the
// agent framework's configuration, read by the framework itself from the
// operator's environment. MPM does not hold a provider credential and does
// not build a sandbox.
const (
	// EnvOpenCodeBin is the OpenCode binary the runtime drives.
	EnvOpenCodeBin = "MPM_OPENCODE_BIN"
)

// The default models.
const (
	// DefaultReviewModel is the reviewer's model.
	DefaultReviewModel = "minimax-coding-plan/MiniMax-M3.1-Flash"
)

// The runtime factory that drives the REAL OpenCode boundary.
//
// # WHY IT LIVES HERE AND NOT IN THE CLI
//
// The CLI and the scheduler integration are two doors into the SAME
// engine, and a factory that only one of them can build would mean two
// implementations of "how a runtime is constructed". So the factory
// lives with the engine, and both doors use it.
//
// # WHAT IT DOES NOT DO
//
// It does not create namespaces, sidecars, relays or credentials. The
// runtime it returns is an ordinary child process with the operator's
// environment, which is the deployment boundary the operator chose.

// OpenCodeFactory builds the runtimes the dispatcher drives.
type OpenCodeFactory struct {
	cfg      Config
	mpmDir   string
	opencode string
	// runtimes holds the runtime built for each assignment, for the life
	// of the pass. The per-assignment review state directory is stable, so
	// one runtime per assignment per pass is enough.
	runtimes map[string]*oc.Runtime
	// closed says the factory has been closed. A closed factory has no
	// runtime, and asking it for one refuses rather than writing to a nil
	// map.
	closed bool
}

// resolveExecutable finds a binary on PATH or at an absolute path.
func resolveExecutable(name string) (string, error) {
	if filepath.IsAbs(name) {
		if _, err := os.Stat(name); err != nil {
			return "", fmt.Errorf("the runtime binary %q is not executable: %w", name, err)
		}
		return name, nil
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("the runtime binary %q was not found on PATH: %w", name, err)
	}
	return p, nil
}

// NewOpenCodeFactory reads the environment the runtimes need.
func NewOpenCodeFactory(cfg Config) (*OpenCodeFactory, error) {
	if err := CheckContainmentRemoval(os.Getenv); err != nil {
		return nil, err
	}
	mpmDir := config.GetMPMDir()
	if !filepath.IsAbs(mpmDir) {
		return nil, fmt.Errorf(
			"the MPM directory %q is not an absolute path; the runtime state "+
				"directories are built from it", mpmDir)
	}

	opencodeBin := os.Getenv(EnvOpenCodeBin)
	if opencodeBin == "" {
		opencodeBin = "opencode"
	}
	resolved, err := resolveExecutable(opencodeBin)
	if err != nil {
		return nil, err
	}

	return &OpenCodeFactory{
		cfg:      cfg,
		mpmDir:   mpmDir,
		opencode: resolved,
		runtimes: map[string]*oc.Runtime{},
	}, nil
}

// Close releases the factory's runtimes.
func (f *OpenCodeFactory) Close() {
	f.closed = true
	f.runtimes = nil
}

// Factory returns the runtime factory the dispatcher drives.
//
// The factory is ROLE-SHAPED: a reviewer gets the review runtime and
// NOTHING else, and an implementer gets the worker runtime and nothing
// else. The reviewer never inherits the worker's agent definition or
// workspace; the ROLE, not the policy, decides that.
func (f *OpenCodeFactory) Factory() RuntimeFactory {
	return func(ctx context.Context, a *assignment.Assignment, workspace string) (
		assignment.Runtime, assignment.ReviewRuntime, error) {
		if f.closed {
			return nil, nil, fmt.Errorf("the runtime factory is closed")
		}
		if a.Role == assignment.RoleReviewer {
			if rt, ok := f.runtimes[a.ID]; ok {
				return nil, rt, nil
			}
			rt, err := f.reviewerRuntime(a)
			if err != nil {
				return nil, nil, err
			}
			f.runtimes[a.ID] = rt
			return nil, rt, nil
		}
		if rt, ok := f.runtimes[a.ID]; ok {
			return rt, nil, nil
		}
		rt, err := f.workerRuntime(a, workspace)
		if err != nil {
			return nil, nil, err
		}
		f.runtimes[a.ID] = rt
		return rt, nil, nil
	}
}

// workerRuntime builds the runtime an implementer runs under.
//
// The directory is stable per ASSIGNMENT — not per attempt — so a retry
// finds the same MPM state, which is where the read path looks.
func (f *OpenCodeFactory) workerRuntime(a *assignment.Assignment, workspace string) (
	*oc.Runtime, error) {

	if workspace == "" {
		return nil, fmt.Errorf("the worker runtime needs an authorised workspace")
	}
	root := filepath.Join(f.mpmDir, "opencode", "runs", a.ID)
	return oc.NewRuntime(oc.Config{
		Root:          root,
		OpenCodeBin:   f.opencode,
		Model:         f.cfg.Model,
		AllowedAgents: []string{DefaultWorkerAgent},
		Timeout:       time.Duration(f.cfg.RuntimeTimeoutSeconds) * time.Second,
	})
}

// reviewerRuntime builds the runtime a reviewer runs under.
//
// It is SEPARATE from the worker: the pinned reviewer definition, the
// operator's reviewer model and the repository under review. The
// exact-revision snapshot and the captured evidence are MPM orchestration,
// not containment, and are unchanged.
func (f *OpenCodeFactory) reviewerRuntime(a *assignment.Assignment) (*oc.Runtime, error) {
	if f.cfg.ReviewRepoPath == "" {
		return nil, fmt.Errorf("the reviewer runtime needs a repository to review")
	}
	model := f.cfg.ReviewerModel
	if model == "" {
		model = DefaultReviewModel
	}
	return oc.NewRuntime(oc.Config{
		Root:          filepath.Join(f.mpmDir, "opencode", "reviews", a.ID),
		OpenCodeBin:   f.opencode,
		Model:         model,
		RepoPath:      f.cfg.ReviewRepoPath,
		AllowedAgents: []string{DefaultReviewerAgent},
		Timeout:       time.Duration(f.cfg.RuntimeTimeoutSeconds) * time.Second,
	})
}

// The agent definitions.
//
// The worker is the file-edit-only definition; the reviewer is the pinned
// reviewer. They are different by construction, and the configuration
// refuses to name the same definition for both.
const (
	// DefaultWorkerAgent is the implementer's agent definition.
	DefaultWorkerAgent = "mpm-worker"
	// DefaultReviewerAgent is the reviewer's agent definition.
	DefaultReviewerAgent = "mpm-reviewer"
)
