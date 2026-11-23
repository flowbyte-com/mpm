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
	// EnvReviewerKey is the REVIEWER's dedicated provider credential.
	// It is separate from the worker's sidecar key and is read only by
	// the reviewer runtime. There is no fallback to any ambient provider
	// key.
	EnvReviewerKey = "MPM_OPENCODE_REVIEWER_KEY"
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
	cfg         Config
	mpmDir      string
	opencode    string
	mpmBin      string
	model       string
	key         string
	reviewerKey string
	upstream    string
	plugin      string
	sidecars    []*oc.Sidecar
	// runtimes holds the runtime built for each assignment, for the life
	// of the pass.
	//
	// # WHY THE FACTORY CACHES
	//
	// The SIDECAR takes an EXCLUSIVE lifetime lock on its socket path,
	// and the socket path is per ASSIGNMENT: <mpmDir>/opencode/runs/<id>/
	// sidecar.sock. So a second sidecar for the same assignment in the
	// same process is refused by the sidecar's own lock — "another
	// sidecar holds …; refusing to take it over" — even though the first
	// one is THIS process's own.
	//
	// And the dispatcher asks for the same assignment more than once per
	// pass: the recovery pass inspects it, and the dispatch path may
	// then launch it. Without the cache the second request would be
	// refused, the recovery pass could never reconcile an implementer,
	// and an implementer with an outstanding attempt would be refused
	// forever — a refusal that creates no attempt, so the
	// dispatch-attempt cap never trips.
	//
	// One runtime per assignment per pass is also what the sidecar was
	// designed for: the socket path is stable per assignment, and the
	// tree is stable per assignment, so a retry finds the same data
	// store and the same relay.
	runtimes map[string]*oc.Runtime
	// closed says the factory has been closed. A closed factory has no
	// sidecar and no runtime, and asking it for one would return a
	// runtime whose sidecar is gone — or write to a nil map and panic.
	// It refuses instead.
	closed bool
	// ctx is the sidecars' lifetime. It is cancelled by Close, so a
	// sidecar that is still serving stops when the pass that needed it
	// is over — the goroutine that serves it is bound to this context
	// rather than detached.
	ctx    context.Context
	cancel context.CancelFunc
	// serveErrs holds each sidecar's serve result. Every channel has a
	// buffer of one, so the goroutine that serves can always report
	// without waiting for a reader.
	serveErrs []chan error
}

// NewOpenCodeFactory reads the environment the runtimes need.
//
// The credential is read HERE and stays in this process: the sidecar
// holds it, and the worker never sees it.
func NewOpenCodeFactory(cfg Config) (*OpenCodeFactory, error) {
	mpmDir := config.GetMPMDir()
	// THE SANDBOX ROOTS ARE BUILT FROM THE MPM DIRECTORY, and the
	// containment resolves them against the filesystem. A relative MPM
	// directory would make every sandbox root relative — and the
	// containment refuses a relative root, so the refusal would arrive
	// after the claim instead of here.
	if !filepath.IsAbs(mpmDir) {
		return nil, fmt.Errorf(
			"the MPM directory %q is not an absolute path; the sandbox roots are built "+
				"from it and the containment requires absolute paths", mpmDir)
	}

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

	// THE SIDECAR KEY IS THE ONLY CREDENTIAL SOURCE.
	//
	// There is deliberately no fallback to the operator's general
	// provider key. A fallback would let a dispatcher run on a
	// credential the operator did not rotate for this boundary, and the
	// Gate 1 check could not tell the two apart. The sidecar is the
	// explicit boundary; the key it holds is named here.
	key := os.Getenv(EnvSidecarKey)
	if key == "" {
		return nil, fmt.Errorf(
			"no sidecar credential: set %s. The worker cannot be given a "+
				"provider key, so inference runs through the sidecar, and the sidecar "+
				"needs one.", EnvSidecarKey)
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

	ctx, cancel := context.WithCancel(context.Background())
	return &OpenCodeFactory{
		cfg:         cfg,
		mpmDir:      mpmDir,
		opencode:    resolved,
		mpmBin:      mpmBin,
		model:       model,
		key:         key,
		reviewerKey: os.Getenv(EnvReviewerKey),
		upstream:    upstream,
		plugin:      resolvePluginPath(mpmDir),
		runtimes:    map[string]*oc.Runtime{},
		ctx:         ctx,
		cancel:      cancel,
	}, nil
}

// Close stops the sidecars the factory opened.
//
// It cancels the factory's context FIRST, so a sidecar that is still
// serving is told to stop, and then it closes the sockets. The serve
// results are drained so a fault is not lost silently.
func (f *OpenCodeFactory) Close() {
	f.closed = true
	if f.cancel != nil {
		f.cancel()
	}
	for _, ch := range f.serveErrs {
		select {
		case <-ch:
		default:
		}
	}
	f.serveErrs = nil
	for _, s := range f.sidecars {
		s.Close()
	}
	f.sidecars = nil
	f.runtimes = nil
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
		if f.closed {
			// THE FACTORY IS CLOSED. Its sidecars are gone and its
			// runtime cache was released, so there is nothing to hand
			// back. It says so, rather than panicking on a nil map or
			// returning a runtime whose sidecar no longer serves.
			return nil, nil, fmt.Errorf("the runtime factory is closed")
		}
		if a.Role == assignment.RoleReviewer {
			// THE CACHED RUNTIME. The reviewer has no sidecar, so it
			// could be rebuilt — but the sandbox tree is per assignment
			// and rebuilding it would write the same files again for no
			// reason. One runtime per assignment, one sidecar per
			// assignment.
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

	// The sidecar serves for the life of the pass. The goroutine is
	// BOUND to the factory's context: cancelling it ends the serve, so
	// the goroutine cannot outlive the work it was opened for.
	ctx := f.ctx
	serveErr := make(chan error, 1)
	f.serveErrs = append(f.serveErrs, serveErr)
	go func() {
		select {
		case <-ctx.Done():
		case serveErr <- sidecar.Serve(ctx):
		}
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
		Timeout: time.Duration(f.cfg.RuntimeTimeoutSeconds) * time.Second,
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
	// THE REVIEWER'S SANDBOX TREE.
	//
	// The runtime's read-only commands run with their working directory
	// at <root>/home, and the review path's own capture reads the
	// session through exactly that command. So the tree has to EXIST:
	// without it every inspection fails with "chdir …: no such file or
	// directory", and a review that cannot be inspected can only ever be
	// reported, never recovered.
	if err := oc.BuildTree(oc.Spec{Root: root, Workspace: f.cfg.ReviewRepoPath}); err != nil {
		return nil, err
	}
	// THE REVIEWER'S MODEL IS ITS OWN.
	//
	// The worker's model is the sidecar's local id, which means nothing
	// outside the worker's namespace. The reviewer's sandbox has no
	// provider and no sidecar, so it names a real one.
	model := f.cfg.ReviewerModel
	if model == "" {
		model = DefaultReviewModel
	}
	// THE REVIEWER CREDENTIAL IS ITS OWN, AND IT IS EXPLICIT. No fallback
	// to MINIMAX_API_KEY, DEEPSEEK_API_KEY, the sidecar key, or the
	// operator's OpenCode auth store.
	if f.reviewerKey == "" {
		return nil, fmt.Errorf(
			"no reviewer credential: set %s. The reviewer has its own provider "+
				"boundary and does not use the worker's sidecar credential.",
			EnvReviewerKey)
	}
	providerEnv, err := ReviewerProviderEnv(model)
	if err != nil {
		return nil, err
	}
	return oc.NewRuntime(oc.Config{
		Root:           root,
		OpenCodeBin:    f.opencode,
		MPMBin:         "",
		Model:          model,
		RepoPath:       f.cfg.ReviewRepoPath,
		PluginPath:     f.plugin,
		AllowedAgents:  []string{DefaultReviewerAgent},
		Timeout:        time.Duration(f.cfg.RuntimeTimeoutSeconds) * time.Second,
		ProviderKey:    f.reviewerKey,
		ProviderKeyEnv: providerEnv,
	})
}

// reviewerProviderEnv maps the reviewer model to the ONE provider
// environment variable OpenCode v2.3.2 uses for it.
//
// The mapping is deliberately closed: a model from another provider is
// refused rather than tried with a key that belongs to a different
// boundary. The pilot's measured model is
// `minimax-coding-plan/MiniMax-M3.1-Flash`, which OpenCode exposes when
// `MINIMAX_API_KEY` is present.
func ReviewerProviderEnv(model string) (string, error) {
	// ONLY THE MEASURED MODEL IS MAPPED. OpenCode v2.3.2 was measured to
	// expose `minimax-coding-plan/MiniMax-M3.1-Flash` with
	// `MINIMAX_API_KEY`; the CN endpoint and every other model are
	// refused rather than guessed at with a key that may not apply.
	if model == DefaultReviewModel {
		return "MINIMAX_API_KEY", nil
	}
	return "", fmt.Errorf(
		"reviewer model %q is not the measured reviewer model %q, so it has "+
			"no dedicated reviewer credential mapping", model, DefaultReviewModel)
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
