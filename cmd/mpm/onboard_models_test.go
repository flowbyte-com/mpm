// onboard_models_test.go — stage 2: the guided model step.
//
// The safety surface of this stage is a four-way branch (is there work?
// is this a dry run? is there a terminal? did the operator approve?) that
// in a real terminal can only be driven one way. decideModelStep is pure
// precisely so the other three can be tested, and that is what most of
// this file does. The subprocess tests cover the rest: that a piped run
// writes nothing, and that a dry run prints a plan and changes nothing.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core/config"
)

// Fixture configurations used by the planning tests. They are local to
// this file rather than shared with onboard_cmds_test.go: these tests
// assert on the PLANNER's view of a config, which is a different question
// from what the subprocess report says about the same shape.
const (
	// planFullConfig has both roles configured and bound.
	planFullConfig = `{
	  "profiles": {
	    "memory": {"provider": "custom", "model": "some-model", "base_url": "http://127.0.0.1:9/v1"},
	    "embed":  {"provider": "openai-compatible", "model": "some-embed", "base_url": "http://127.0.0.1:9/v1"}
	  },
	  "components": {
	    "memory": "memory", "critic": "memory", "scheduler": "memory", "embedding": "embed"
	  }
	}`

	// planModelsSkippedConfig configures the required role and
	// explicitly declines the optional one.
	planModelsSkippedConfig = `{
	  "profiles": {"default": {"provider": "custom", "model": "some-model"}},
	  "components": {"embedding": "disabled"}
	}`
)

// readSourceFile reads a sibling source file by name. Used for the
// structural assertions, which catch regressions the behavioural tests
// cannot see (a new network call, a new vendor literal).
func readSourceFile(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(body)
}

// ---------------------------------------------------------------------------
// Gates
// ---------------------------------------------------------------------------

// TestOnboardModelGateIsOpen pins stage 2's one deliberate widening.
func TestOnboardModelGateIsOpen(t *testing.T) {
	var opts onboardOptions
	if !opts.mutateModelsAllowed() {
		t.Error("mutateModelsAllowed() = false; stage 2 opens exactly this gate")
	}
	if !opts.mutateAllowed() {
		t.Error("mutateAllowed() should be true now that a step may write")
	}
}

// TestOnboardHostGateIsCapableNotPermitted is the counterpart to
// TestOnboardModelGateIsOpen. Stage 3 opened the host gate, so this test
// changed on purpose.
//
// What it now asserts is the thing that actually protects the machine: an
// open gate means CAPABLE, and every path through it still demands a
// target the operator typed plus an approval they gave. The gates are
// permissive; the consent around them is not.
func TestOnboardHostGateIsCapableNotPermitted(t *testing.T) {
	var opts onboardOptions
	if !opts.mutateHostsAllowed() {
		t.Error("mutateHostsAllowed() = false; stage 3 implements the host step")
	}

	// No targets — which is what detection produces, always — means no
	// installation is even schedulable.
	if len(opts.hostTargets) != 0 {
		t.Errorf("a default invocation carries %d host targets; a target must "+
			"only ever come from the command line", len(opts.hostTargets))
	}
	for _, state := range hostStates() {
		d := decisionFor(t, state.Host,
			decideHostInstall([]hostTargetState{state}, opts, false, nil))
		if d.Proceed {
			t.Errorf("host %q is scheduled with no named target and no approval", state.Host)
		}
	}

	// And the model gate opening must never have opened this one by
	// implication — the two are separate methods for exactly that reason.
	modelOnly := onboardOptions{scope: onboardScopeModels}
	if !modelOnly.mutateModelsAllowed() {
		t.Error("--models no longer opens the model gate")
	}
	if _, err := parseOnboardArgs([]string{"--models"}); err != nil {
		t.Fatalf("parse --models: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Planning — read-only
// ---------------------------------------------------------------------------

func planFor(t *testing.T, body string) modelStepPlan {
	t.Helper()
	// ResolveEmbeddingConfig consults the environment as a fallback, so
	// an operator's shell would otherwise decide what these tests
	// measure. Neutralising it is what makes "fresh install" mean fresh.
	for _, k := range []string{
		"OLLAMA_ENDPOINT", "OLLAMA_MODEL", "OLLAMA_HOST",
		"OPENAI_ENDPOINT", "OPENAI_EMBEDDING_MODEL", "OPENAI_API_KEY",
		"OPENROUTER_ENDPOINT", "OPENROUTER_EMBEDDING_MODEL", "OPENROUTER_API_KEY",
		"OAI_COMPAT_ENDPOINT", "OAI_COMPAT_EMBEDDING_MODEL", "OAI_COMPAT_API_KEY",
	} {
		t.Setenv(k, "")
	}
	var cfg config.Config
	if body != "" {
		if err := json.Unmarshal([]byte(body), &cfg); err != nil {
			t.Fatalf("decode fixture config: %v", err)
		}
	}
	return planModelStep(&cfg)
}

// TestPlanModelStepFreshConfigIsReady covers the clean-install case:
// nothing is configured, so onboarding has something to offer.
func TestPlanModelStepFreshConfigIsReady(t *testing.T) {
	p := planFor(t, "")
	if !p.Ready {
		t.Fatalf("a fresh install should be ready for guided configuration; got %+v", p)
	}
	if p.Generative != modelStateMissing {
		t.Errorf("Generative = %q, want %q", p.Generative, modelStateMissing)
	}
	if p.Embedding != modelStateMissing {
		t.Errorf("Embedding = %q, want %q", p.Embedding, modelStateMissing)
	}
	if len(p.Actions) == 0 {
		t.Error("a ready plan must state what it would do")
	}
}

// TestPlanModelStepNeverOverwritesWorkingConfiguration is the
// idempotency rule from ONBOARD.md §8: a fully configured install
// produces a plan with nothing to do.
func TestPlanModelStepNeverOverwritesWorkingConfiguration(t *testing.T) {
	p := planFor(t, planFullConfig)
	if p.Ready {
		t.Errorf("a fully configured install must not be ready; got %+v", p)
	}
	if p.Generative != modelStateConfigured {
		t.Errorf("Generative = %q, want %q", p.Generative, modelStateConfigured)
	}
	if p.Embedding != modelStateConfigured {
		t.Errorf("Embedding = %q, want %q", p.Embedding, modelStateConfigured)
	}
	for _, a := range p.Actions {
		if strings.Contains(a, "collect a generative") {
			t.Errorf("a configured install must not offer to collect again: %q", a)
		}
	}
}

// TestPlanModelStepTreatsDisabledEmbeddingAsADecision is the subtle one.
// An embedding bound to "disabled" is a declined optional feature, not a
// gap. Re-offering it would train the operator to dismiss a prompt that
// always means "you already decided".
func TestPlanModelStepTreatsDisabledEmbeddingAsADecision(t *testing.T) {
	p := planFor(t, planModelsSkippedConfig)
	if p.Embedding != modelStateDisabled {
		t.Fatalf("Embedding = %q, want %q", p.Embedding, modelStateDisabled)
	}
	if p.Ready {
		t.Errorf("a deliberately disabled embedding must not make the step ready; got %+v", p)
	}
	if p.Generative != modelStateConfigured {
		t.Errorf("Generative = %q, want %q", p.Generative, modelStateConfigured)
	}
}

// TestPlanModelStepOffersInvalidEmbedding repairs a broken binding
// rather than treating it as absent — the two need different remedies.
func TestPlanModelStepOffersInvalidEmbedding(t *testing.T) {
	p := planFor(t, `{
	  "profiles": {"default": {"provider": "custom", "model": "some-model"}},
	  "components": {"embedding": "no-such-profile"}
	}`)
	if p.Embedding != modelStateInvalid {
		t.Fatalf("Embedding = %q, want %q", p.Embedding, modelStateInvalid)
	}
	if !p.Ready {
		t.Error("an embedding bound to a missing profile is a gap onboarding should offer to fix")
	}
}

// TestPlanModelStepEmbeddingNeverInheritsDefault pins ONBOARD.md §4.2.
// An unset embedding binding must NOT resolve through the generative
// default profile; if it did, "embedding is configured" would be reported
// for installs that have never configured one.
func TestPlanModelStepEmbeddingNeverInheritsDefault(t *testing.T) {
	p := planFor(t, `{
	  "profiles": {"default": {"provider": "custom", "model": "some-model", "base_url": "http://127.0.0.1:9/v1"}},
	  "components": {"memory": "default"}
	}`)
	if p.Embedding == modelStateConfigured {
		t.Errorf("embedding inherited the generative default profile; plan = %+v", p)
	}
}

// TestPlanModelStepNamesNoVendorOrModel keeps the guiding principle
// enforced on the plan text, which is the part an operator reads before
// approving a write.
func TestPlanModelStepNamesNoVendorOrModel(t *testing.T) {
	p := planFor(t, "")
	blob := strings.ToLower(strings.Join(p.Actions, " "))
	for _, banned := range []string{
		"we recommend", "recommended model", "suggested", "best model",
		"default model", "you should use", "we suggest",
	} {
		if strings.Contains(blob, banned) {
			t.Errorf("plan text contains prescriptive language %q: %v", banned, p.Actions)
		}
	}
}

// TestPlanModelStepSourceHasNoProviderCall is the structural half of the
// no-network rule. ONBOARD.md §4 restricts onboard to local reads; a
// regression that made a live call would not be visible in the pure
// tests above, only here.
func TestPlanModelStepSourceHasNoProviderCall(t *testing.T) {
	src := readSourceFile(t, "onboard_models.go")
	for _, banned := range []string{"exec.Command", "http.Get", "http.Post", "net.Dial"} {
		if strings.Contains(src, banned) {
			t.Errorf("onboard_models.go contains %q; onboard makes no live model calls", banned)
		}
	}
}

// ---------------------------------------------------------------------------
// Consent — the whole matrix
// ---------------------------------------------------------------------------

func readyPlan() modelStepPlan {
	return modelStepPlan{Generative: modelStateMissing, Embedding: modelStateMissing, Ready: true}
}

func settledPlan() modelStepPlan {
	return modelStepPlan{Generative: modelStateConfigured, Embedding: modelStateDisabled, Ready: false}
}

// TestDecideModelStepRefusesWhenThereIsNothingToDo.
func TestDecideModelStepRefusesWhenThereIsNothingToDo(t *testing.T) {
	opts := onboardOptions{scope: onboardScopeModels}
	d := decideModelStep(settledPlan(), opts, true, "y")
	if d.Proceed {
		t.Fatal("a settled install must not start the wizard even when approved")
	}
	if !strings.Contains(d.Reason, "already complete") {
		t.Errorf("reason should say why nothing happened, got %q", d.Reason)
	}
}

// TestDecideModelStepDryRunNeverProceeds sweeps every other axis.
func TestDecideModelStepDryRunNeverProceeds(t *testing.T) {
	cases := []struct {
		name      string
		assumeYes bool
		isTTY     bool
		answer    string
	}{
		{"tty approved", true, true, "y"},
		{"tty approved by prompt", false, true, "yes"},
		{"piped with yes", true, false, ""},
		{"piped, nothing typed", false, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := decideModelStep(readyPlan(), onboardOptions{scope: onboardScopeModels, dryRun: true}, c.isTTY, c.answer)
			if d.Proceed {
				t.Error("a dry run proceeded; --dry-run must be absolute")
			}
			if !strings.Contains(d.Reason, "dry run") {
				t.Errorf("reason should name the dry run, got %q", d.Reason)
			}
		})
	}
}

// TestDecideModelStepRefusesWithoutATerminal is the unattended case.
// Note that even --yes does not unlock it: the wizard's own credential
// prompts need a terminal, so claiming otherwise would produce a run
// that approves a mutation and then does nothing.
func TestDecideModelStepRefusesWithoutATerminal(t *testing.T) {
	for _, assumeYes := range []bool{false, true} {
		d := decideModelStep(readyPlan(), onboardOptions{scope: onboardScopeModels, assumeYes: assumeYes}, false, "y")
		if d.Proceed {
			t.Errorf("assumeYes=%v: a piped run proceeded; onboarding must not assume consent", assumeYes)
		}
		if !strings.Contains(d.Reason, "not a terminal") {
			t.Errorf("assumeYes=%v: reason should name the terminal requirement, got %q", assumeYes, d.Reason)
		}
	}
}

// TestDecideModelStepRequiresExplicitApproval is the rejected-approval
// matrix. Note the empty string: a bare Enter is NOT approval, which is
// deliberately stricter than confirmPrompt's default-yes.
func TestDecideModelStepRequiresExplicitApproval(t *testing.T) {
	for _, answer := range []string{"", " ", "\n", "n", "no", "yep", "yes please", "true", "1"} {
		d := decideModelStep(readyPlan(), onboardOptions{scope: onboardScopeModels}, true, answer)
		if d.Proceed {
			t.Errorf("answer %q was accepted as approval; only y/yes may approve", answer)
		}
	}
	for _, answer := range []string{"y", "Y", "yes", "YES", " yes "} {
		d := decideModelStep(readyPlan(), onboardOptions{scope: onboardScopeModels}, true, answer)
		if !d.Proceed {
			t.Errorf("answer %q was rejected as approval", answer)
		}
	}
}

// TestDecideModelStepAssumeYesSkipsThePrompt — but only on a terminal.
func TestDecideModelStepAssumeYesSkipsThePrompt(t *testing.T) {
	d := decideModelStep(readyPlan(), onboardOptions{scope: onboardScopeModels, assumeYes: true}, true, "")
	if !d.Proceed {
		t.Error("--yes on a terminal should proceed without a prompt")
	}
}

// ---------------------------------------------------------------------------
// Outcome accounting
// ---------------------------------------------------------------------------

// TestSummariseModelOutcomeNamesPartialCompletion is the "don't present
// partial completion as total success" rule.
func TestSummariseModelOutcomeNamesPartialCompletion(t *testing.T) {
	done, remaining := summariseModelOutcome(modelStepPlan{
		Generative: modelStateConfigured,
		Embedding:  modelStateMissing,
	})
	if len(done) != 1 || !strings.Contains(done[0], "generative") {
		t.Errorf("done = %v, want the generative profile", done)
	}
	if len(remaining) != 1 || !strings.Contains(remaining[0], "embedding") {
		t.Errorf("remaining = %v, want the embedding profile", remaining)
	}
}

// TestSummariseModelOutcomeCountsDisabledAsSettled — a declined optional
// feature is settled, not outstanding. Reporting it as outstanding would
// put a permanent "needs attention" on every install that chose not to
// use embeddings.
func TestSummariseModelOutcomeCountsDisabledAsSettled(t *testing.T) {
	done, remaining := summariseModelOutcome(modelStepPlan{
		Generative: modelStateConfigured,
		Embedding:  modelStateDisabled,
	})
	if len(remaining) != 0 {
		t.Errorf("remaining = %v, want none; disabled is a decision", remaining)
	}
	if len(done) != 2 {
		t.Errorf("done = %v, want both capabilities settled", done)
	}
}

// ---------------------------------------------------------------------------
// Subprocess: nothing writes without a terminal
// ---------------------------------------------------------------------------

// TestOnboardModelsScopeNeverWritesWithoutATerminal is the load-bearing
// safety test for stage 2. A test process has no terminal, so this
// exercises the unattended path end to end and proves the write gate
// holds through the real binary rather than only in the pure function.
func TestOnboardModelsScopeNeverWritesWithoutATerminal(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and runs subprocesses")
	}
	f := newOnboardFixture(t, "fresh")
	f.warmUp(t)
	before := digestOnboardTree(t, f)

	for _, args := range [][]string{
		{"onboard", "--models"},
		{"onboard", "--models", "--yes"},
		{"onboard", "--models", "-y", "--json"},
		{"onboard", "--yes"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if _, err := f.run(t, args...); err != nil {
				t.Logf("run %v: %v", args, err)
			}
			assertUnchanged(t, strings.Join(args, " "), before, digestOnboardTree(t, f))
		})
	}
}

// TestOnboardModelsWithoutATerminalHandsOverCommands pins the helpful
// half of the refusal: declining unattended is only useful if the
// operator is told what to run instead.
func TestOnboardModelsWithoutATerminalHandsOverCommands(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and runs a subprocess")
	}
	f := newOnboardFixture(t, "fresh")
	out, _ := f.run(t, "onboard", "--models")
	for _, want := range []string{
		"not a terminal",
		"mpm config profile add default",
		"mpm config component set memory default",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
}

// TestOnboardModelsDryRunPrintsThePlanBeforeApproval is ONBOARD.md §9:
// show the planned mutations before asking for approval.
func TestOnboardModelsDryRunPrintsThePlanBeforeApproval(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and runs a subprocess")
	}
	f := newOnboardFixture(t, "fresh")
	out, _ := f.run(t, "onboard", "--models", "--dry-run")
	for _, want := range []string{"Plan", "dry run", "nothing was changed"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "Apply these changes") {
		t.Errorf("a dry run prompted for approval\n---\n%s", out)
	}
}

// TestOnboardModelsJSONCarriesThePlanAndOutcome keeps the machine
// contract honest: a consumer can see what would change and what did.
func TestOnboardModelsJSONCarriesThePlanAndOutcome(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and runs a subprocess")
	}
	f := newOnboardFixture(t, "fresh")
	rep := f.report(t, "--models")
	if rep.ModelPlan == nil {
		t.Fatal("model_plan is missing; a models-scope JSON run must carry the plan")
	}
	if rep.ModelStep == nil {
		t.Fatal("model_step is missing; a models-scope JSON run must carry the outcome")
	}
	if rep.ModelStep.Applied {
		t.Error("model_step.applied = true on a piped run; nothing may write without a terminal")
	}
	if len(rep.Plan) == 0 {
		t.Error("plan is empty, but a fresh install has model configuration to do")
	}
	if len(rep.ModelStep.Remaining) == 0 {
		t.Error("remaining is empty on a fresh install, but nothing was configured")
	}
}

// TestOnboardModelsJSONKeepsHostInstallationUnavailable is the cross-scope
// honesty check: opening the model gate must not have opened the host
// gate by accident.
func TestOnboardModelsJSONKeepsHostInstallationUnavailable(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and runs a subprocess")
	}
	f := newOnboardFixture(t, "fresh")
	rep := f.report(t)
	joined := strings.Join(rep.NotAvailable, " | ")
	if !strings.Contains(joined, "host installation") {
		t.Errorf("full scope must still report host installation as unavailable; got %v", rep.NotAvailable)
	}
	if strings.Contains(joined, "model configuration: this build reports model state but cannot change it") {
		t.Errorf("model configuration is available in stage 2; the stale unavailability notice is still printed: %v",
			rep.NotAvailable)
	}
}

// TestOnboardModelsNeverInstallsAHost is the consent boundary from
// ONBOARD.md §6 in executable form: the presence of a host integration
// on the machine must not cause onboarding to write to it.
func TestOnboardModelsNeverInstallsAHost(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and runs a subprocess")
	}
	f := newOnboardFixture(t, "fresh")
	f.warmUp(t)

	// A host directory that looks installed, so detection has something
	// real to find. Nothing here should be touched.
	hostDir := filepath.Join(f.home, ".pi", "agent")
	if err := os.MkdirAll(hostDir, 0o755); err != nil {
		t.Fatalf("stage a host directory: %v", err)
	}
	marker := filepath.Join(hostDir, "AGENTS.md")
	if err := os.WriteFile(marker, []byte("# operator notes\n"), 0o644); err != nil {
		t.Fatalf("stage a host file: %v", err)
	}
	before := digestOnboardTree(t, f)

	for _, args := range [][]string{{"onboard", "--yes"}, {"onboard", "--models", "--yes"}} {
		if _, err := f.run(t, args...); err != nil {
			t.Logf("run %v: %v", args, err)
		}
		assertUnchanged(t, strings.Join(args, " "), before, digestOnboardTree(t, f))
	}

	body, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("host file disappeared: %v", err)
	}
	if string(body) != "# operator notes\n" {
		t.Errorf("host instruction file was modified by onboarding:\n%s", body)
	}
}

// TestOnboardHelpAndReportStillRunTheModelStepOnlyWhenAsked keeps the
// scopes separated: `report` is read-only and must not even build a
// model plan.
func TestOnboardReportScopeCarriesNoModelPlan(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and runs a subprocess")
	}
	f := newOnboardFixture(t, "fresh")
	rep := f.report(t, "report")
	if rep.ModelPlan != nil {
		t.Errorf("report scope built a model plan %+v; it asked for no mutation", rep.ModelPlan)
	}
	if rep.ModelStep != nil {
		t.Errorf("report scope ran the model step: %+v", rep.ModelStep)
	}
}
