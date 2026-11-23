// opencode_factory_test.go — Continuity 001, PR 4: the REAL runtime
// factory.
//
// # WHY THIS FILE EXISTS
//
// The dispatcher's tests use a FAKE runtime, because the dispatcher's
// job is to make decisions and the real boundary has its own gate. But
// the WIRING — the factory that builds the two runtimes — is exactly
// where the decisions stop being the dispatcher's business: a factory
// that refuses to build a runtime, or that builds the wrong one, turns
// every decision into a refusal that no dispatcher test would notice.
//
// So this file drives the REAL factory, with a disposable credential and
// a stub binary, and checks the two properties that are structural:
//
//   - each role gets its OWN runtime, with its own model, and neither
//     gets the other's;
//   - the reviewer's sandbox tree EXISTS, because the runtime's
//     read-only commands run inside it.
//
// # NO INFERENCE HAPPENS
//
// The credential is disposable and the binary is a stub that exits
// immediately. Nothing is launched, no provider is contacted, and no
// model is asked anything.

package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core/assignment"
	oc "github.com/flowbyte-com/mpm-core/opencode"
)

// stubOpenCode writes an executable that does nothing.
//
// The factory RESOLVES the binary and constructs the runtime; it does
// not run it. The stub exists so the resolution succeeds on a host with
// no OpenCode installed.
func stubOpenCode(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestTheRealFactoryGivesEachRoleItsOwnSurface proves the two roles do
// not share a runtime, a model or a sandbox.
//
// The worker's model is the SIDECAR's local id, which only means
// anything inside the worker's namespace with the sidecar's socket. The
// reviewer's sandbox has no provider and no sidecar, so it names a real
// provider/model. A factory that gave the reviewer the worker's id would
// ask the reviewer for a model its own configuration does not define.
func TestTheRealFactoryGivesEachRoleItsOwnSurface(t *testing.T) {
	t.Setenv("MPM_OPENCODE_BIN", stubOpenCode(t))
	t.Setenv("MPM_OPENCODE_SIDECAR_KEY", "disposable-not-a-credential")
	t.Setenv("MPM_WORKSPACE", t.TempDir())

	cfg := testConfig(t)
	cfg.Model = "mpm-sidecar/permitted-model"
	cfg.ReviewerModel = "provider/reviewer-model"

	factory, err := NewOpenCodeFactory(cfg)
	if err != nil {
		t.Fatalf("build the factory: %v", err)
	}
	defer factory.Close()

	implementer := &assignment.Assignment{
		ID: "at-implementer", RootID: "at-root", Role: assignment.RoleImplementer,
	}
	reviewer := &assignment.Assignment{
		ID: "at-reviewer", RootID: "at-root", Role: assignment.RoleReviewer,
	}
	workspace := filepath.Join(cfg.WorkspaceRoot, implementer.ID)
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}

	build := factory.Factory()
	ctx := context.Background()

	// THE IMPLEMENTER: the worker runtime, and NOTHING else.
	worker, reviewRuntime, err := build(ctx, implementer, workspace)
	if err != nil {
		t.Fatalf("build the worker runtime: %v", err)
	}
	if worker == nil {
		t.Fatal("the implementer got no worker runtime")
	}
	if reviewRuntime != nil {
		t.Error("the implementer got a REVIEW runtime; the two surfaces must not mix")
	}
	wr, ok := worker.(*oc.Runtime)
	if !ok {
		t.Fatalf("the worker runtime is a %T", worker)
	}
	if wr.Config().Model != cfg.Model {
		t.Errorf("the worker runs %q, want the sidecar model %q",
			wr.Config().Model, cfg.Model)
	}
	if len(wr.Config().AllowedAgents) != 1 ||
		wr.Config().AllowedAgents[0] != DefaultWorkerAgent {
		t.Errorf("the worker's allow-list is %v, want exactly %q",
			wr.Config().AllowedAgents, DefaultWorkerAgent)
	}

	// THE REVIEWER: the review runtime, and NOTHING else.
	worker, reviewRuntime, err = build(ctx, reviewer, "")
	if err != nil {
		t.Fatalf("build the reviewer runtime: %v", err)
	}
	if reviewRuntime == nil {
		t.Fatal("the reviewer got no review runtime")
	}
	if worker != nil {
		t.Error("the reviewer got a WORKER runtime; the two surfaces must not mix")
	}
	rr, ok := reviewRuntime.(*oc.Runtime)
	if !ok {
		t.Fatalf("the review runtime is a %T", reviewRuntime)
	}
	if rr.Config().Model != cfg.ReviewerModel {
		t.Errorf("the reviewer runs %q, want its own model %q",
			rr.Config().Model, cfg.ReviewerModel)
	}
	if rr.Config().Model == wr.Config().Model {
		t.Error("the reviewer and the worker run the SAME model")
	}
	if len(rr.Config().AllowedAgents) != 1 ||
		rr.Config().AllowedAgents[0] != DefaultReviewerAgent {
		t.Errorf("the reviewer's allow-list is %v, want exactly %q",
			rr.Config().AllowedAgents, DefaultReviewerAgent)
	}
	// THE REVIEWER'S SANDBOX TREE EXISTS.
	//
	// The runtime's read-only commands run with their working directory
	// at <root>/home. Without it, every inspection fails with "chdir …:
	// no such file or directory" and a review can only ever be
	// reported, never recovered.
	for _, name := range []string{"home", "ocfg", "odata", "ostate", "findings"} {
		path := filepath.Join(rr.Config().Root, name)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("the reviewer's sandbox tree has no %q: %v", name, err)
		}
	}
}

// TestTheRealFactoryGivesOneRuntimePerAssignment proves the factory is
// RE-ENTRANT per assignment.
//
// # WHY THIS MATTERS
//
// The SIDECAR takes an EXCLUSIVE lifetime lock on its socket path, and
// the socket path is per ASSIGNMENT: <mpmDir>/opencode/runs/<id>/
// sidecar.sock. A second sidecar for the same assignment in the same
// process is therefore refused by the sidecar's own lock — "another
// sidecar holds …; refusing to take it over" — even though the first
// one is THIS process's own.
//
// And the dispatcher asks for the same assignment more than once per
// pass: the recovery pass inspects it, and the dispatch path may then
// launch it. Without the cache the second request is refused, the
// recovery pass can never reconcile an implementer, and an implementer
// with an outstanding attempt is refused FOREVER — a refusal that
// creates no attempt, so the dispatch-attempt cap never trips.
//
// This test drives the REAL factory, so it fails without the cache and
// passes with it.
func TestTheRealFactoryGivesOneRuntimePerAssignment(t *testing.T) {
	t.Setenv("MPM_OPENCODE_BIN", stubOpenCode(t))
	t.Setenv("MPM_OPENCODE_SIDECAR_KEY", "disposable-not-a-credential")
	t.Setenv("MPM_WORKSPACE", t.TempDir())

	cfg := testConfig(t)
	factory, err := NewOpenCodeFactory(cfg)
	if err != nil {
		t.Fatalf("build the factory: %v", err)
	}
	defer factory.Close()

	implementer := &assignment.Assignment{
		ID: "at-once", RootID: "at-root", Role: assignment.RoleImplementer,
	}
	workspace := filepath.Join(cfg.WorkspaceRoot, implementer.ID)
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}

	build := factory.Factory()
	ctx := context.Background()

	first, _, err := build(ctx, implementer, workspace)
	if err != nil {
		t.Fatalf("the first runtime: %v", err)
	}
	second, _, err := build(ctx, implementer, workspace)
	if err != nil {
		t.Fatalf("the second runtime for the SAME assignment: %v", err)
	}
	if first != second {
		t.Error("the factory built a second runtime for the same assignment")
	}
}

// TestAClosedFactoryRefuses proves the closed factory says so.
//
// Its sidecars are gone and its runtime cache was released, so there is
// nothing to hand back. It refuses — a runtime whose sidecar no longer
// serves would fail later and further away, and a nil map would panic.
func TestAClosedFactoryRefuses(t *testing.T) {
	t.Setenv("MPM_OPENCODE_BIN", stubOpenCode(t))
	t.Setenv("MPM_OPENCODE_SIDECAR_KEY", "disposable-not-a-credential")
	t.Setenv("MPM_WORKSPACE", t.TempDir())

	factory, err := NewOpenCodeFactory(testConfig(t))
	if err != nil {
		t.Fatalf("build the factory: %v", err)
	}
	build := factory.Factory()
	factory.Close()

	_, _, err = build(context.Background(),
		&assignment.Assignment{ID: "at-closed", RootID: "at-root", Role: assignment.RoleImplementer},
		t.TempDir())
	if err == nil {
		t.Fatal("a closed factory handed out a runtime")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Errorf("the refusal does not say the factory is closed: %v", err)
	}
}

// TestTheRealFactoryRefusesAMissingCredential proves the credential is
// required, not defaulted.
//
// A dispatcher that ran without one would fail at the first launch —
// after it had already claimed the assignment — and the claim would be
// wasted on a failure the operator could have seen before the run
// started.
func TestTheRealFactoryRefusesAMissingCredential(t *testing.T) {
	t.Setenv("MPM_OPENCODE_BIN", stubOpenCode(t))
	t.Setenv("MPM_OPENCODE_SIDECAR_KEY", "")
	t.Setenv("DEEPSEEK_API_KEY", "")
	t.Setenv("MPM_WORKSPACE", t.TempDir())

	_, err := NewOpenCodeFactory(testConfig(t))
	if err == nil {
		t.Fatal("a factory with no credential was built")
	}
	if !strings.Contains(err.Error(), "credential") {
		t.Errorf("the refusal does not name the credential: %v", err)
	}
	// THE GENERAL PROVIDER KEY IS NOT A FALLBACK. A dispatcher that
	// picked it up would be running on a credential the operator did not
	// prepare for this boundary.
	t.Setenv("DEEPSEEK_API_KEY", "a-provider-key-that-must-not-be-used")
	_, err = NewOpenCodeFactory(testConfig(t))
	if err == nil {
		t.Fatal("the factory accepted the general provider key as a fallback")
	}
	if !strings.Contains(err.Error(), "MPM_OPENCODE_SIDECAR_KEY") {
		t.Errorf("the fallback refusal does not name the sidecar key: %v", err)
	}
}

// TestTheRealFactoryRefusesAWorkerWithNoWorkspace proves the
// containment holds at the factory.
//
// A worker runtime with no authorised workspace is a runtime that could
// run anywhere, so the factory refuses it. This is the refusal the
// dispatcher's recovery path would hit if it asked for a runtime without
// building the workspace first.
func TestTheRealFactoryRefusesAWorkerWithNoWorkspace(t *testing.T) {
	t.Setenv("MPM_OPENCODE_BIN", stubOpenCode(t))
	t.Setenv("MPM_OPENCODE_SIDECAR_KEY", "disposable-not-a-credential")
	t.Setenv("MPM_WORKSPACE", t.TempDir())

	factory, err := NewOpenCodeFactory(testConfig(t))
	if err != nil {
		t.Fatalf("build the factory: %v", err)
	}
	defer factory.Close()

	_, _, err = factory.Factory()(context.Background(),
		&assignment.Assignment{ID: "at-x", RootID: "at-root", Role: assignment.RoleImplementer}, "")
	if err == nil {
		t.Fatal("a worker runtime with no workspace was built")
	}
	if !strings.Contains(err.Error(), "workspace") {
		t.Errorf("the refusal does not name the workspace: %v", err)
	}
}
