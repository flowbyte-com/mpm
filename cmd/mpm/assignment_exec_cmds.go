// assignment_exec_cmds.go — the EXPLICIT invocation surface for PR 3.
//
// Two verbs live here, and neither is in the agent-facing tool registry:
//
//	mpm assignment execute --id ... --epoch ... --agent ... --workspace ...
//	mpm assignment capture-review-evidence --id ... --epoch ... ...
//
// # WHY THEY ARE CLI-ONLY
//
// `execute` starts a contained runtime process and spends real model
// budget. PR 4's dispatcher is the intended automatic path; PR 3 is
// explicitly invoked, so the door is the operator's shell.
//
// `capture-review-evidence` writes the reviewed revision and the findings
// digest. An agent-facing action for that would let an implementer
// manufacture the record its own work is judged by — the same rule that
// keeps `create-root` and `record-approval` off the registry.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/flowbyte-com/mpm-core/assignment"
	"github.com/flowbyte-com/mpm-core/config"
	oc "github.com/flowbyte-com/mpm-core/opencode"
	"github.com/flowbyte-com/mpm-core/usererror"
)

// Environment variables the sidecar reads. They are the ADAPTER's
// environment — the sidecar runs on the host, outside the worker's
// namespace, and the credential never enters the sandbox.
const (
	envSidecarKey      = "MPM_OPENCODE_SIDECAR_KEY"
	envSidecarUpstream = "MPM_OPENCODE_SIDECAR_UPSTREAM"
	envOpenCodeBin     = "MPM_OPENCODE_BIN"
	// The UPSTREAM model id. The worker asks for the stable local id
	// ("permitted-model"); the sidecar translates it. That way the
	// worker's configuration never names a real provider model.
	envUpstreamModel = "MPM_OPENCODE_SIDECAR_MODEL"
	// The fallback credential source. It is the operator's own provider
	// key, and it stays in this process.
	envDeepSeekKey = "DEEPSEEK_API_KEY"
)

const defaultSidecarUpstream = "https://api.deepseek.com"

// assignmentVerbExecute runs one explicitly selected assignment.
func assignmentVerbExecute(store *assignment.Store, f *assignmentFlags) (interface{}, error) {
	if strings.TrimSpace(f.id) == "" {
		return nil, fmt.Errorf("execute needs --id")
	}
	if strings.TrimSpace(f.agent) == "" {
		return nil, fmt.Errorf("execute needs --agent (an allow-listed agent definition)")
	}
	if !filepath.IsAbs(f.workspace) {
		return nil, fmt.Errorf("the workspace must be an absolute path, got %q", f.workspace)
	}

	mpmDir := config.GetMPMDir()
	sandboxRoot := filepath.Join(mpmDir, "opencode", "runs", f.id)

	// The credential. It is read HERE, in the adapter, and handed to the
	// sidecar — which lives outside the sandbox. It is never written to
	// disk, never placed in argv, and never passed into the namespace.
	key := os.Getenv(envSidecarKey)
	if key == "" {
		key = os.Getenv(envDeepSeekKey)
	}
	if key == "" {
		return nil, fmt.Errorf(
			"no sidecar credential: set %s (or %s). The worker cannot be given a "+
				"provider key, so inference runs through the sidecar, and the sidecar "+
				"needs one.", envSidecarKey, envDeepSeekKey)
	}
	upstream := os.Getenv(envSidecarUpstream)
	if upstream == "" {
		upstream = defaultSidecarUpstream
	}
	upstreamModel := os.Getenv(envUpstreamModel)
	if upstreamModel == "" {
		upstreamModel = defaultUpstreamModel
	}

	opencodeBin := os.Getenv(envOpenCodeBin)
	mpmBin, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve the MPM binary: %w", err)
	}
	if opencodeBin == "" {
		opencodeBin = "opencode"
	}
	opencodeBin, err = resolveExecutable(opencodeBin)
	if err != nil {
		return nil, err
	}

	model := f.model
	if model == "" {
		model = "mpm-sidecar/" + defaultSidecarModel
	}

	workspaceID := f.workspaceID
	if workspaceID == "" {
		workspaceID = filepath.Base(filepath.Clean(f.workspace))
	}

	// The sandbox tree is built BEFORE the runtime, and it is stable per
	// ASSIGNMENT (not per attempt) so a retry finds the same isolated data
	// store — which is where the native session lives.
	spec := oc.Spec{Root: sandboxRoot, Workspace: f.workspace}
	if err := oc.BuildTree(spec); err != nil {
		return nil, err
	}

	sidecar, err := oc.NewSidecar(oc.SidecarConfig{
		SocketPath:      filepath.Join(sandboxRoot, "sidecar.sock"),
		UpstreamBaseURL: upstream,
		APIKey:          key,
		ModelAllowList: map[string]string{
			defaultSidecarModel: upstreamModel,
		},
		Audit: auditSink(sandboxRoot),
	})
	if err != nil {
		return nil, err
	}
	defer sidecar.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErr := make(chan error, 1)
	go func() { serveErr <- sidecar.Serve(ctx) }()

	rt, err := oc.NewRuntime(oc.Config{
		Root:          sandboxRoot,
		OpenCodeBin:   opencodeBin,
		MPMBin:        mpmBin,
		Model:         model,
		AllowedAgents: []string{workerAgentDefinition},
		Sidecar: oc.SidecarConfig{
			SocketPath: sidecar.SocketPath(),
		},
	})
	if err != nil {
		return nil, err
	}

	res, err := store.Execute(context.Background(), rt, assignment.ExecuteParams{
		ID:          f.id,
		Epoch:       f.epoch,
		Actor:       assignmentActorFrom(f),
		Agent:       f.agent,
		Workspace:   f.workspace,
		WorkspaceID: workspaceID,
		Model:       model,
		Title:       "assignment " + f.id,
		References:  f.references,
	})
	if err != nil {
		return nil, err
	}
	select {
	case serr := <-serveErr:
		if serr != nil {
			return nil, serr
		}
	default:
	}
	return res, nil
}

// assignmentVerbCaptureReviewEvidence records the reviewed revision and the
// findings digest a HOST captured.
func assignmentVerbCaptureReviewEvidence(store *assignment.Store, f *assignmentFlags) (interface{}, error) {
	return store.CaptureReviewEvidence(assignment.ReviewEvidenceParams{
		ReviewerID:     f.id,
		Epoch:          f.epoch,
		ReviewedSHA:    f.reviewedSHA,
		FindingsSHA256: f.findingsSHA256,
		EvidenceRef:    f.evidenceRef,
		Actor:          assignmentActorFrom(f),
	})
}

const (
	workerAgentDefinition = "mpm-worker"
	defaultSidecarModel   = "permitted-model"
	defaultUpstreamModel  = "deepseek-chat"
)

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

// auditSink opens the sidecar's bounded audit log beside the sandbox.
func auditSink(root string) *os.File {
	path := filepath.Join(root, "sidecar-audit.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		usererror.Error("mpm assignment: the sidecar audit log could not be opened: %v", err)
		return nil
	}
	return f
}
