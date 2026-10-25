// onboard_models.go — stage 2 of `mpm onboard`: the guided model step.
//
// WHAT THIS IS NOT. It is not a second configuration system. The
// existing `mpm config` wizard (handleConfigInteractive,
// handlers_config.go:747) already collects a generative profile, an
// embedding profile and the component bindings, already refuses
// embedding-only model IDs for generative roles, and already prompts for
// credentials with echo suppressed. Everything here decides WHETHER to
// run it, prints what it will do, obtains consent, and then delegates.
// If the wizard changes, onboarding changes with it for free.
//
// THE CONSENT MODEL. Four gates stand between an operator and a write,
// and all four must open:
//
//  1. something is actually missing — a configured install is a no-op,
//     not a reason to re-run the wizard;
//  2. the invocation is not --dry-run;
//  3. there is a terminal to approve on — a piped run refuses rather
//     than assuming consent, and prints the equivalent commands instead;
//  4. the operator approved, explicitly. A bare Enter does not approve.
//
// Gate 3 is why decideModelStep is a pure function: this four-way branch
// is the security-relevant part of the file, and a pure function is the
// only version of it that can be tested without a pty. In particular the
// REJECTED-APPROVAL case — which no real terminal can produce in a test
// suite — is directly reachable here.
//
// IDEMPOTENCY. Onboarding never overwrites a working configuration, and
// never re-asks about a decision the operator has already made: an
// embedding bound to "disabled" is a declined optional feature, not a
// gap. See modelStepPlan.Ready.
package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strings"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/config"
)

// ---------------------------------------------------------------------------
// Write gates
// ---------------------------------------------------------------------------

// mutateModelsAllowed reports whether the guided model step may write.
//
// Stage 2 opens exactly this gate and nothing else. It is a separate
// method from mutateHostsAllowed rather than one flag because the two
// steps have different consent surfaces: configuring models the operator
// just asked about is ordinary setup, whereas writing into a framework's
// own instruction file is not. Keeping them apart is what stops opening
// one from implicitly opening the other.
func (o onboardOptions) mutateModelsAllowed() bool { return true }

// mutateHostsAllowed reports whether the host step may write.
//
// Open as of stage 3, but "open" means CAPABLE, not PERMITTED: every
// path that writes still requires targets the operator named on the
// command line (parseOnboardArgs refuses the auto-sweep vocabulary) plus
// an approval the operator gave (decideHostInstall). Detection never
// writes, and a discovered host is skipped rather than installed.
func (o onboardOptions) mutateHostsAllowed() bool { return true }

// mutateAllowed reports whether this invocation may change anything.
func (o onboardOptions) mutateAllowed() bool {
	return o.mutateModelsAllowed() || o.mutateHostsAllowed()
}

// ---------------------------------------------------------------------------
// Planning — read-only
// ---------------------------------------------------------------------------

// Model step states.
const (
	modelStateConfigured = "configured"
	modelStateMissing    = "missing"
	modelStateInvalid    = "invalid"
	modelStateDisabled   = "disabled"
)

// modelStepPlan is what the guided model step WOULD do, derived purely
// from the on-disk configuration. It performs no writes, so it is safe
// to build in --dry-run and in every inspection mode.
type modelStepPlan struct {
	Generative string   `json:"generative"`
	Embedding  string   `json:"embedding"`
	Actions    []string `json:"actions"`

	// Ready is true only when there is something worth asking about.
	//
	// DISABLED is deliberately never Ready. components.embedding bound to
	// "disabled" is an operator declining an optional feature, which the
	// report already presents as a supported outcome with its
	// consequences spelled out. Re-offering the wizard on every run would
	// train the operator to dismiss a prompt that always means "you
	// already decided".
	Ready bool `json:"ready"`
}

// planModelStep derives the model plan from a loaded configuration.
//
// It reads resolution outcomes only — the same pure functions the report
// already renders from. It never opens an endpoint, because ONBOARD.md §4
// restricts onboard to structural facts: no live model calls, no model
// downloads, no paid API traffic.
func planModelStep(cfg *config.Config) modelStepPlan {
	p := modelStepPlan{Actions: []string{}}
	if cfg == nil {
		cfg = &config.Config{}
	}

	// Generative: ONBOARD.md §3 makes this REQUIRED. Absence is a real
	// functional loss, so absence is offerable.
	res := cfg.ResolveProfile("memory")
	switch {
	case res.ConfiguredName == "disabled":
		p.Generative = modelStateDisabled
	case res.Profile == nil:
		p.Generative = modelStateMissing
	case !res.Valid || res.Profile.Provider == "" || res.Profile.Model == "":
		p.Generative = modelStateInvalid
	default:
		p.Generative = modelStateConfigured
	}

	// Embedding: resolved through ResolveEmbeddingConfig, NEVER through
	// ResolveProfile — an unset embedding binding must not inherit
	// profiles.default (ONBOARD.md §4.2).
	emb := mpminternal.ResolveEmbeddingConfig(cfg)
	switch {
	case emb == nil, emb.Source == mpminternal.EmbeddingSourceAbsent:
		p.Embedding = modelStateMissing
	case emb.Source == mpminternal.EmbeddingSourceDisabled:
		p.Embedding = modelStateDisabled
	case emb.Status == mpminternal.EmbeddingStatusConfigured:
		// Includes the environment fallback: an endpoint and model
		// supplied through the environment is a working embedding
		// configuration, and onboarding does not tell an operator to
		// configure something that already works.
		p.Embedding = modelStateConfigured
	default:
		p.Embedding = modelStateInvalid
	}

	switch p.Generative {
	case modelStateMissing, modelStateInvalid:
		p.Actions = append(p.Actions,
			"collect a generative model profile — the wire protocol, the base URL, the model ID, "+
				"and a credential if the endpoint needs one")
	case modelStateDisabled:
		p.Actions = append(p.Actions,
			"generative is bound to \"disabled\" — recorded, not changed; "+
				"re-enable it with `mpm config component set memory <profile>`")
	}
	switch p.Embedding {
	case modelStateMissing, modelStateInvalid:
		p.Actions = append(p.Actions,
			"offer the optional embedding profile and bind components.embedding to it "+
				"(declining is a supported outcome)")
	case modelStateDisabled:
		p.Actions = append(p.Actions,
			"embedding is disabled — a declined optional feature, so it is left exactly as it is")
	}

	p.Ready = isOfferable(p.Generative) || isOfferable(p.Embedding)
	return p
}

// isOfferable reports whether a model-step state is a GAP worth offering
// to fill. Configured is done; disabled is a decision the operator has
// already made and recorded.
func isOfferable(state string) bool {
	return state == modelStateMissing || state == modelStateInvalid
}

// ---------------------------------------------------------------------------
// Consent — pure
// ---------------------------------------------------------------------------

// modelStepDecision is the outcome of the consent gates.
type modelStepDecision struct {
	Proceed bool   `json:"proceed"`
	Reason  string `json:"reason"`
}

// decideModelStep applies the four gates in a fixed order and returns
// why it would or would not proceed.
//
// It is pure: isTTY and answer are inputs rather than ambient reads, so
// the whole matrix — including the rejected-approval branch, which no
// real terminal can produce in a test — is directly testable.
//
// `answer` is consulted only where a prompt would actually be shown, so
// a caller that passes "y" without having prompted cannot accidentally
// satisfy the approval gate.
func decideModelStep(plan modelStepPlan, opts onboardOptions, isTTY bool, answer string) modelStepDecision {
	if !plan.Ready {
		return modelStepDecision{false,
			"model configuration is already complete; nothing would change, so the wizard was not started"}
	}
	if opts.dryRun {
		return modelStepDecision{false,
			"dry run: the plan above is exactly what would be written; nothing was changed"}
	}
	if !isTTY {
		// Not a refusal to be helpful — the wizard has the same
		// constraint (handlers_config.go:748-762), because credential
		// prompts need a terminal. Onboarding says so and hands over the
		// commands instead of pretending it could do better unattended.
		return modelStepDecision{false,
			"stdin is not a terminal, and onboarding does not prompt without one — nothing was changed. " +
				"Run `mpm config` from a terminal, or use the non-interactive commands printed below"}
	}
	if !opts.assumeYes {
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "y", "yes":
		default:
			return modelStepDecision{false,
				"declined: nothing was changed. Configure later with `mpm config`"}
		}
	}
	return modelStepDecision{true, "approved: delegating to the existing `mpm config` wizard"}
}

// nonInteractiveModelCommands are the equivalent scriptable commands,
// printed whenever the guided step cannot prompt. They are the same
// commands handleConfigInteractive names in its own non-interactive
// refusal, so an operator who follows either path lands in one place.
func nonInteractiveModelCommands() []string {
	return []string{
		"mpm config profile add default --model <model> --base-url <url>",
		"mpm config profile set default api_key            # hidden prompt (TTY)",
		`printf "$KEY" | mpm config profile set default api_key --stdin`,
		"mpm config component set memory default",
		"mpm config component set embedding <profile>      # optional",
	}
}

// ---------------------------------------------------------------------------
// Outcome
// ---------------------------------------------------------------------------

// modelStepOutcome records a guided model step: what was proposed, what
// was approved, and what actually landed.
//
// Applied is not Done. Applied means the wizard ran; Done lists the
// capabilities that resolve afterwards. A step that ran and left work
// outstanding is reported as incomplete, never as success — that is the
// "record what succeeded and what remains incomplete" rule from the
// brief, and it matters because the wizard's embedding step DEFAULTS to
// "leave unchanged", so an approved run routinely finishes with
// embedding still unconfigured. That is a success, but it is not the
// same success as "both configured", and the report must not conflate
// them.
type modelStepOutcome struct {
	Reason   string `json:"reason"`
	Applied  bool   `json:"applied"`
	ExitCode int    `json:"exit_code,omitempty"`

	// Saved is true when the configuration ON DISK changed as a result
	// of this step.
	//
	// Applied is not Saved. Applied means the wizard ran. The wizard
	// returns the same exit code whether it saved or the operator
	// declined the final "Save?" — so the exit code cannot answer the
	// question, and a report that implied it did would be wrong in the
	// exact case the operator most needs to know about: they said "no"
	// and need to know that nothing was committed.
	//
	// A run that re-saved byte-identical content is reported as
	// unchanged, because nothing about the operator's configuration
	// actually changed.
	Saved bool `json:"saved"`

	// Committed is true when the wizard COMMITTED the configuration —
	// it wrote the file — whether or not the bytes differ from what
	// was already on disk.
	//
	// Saved and Committed are different facts, and the report needs
	// both. A run that rewrites byte-identical content has committed
	// the configuration without changing it; reporting that as "nothing
	// was committed" contradicts the wizard's own "✓ Configuration
	// saved" line from moments earlier.
	Committed bool `json:"committed"`

	Done      []string       `json:"done,omitempty"`
	Remaining []string       `json:"remaining,omitempty"`
	After     *modelStepPlan `json:"after,omitempty"`

	// Attention, Skipped and Errors are the entries THIS STEP
	// contributed to the report.
	//
	// They are kept apart from the report's collected entries on
	// purpose. The collected entries describe the configuration as it
	// was FOUND; after the wizard has saved, the report re-collects
	// them so they describe the configuration as it IS. Without this
	// split, an after-state report would carry forward an attention
	// entry that the very step in question had just resolved.
	Attention []string `json:"attention,omitempty"`
	Skipped   []string `json:"skipped,omitempty"`
	Errors    []string `json:"errors,omitempty"`
}

// summariseModelOutcome splits a post-run plan into what landed and what
// did not. A capability that is DISABLED counts as settled: the operator
// decided, and onboarding does not reopen decisions.
func summariseModelOutcome(after modelStepPlan) (done, remaining []string) {
	done, remaining = []string{}, []string{}
	for _, chk := range []struct {
		label string
		state string
	}{
		{"generative model profile", after.Generative},
		{"embedding model profile", after.Embedding},
	} {
		if chk.state == modelStateConfigured || chk.state == modelStateDisabled {
			done = append(done, chk.label)
		} else {
			remaining = append(remaining, chk.label)
		}
	}
	return done, remaining
}

// ---------------------------------------------------------------------------
// Execution — delegates
// ---------------------------------------------------------------------------

// configSnapshot captures the configuration on disk at a point in time.
//
// The guided model step delegates to the `mpm config` wizard, and the
// wizard returns the same exit code whether it saved the configuration
// or the operator declined the final "Save?". The only honest way to
// tell the two apart is to compare what is on disk before and after.
type configSnapshot struct {
	exists bool
	data   []byte
}

// takeConfigSnapshot reads the configuration file.
//
// A configuration that cannot be read is recorded as absent: the
// comparison is about what changed, and an unreadable file is not a
// state the operator can have deliberately saved.
func takeConfigSnapshot() configSnapshot {
	path := config.ConfigPath()
	if path == "" {
		return configSnapshot{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return configSnapshot{}
	}
	return configSnapshot{exists: true, data: data}
}

// changed reports whether the configuration on disk differs from the
// snapshot s.
func (s configSnapshot) changed(now configSnapshot) bool {
	if s.exists != now.exists {
		return true
	}
	if !s.exists {
		return false
	}
	return !bytes.Equal(s.data, now.data)
}

// runModelStep runs the guided model step and records what it did.
//
// Every decision lives in decideModelStep; this is the I/O shell around
// it. It never writes configuration itself — the only write path is
// handleConfigInteractive, which is the same code `mpm config` runs.
func runModelStep(opts onboardOptions, rep *onboardReport) {
	cfg, err := config.LoadConfig()
	if err != nil {
		rep.Errors = append(rep.Errors,
			fmt.Sprintf("cannot read %s: %v", config.ConfigPath(), err))
		return
	}

	plan := planModelStep(cfg)
	rep.ModelPlan = &plan
	// The plan is recorded even when nothing proceeds. "Here is exactly
	// what onboarding would do" is the answer to the question the
	// operator asked, whether or not the answer is "nothing".
	rep.Plan = append(rep.Plan, plan.Actions...)

	isTTY := isatty(os.Stdin)
	answer := ""

	if plan.Ready && isTTY && !opts.assumeYes && !opts.dryRun {
		fmt.Println()
		fmt.Println("Planned changes:")
		for _, a := range plan.Actions {
			fmt.Printf("  · %s\n", a)
		}
		fmt.Printf("  · write %s\n", config.ConfigPath())
		fmt.Println()
		// Strict consent. confirmPrompt (handlers_config.go:1624)
		// defaults to YES on empty input, which is the wrong default for
		// a gate that writes the operator's model configuration.
		fmt.Print("  Apply these changes? [y/N]: ")
		answer, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		fmt.Println()
	}

	dec := decideModelStep(plan, opts, isTTY, answer)
	out := &modelStepOutcome{Reason: dec.Reason}
	rep.ModelStep = out

	// Report what is outstanding WHETHER OR NOT the step ran. "What is
	// still unconfigured" is true independently of whether onboarding
	// tried to change it, and an operator whose guided run was declined
	// for want of a terminal needs to know exactly what they are still
	// missing — not a step outcome that says only "declined".
	after := plan
	if dec.Proceed {
		// Delegation. The wizard prompts, validates and saves; onboarding
		// re-implements none of it.
		//
		// The snapshot is taken around the wizard so the outcome can say
		// whether anything was actually committed. "The wizard ran" and
		// "the configuration changed" are different facts, and only the
		// second one tells the operator whether their decline was
		// honoured.
		before := takeConfigSnapshot()
		out.Applied = true
		out.ExitCode, out.Committed = handleConfigInteractive(cfg)
		out.Saved = before.changed(takeConfigSnapshot())
		after = planModelStep(reloadConfig(rep))
	}
	out.After = &after
	out.Done, out.Remaining = summariseModelOutcome(after)

	if !dec.Proceed {
		entry := "model configuration: " + dec.Reason
		out.Skipped = append(out.Skipped, entry)
		rep.Skipped = append(rep.Skipped, entry)
		if !isTTY {
			// Hand over the scriptable path rather than just declining.
			// Naming `mpm config` here matters: an operator who reads only
			// this section must still be told where the working path is,
			// and "not available" without a destination reads as a
			// product limitation rather than a terminal limitation.
			rep.NotAvailable = append(rep.NotAvailable,
				"guided model configuration needs a terminal; run `mpm config` "+
					"from one, or use the non-interactive commands printed in the report")
		}
		return
	}

	if len(out.Remaining) > 0 {
		// Partial completion is named as such. Reporting this run as a
		// success because the wizard exited 0 would be the exact
		// "presents partial completion as total success" failure — and it
		// is the common case, because the wizard's embedding step
		// DEFAULTS to leaving it unchanged.
		entry := fmt.Sprintf(
			"model configuration is incomplete: %s still %s",
			strings.Join(out.Remaining, ", "),
			pluralVerb(len(out.Remaining), "does", "do"))
		out.Attention = append(out.Attention, entry)
		rep.Attention = append(rep.Attention, entry)
	} else {
		entry := "model configuration: complete after this run"
		out.Skipped = append(out.Skipped, entry)
		rep.Skipped = append(rep.Skipped, entry)
	}
	if out.ExitCode != 0 {
		entry := fmt.Sprintf("`mpm config` wizard exited %d", out.ExitCode)
		out.Errors = append(out.Errors, entry)
		rep.Errors = append(rep.Errors, entry)
	}
}

// reloadConfig re-reads configuration after the wizard ran, recording a
// read failure as a report error rather than panicking.
//
// The failure is recorded in BOTH the report and the step's own error
// list. The report is the pre-change one; after a run that saved, the
// merge replaces the report's collected errors with the step's — so an
// error recorded only in the report is dropped exactly when it matters
// most. The step's list is the one that survives.
func reloadConfig(rep *onboardReport) *config.Config {
	cfg, err := config.LoadConfig()
	if err != nil {
		entry := fmt.Sprintf("cannot re-read %s after configuration: %v", config.ConfigPath(), err)
		rep.Errors = append(rep.Errors, entry)
		if rep.ModelStep != nil {
			rep.ModelStep.Errors = append(rep.ModelStep.Errors, entry)
		}
		return &config.Config{}
	}
	return cfg
}

func pluralVerb(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
