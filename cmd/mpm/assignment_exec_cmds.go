// assignment_exec_cmds.go — the EXPLICIT invocation surface for PR 3.
//
// Two verbs live here, and neither is in the agent-facing tool registry:
//
//	mpm assignment execute --id ... --epoch ... --agent ... --workspace ...
//	mpm assignment execute-review --id ... --epoch ... --brief ... --reviewed-sha ...
//	mpm assignment capture-review-evidence --id ... --epoch ... ...
//
// # WHY THEY ARE CLI-ONLY
//
// `execute` starts a runtime process and spends real model budget. PR 4's
// dispatcher is the intended automatic path; PR 3 is explicitly invoked, so
// the door is the operator's shell.
//
// # WHAT MPM DOES NOT DO
//
// These verbs do not create namespaces or sandboxes. The runtime is an
// ordinary child process with the operator's environment, and the runtime's
// tool permissions and the deployment environment are where isolation, if
// any, lives.
//
// `capture-review-evidence` writes the reviewed revision and the findings
// digest. An agent-facing action for that would let an implementer
// manufacture the record its own work is judged by — the same rule that
// keeps `create-project`, `create-root` and `record-approval` off the
// registry.
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
	"github.com/flowbyte-com/mpm-core/dispatch"
	oc "github.com/flowbyte-com/mpm-core/opencode"
)

// Environment variables this surface reads.
const (
	// envOpenCodeBin names the runtime binary. Everything else — the
	// model, the agent definitions, the provider credentials and the tool
	// permissions — is the agent framework's own configuration.
	envOpenCodeBin = dispatch.EnvOpenCodeBin
)

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

	if err := dispatch.CheckContainmentRemoval(os.Getenv); err != nil {
		return nil, err
	}
	opencodeBin, err := resolveExecutable(envOr(envOpenCodeBin, "opencode"))
	if err != nil {
		return nil, err
	}
	workspaceID := f.workspaceID
	if workspaceID == "" {
		workspaceID = filepath.Base(filepath.Clean(f.workspace))
	}
	rt, err := oc.NewRuntime(oc.Config{
		Root:          filepath.Join(config.GetMPMDir(), "opencode", "runs", f.id),
		OpenCodeBin:   opencodeBin,
		Model:         f.model,
		AllowedAgents: []string{workerAgentDefinition},
	})
	if err != nil {
		return nil, err
	}
	res, err := store.Execute(context.Background(), rt, assignment.ExecuteParams{
		ID:        f.id,
		Epoch:     f.epoch,
		Actor:     assignmentActorFrom(f),
		Agent:     f.agent,
		Workspace: f.workspace,
		// The runtime runs IN the authorised workspace on this host, so
		// the directory it reports is the workspace itself.
		ExpectedRuntimeDir: f.workspace,
		WorkspaceID:        workspaceID,
		Model:              f.model,
		Title:              "assignment " + f.id,
		References:         f.references,
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

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
	workerAgentDefinition   = "mpm-worker"
	defaultReviewModel      = dispatch.DefaultReviewModel
	reviewerAgentDefinition = "mpm-reviewer"
)

// envOr reads an environment variable with a fallback.
func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
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

// assignmentVerbExecuteReview runs one INDEPENDENT REVIEW and records the
// evidence the HOST captured.
//
// The reviewer is an ordinary child process with the operator's
// environment and the configured reviewer definition. The exact-revision
// snapshot and the host-captured findings are MPM orchestration and are
// unchanged.
func assignmentVerbExecuteReview(store *assignment.Store, f *assignmentFlags) (interface{}, error) {
	if strings.TrimSpace(f.id) == "" {
		return nil, fmt.Errorf("execute-review needs --id (the REVIEWER assignment)")
	}
	if strings.TrimSpace(f.brief) == "" {
		return nil, fmt.Errorf("execute-review needs --brief")
	}
	if !filepath.IsAbs(f.workspace) {
		return nil, fmt.Errorf("the repository under review must be an absolute path, got %q", f.workspace)
	}
	// THE EXACT REVISION IS PART OF THE REVIEW. The dispatcher already
	// refuses to review without one; the explicit verb must not be a
	// weaker path that silently reviews whatever HEAD happens to be.
	if strings.TrimSpace(f.reviewedSHA) == "" {
		return nil, fmt.Errorf(
			"execute-review needs --reviewed-sha (the exact revision to review)")
	}

	if err := dispatch.CheckContainmentRemoval(os.Getenv); err != nil {
		return nil, err
	}
	opencodeBin, err := resolveExecutable(envOr(envOpenCodeBin, "opencode"))
	if err != nil {
		return nil, err
	}
	model := f.model
	if model == "" {
		model = defaultReviewModel
	}
	rt, err := oc.NewRuntime(oc.Config{
		Root:          filepath.Join(config.GetMPMDir(), "opencode", "reviews", f.id),
		OpenCodeBin:   opencodeBin,
		Model:         model,
		RepoPath:      f.workspace,
		AllowedAgents: []string{reviewerAgentDefinition},
	})
	if err != nil {
		return nil, err
	}
	return store.ExecuteReview(context.Background(), rt, assignment.ExecuteReviewParams{
		ID:               f.id,
		Epoch:            f.epoch,
		Actor:            assignmentActorFrom(f),
		Brief:            f.brief,
		Title:            "review " + f.id,
		ExpectedRevision: f.reviewedSHA,
		ExpectedModel:    model,
	})
}
