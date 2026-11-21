package dispatch

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/flowbyte-com/mpm-core/assignment"
	"github.com/flowbyte-com/mpm-core/config"
	oc "github.com/flowbyte-com/mpm-core/opencode"
)

// The environment the real runtime needs.
//
// These are the ADAPTER's environment. The sidecar runs on the host,
// outside the worker's namespace, and the credential never enters the
// sandbox: it is read here, held by the sidecar, and never written to
// disk, never placed in argv and never passed into the namespace.
const (
	// EnvSidecarKey is the sidecar's credential.
	EnvSidecarKey = "MPM_OPENCODE_SIDECAR_KEY"
	// EnvSidecarUpstream is the provider's base URL.
	EnvSidecarUpstream = "MPM_OPENCODE_SIDECAR_UPSTREAM"
	// EnvOpenCodeBin is the OpenCode binary the runtime drives.
	EnvOpenCodeBin = "MPM_OPENCODE_BIN"
	// EnvUpstreamModel is the provider model the sidecar translates to.
	EnvUpstreamModel = "MPM_OPENCODE_SIDECAR_MODEL"
	// EnvOpenCodePlugin is where the MPM OpenCode plugin lives. The
	// REVIEWER's `sandbox_exec` comes from it.
	EnvOpenCodePlugin = "MPM_OPENCODE_PLUGIN"
	// EnvFallbackProviderKey is the fallback credential source: the
	// operator's own provider key. It stays in this process.
	EnvFallbackProviderKey = "DEEPSEEK_API_KEY"
)

// The default models.
//
// The worker asks for the STABLE local id ("permitted-model") and the
// sidecar translates it, so the worker's configuration never names a
// real provider model.
const (
	// DefaultSidecarModel is the worker's stable model id.
	DefaultSidecarModel = "permitted-model"
	// DefaultUpstreamModel is the provider model behind it.
	DefaultUpstreamModel = "deepseek-chat"
	// DefaultReviewModel is the reviewer's model.
	DefaultReviewModel = "minimax-coding-plan/MiniMax-M3.1-Flash"
	// DefaultSidecarUpstream is the provider's default base URL.
	DefaultSidecarUpstream = "https://api.deepseek.com"
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
// # WHAT IT REFUSES
//
// A missing credential is an ERROR, not a default. A dispatcher that
// ran without one would fail at the first launch — after it had already
// claimed the assignment — and the claim would be wasted on a failure
// the operator could have seen before the run started.

// OpenCodeFactory builds the runtimes the dispatcher drives.
//
// It holds the sidecars it opened so they can be closed when the pass
// finishes: a sidecar is a listening socket with a credential in its
// environment, and it must not outlive the work it was opened for.
type OpenCodeFactory struct {
	cfg      Config
	mpmDir   string
	opencode string
	mpmBin   string
	model    string
	key      string
	upstream string
	plugin   string
	sidecars []*oc.Sidecar
}

// NewOpenCodeFactory reads the environment the runtimes need.
//
// The credential is read HERE and stays in this process: the sidecar
// holds it, and the worker never sees it.
func NewOpenCodeFactory(cfg Config) (*OpenCodeFactory, error) {
	mpmDir := config.GetMPMDir()

	opencodeBin := os.Getenv(EnvOpenCodeBin)
	if opencodeBin == "" {
		opencodeBin = "opencode"
	}
	resolved, err := resolveExecutable(opencodeBin)
	if err != nil {
		return nil, err
	}
	mpmBin, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve the MPM binary: %w", err)
	}

	key := os.Getenv(EnvSidecarKey)
	if key == "" {
		key = os.Getenv(EnvFallbackProviderKey)
	}
	if key == "" {
		return nil, fmt.Errorf(
			"no sidecar credential: set %s (or %s). The worker cannot be given a "+
				"provider key, so inference runs through the sidecar, and the sidecar "+
				"needs one.", EnvSidecarKey, EnvFallbackProviderKey)
	}

	upstream := os.Getenv(EnvSidecarUpstream)
	if upstream == "" {
		upstream = DefaultSidecarUpstream
	}
	upstreamModel := os.Getenv(EnvUpstreamModel)
	if upstreamModel == "" {
		upstreamModel = DefaultUpstreamModel
	}
	model := cfg.Model
	if model == "" {
		model = "mpm-sidecar/" + DefaultSidecarModel
	}

	return &OpenCodeFactory{
		cfg:      cfg,
		mpmDir:   mpmDir,
		opencode: resolved,
		mpmBin:   mpmBin,
		model:    model,
		key:      key,
		upstream: upstream,
		plugin:   resolvePluginPath(mpmDir),
	}, nil
}

// Close closes the sidecars the factory opened.
func (f *OpenCodeFactory) Close() {
	for _, s := range f.sidecars {
		s.Close()
	}
	f.sidecars = nil
}

// Factory returns the runtime factory the dispatcher drives.
//
// The factory is ROLE-SHAPED: a reviewer gets the review runtime and
// NOTHING else, and an implementer gets the worker runtime and nothing
// else. The reviewer never inherits the worker's surface — not the
// worker's sandbox, not the worker's model, not the worker's agent
// definition.
func (f *OpenCodeFactory) Factory() RuntimeFactory {
	return func(ctx context.Context, a *assignment.Assignment, workspace string) (
		assignment.Runtime, assignment.ReviewRuntime, error) {
		if a.Role == assignment.RoleReviewer {
			rt, err := f.reviewerRuntime(a)
			if err != nil {
				return nil, nil, err
			}
			return nil, rt, nil
		}
		rt, err := f.workerRuntime(a, workspace)
		if err != nil {
			return nil, nil, err
		}
		return rt, nil, nil
	}
}

// workerRuntime builds the contained runtime an implementer runs under.
//
// The sandbox tree is stable per ASSIGNMENT — not per attempt — so a
// retry finds the same isolated data store, which is where the native
// session lives.
func (f *OpenCodeFactory) workerRuntime(a *assignment.Assignment, workspace string) (
	*oc.Runtime, error) {

	if workspace == "" {
		return nil, fmt.Errorf("the worker runtime needs an authorised workspace")
	}
	root := filepath.Join(f.mpmDir, "opencode", "runs", a.ID)
	spec := oc.Spec{Root: root, Workspace: workspace}
	if err := oc.BuildTree(spec); err != nil {
		return nil, err
	}

	sidecar, err := oc.NewSidecar(oc.SidecarConfig{
		SocketPath:      filepath.Join(root, "sidecar.sock"),
		UpstreamBaseURL: f.upstream,
		APIKey:          f.key,
		ModelAllowList: map[string]string{
			DefaultSidecarModel: f.upstreamModel(),
		},
		Audit: openAuditSink(root),
	})
	if err != nil {
		return nil, err
	}
	f.sidecars = append(f.sidecars, sidecar)

	go func() {
		// The sidecar serves for the life of the pass. A serve error
		// after a launch surfaces through the launch itself.
		_ = sidecar.Serve(context.Background())
	}()

	return oc.NewRuntime(oc.Config{
		Root:          root,
		OpenCodeBin:   f.opencode,
		MPMBin:        f.mpmBin,
		Model:         f.model,
		AllowedAgents: []string{DefaultWorkerAgent},
		Sidecar: oc.SidecarConfig{
			SocketPath: sidecar.SocketPath(),
		},
		Timeout: f.cfg.RuntimeTimeout,
	})
}

// reviewerRuntime builds the independent runtime a reviewer runs under.
//
// It is a SEPARATE sandbox with the pinned, default-deny reviewer
// definition and the plugin that provides `sandbox_exec`. The worker's
// containment is not involved: a review reads a repository, and it does
// it in its own room.
func (f *OpenCodeFactory) reviewerRuntime(a *assignment.Assignment) (*oc.Runtime, error) {
	if f.cfg.ReviewRepoPath == "" {
		return nil, fmt.Errorf("the reviewer runtime needs a repository to review")
	}
	root := filepath.Join(f.mpmDir, "opencode", "reviews", a.ID)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create the review root: %w", err)
	}
	model := f.cfg.Model
	if model == "" {
		model = DefaultReviewModel
	}
	return oc.NewRuntime(oc.Config{
		Root:          root,
		OpenCodeBin:   f.opencode,
		MPMBin:        "",
		Model:         model,
		RepoPath:      f.cfg.ReviewRepoPath,
		PluginPath:    f.plugin,
		AllowedAgents: []string{DefaultReviewerAgent},
		Timeout:       f.cfg.RuntimeTimeout,
	})
}

// upstreamModel is the provider model the sidecar translates to.
func (f *OpenCodeFactory) upstreamModel() string {
	model := os.Getenv(EnvUpstreamModel)
	if model == "" {
		return DefaultUpstreamModel
	}
	return model
}

// The agent definitions.
//
// The worker is the file-edit-only definition; the reviewer is the
// pinned, default-deny one. They are different by construction, and the
// configuration refuses to name the same definition for both.
const (
	// DefaultWorkerAgent is the implementer's agent definition.
	DefaultWorkerAgent = "mpm-worker"
	// DefaultReviewerAgent is the reviewer's agent definition.
	DefaultReviewerAgent = "mpm-reviewer"
)

// resolvePluginPath finds the MPM OpenCode plugin.
//
// The plugin is where the reviewer's `sandbox_exec` comes from, so a
// review cannot start without it.
func resolvePluginPath(mpmDir string) string {
	if p := os.Getenv(EnvOpenCodePlugin); p != "" {
		return p
	}
	p := filepath.Join(filepath.Dir(filepath.Dir(mpmDir)),
		"agent_installation", "mpm-opencode")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return filepath.Join(mpmDir, "agent_installation", "mpm-opencode")
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

// openAuditSink opens the sidecar's bounded audit log beside the
// sandbox.
//
// The log records request and response SIZES. It is not a cost meter,
// and nothing in MPM treats it as one.
func openAuditSink(root string) *os.File {
	path := filepath.Join(root, "sidecar-audit.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil
	}
	return f
}
