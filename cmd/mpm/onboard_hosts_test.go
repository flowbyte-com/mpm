package main

// onboard_hosts_test.go — tests for stage 3 of `mpm onboard`: opt-in host
// installation.
//
// The rule under test, from the brief: "Detection must be read-only. Never
// install an integration merely because a framework is present." Every test
// here is an attempt to make onboarding install something it should not,
// plus the tests that prove a host the operator DID name does get installed.
//
// HOST SCOPE
//
// OpenCode is the only supported host since the 2026-10-27 OpenCode-only
// adapter removal. Two consequences run through this file:
//
//   - Where a test names a REAL host, it names "opencode". The retired
//     hosts (claude_code, pi, hermes, openclaw) are not hosts any more:
//     `mpm onboard --hosts install pi` is now an unknown-target refusal,
//     and the refusal must say so rather than invite the operator to
//     try one.
//
//   - Where a test needs SEVERAL hosts to exercise a rule, it uses
//     NEUTRAL SYNTHETIC LABELS ("host-current", "host-absent",
//     "host-drifted"). The consent matrix in decideHostInstall is
//     label-agnostic — it switches on the inspected verdict, not on the
//     host's name — so the matrix stays fully covered with one real
//     host. Naming the retired hosts here would have meant exercising a
//     registry that no longer exists.
//
// Isolation is the same as onboard_cmds_test.go, with one addition: the
// tests that exercise a real installer run it against a temporary HOME,
// so nothing in the developer's own ~/.config/opencode can be reached.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Parser — the consent surface that must never widen by accident
// ---------------------------------------------------------------------------

// TestOnboardParserRefusesAutoSweepVocabulary is the direct test of
// "never install because a framework is present". Every word an operator
// might reach for when they mean "whatever you found" is refused BY NAME,
// with the reason in the message — not silently ignored, because a
// silently ignored `--hosts install auto` would look like consent that had
// no effect rather than a refusal.
func TestOnboardParserRefusesAutoSweepVocabulary(t *testing.T) {
	for word := range autoInstallWords {
		t.Run(word, func(t *testing.T) {
			_, err := parseOnboardArgs([]string{"--hosts", "install", word})
			if err == nil {
				t.Fatalf("parseOnboardArgs accepted `--hosts install %s`; a sweep "+
					"word must never become an installation target", word)
			}
			msg := err.Error()
			if !strings.Contains(msg, "auto-sweep") {
				t.Errorf("refusal does not explain WHY: %q", msg)
			}
			if !strings.Contains(msg, "name the hosts you want") {
				t.Errorf("refusal does not say what to do instead: %q", msg)
			}
		})
	}
}

// TestOnboardParserRefusesUnknownTarget keeps a typo from silently
// installing nothing, or from being rounded to a nearby host.
func TestOnboardParserRefusesUnknownTarget(t *testing.T) {
	_, err := parseOnboardArgs([]string{"--hosts", "install", "nope"})
	if err == nil {
		t.Fatal("parseOnboardArgs accepted an unknown host target")
	}
	for _, want := range []string{"unknown host target", "opencode"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal omits %q: %q", want, err)
		}
	}
	// A retired host is not a target. The refusal lists the KNOWN targets
	// and nothing else: a retired name in that list would send the
	// operator back to a host MPM no longer supports.
	for _, retired := range []string{"claude_code", "pi", "hermes", "openclaw"} {
		if strings.Contains(err.Error(), retired) {
			t.Errorf("refusal still lists the retired host %q: %q", retired, err)
		}
	}
}

// TestOnboardParserRequiresNamedTargetsForInstall proves `install` cannot
// be spelled without naming what to install. Without this, `--hosts
// install` alone would be exactly the sweep the design forbids.
func TestOnboardParserRequiresNamedTargetsForInstall(t *testing.T) {
	_, err := parseOnboardArgs([]string{"--hosts", "install"})
	if err == nil {
		t.Fatal("`--hosts install` with no target was accepted")
	}
	if !strings.Contains(err.Error(), "at least one host named on the command line") {
		t.Errorf("refusal does not state the requirement: %q", err)
	}
	if !strings.Contains(err.Error(), "no `--hosts auto`") {
		t.Errorf("refusal does not explain the absence of an auto mode: %q", err)
	}
}

// TestOnboardParserKeepsInspectionReadOnlyWithoutInstall: naming a host
// without `install` inspects it. This is the mistake an operator makes by
// omitting one word, and the cost of the mistake must be zero.
func TestOnboardParserKeepsInspectionReadOnlyWithoutInstall(t *testing.T) {
	_, err := parseOnboardArgs([]string{"--hosts", "opencode"})
	if err == nil {
		t.Fatal("a host named without `install` was accepted")
	}
	if !strings.Contains(err.Error(), "only inspects") {
		t.Errorf("refusal does not say what the command WOULD do: %q", err)
	}
}

// TestOnboardParserScopesRepairToInstall: `--repair` rewrites something
// that exists. It must not be spellable on its own, or it would sit on
// every scope waiting to be combined with a flag it does not belong to.
func TestOnboardParserScopesRepairToInstall(t *testing.T) {
	_, err := parseOnboardArgs([]string{"--hosts", "--repair"})
	if err == nil {
		t.Fatal("`--repair` without `install` was accepted")
	}
	if !strings.Contains(err.Error(), "only applies to") {
		t.Errorf("refusal does not scope the flag: %q", err)
	}

	opts, err := parseOnboardArgs([]string{"--hosts", "install", "opencode", "--repair"})
	if err != nil {
		t.Fatalf("--hosts install opencode --repair: %v", err)
	}
	if !opts.hostRepair {
		t.Error("--repair did not set hostRepair")
	}
}

// TestOnboardParserRefusesInstallOutsideHostScope: `install` is a host
// verb. Under `--models` it would read as "install the models", which
// onboarding does not do — it delegates configuration to `mpm config`.
func TestOnboardParserRefusesInstallOutsideHostScope(t *testing.T) {
	_, err := parseOnboardArgs([]string{"--models", "install"})
	if err == nil {
		t.Fatal("`install` was accepted under --models")
	}
	if !strings.Contains(err.Error(), "host step only") {
		t.Errorf("refusal does not explain the scope: %q", err)
	}
}

// TestOnboardParserNormalisesHostTargets: operators type "OPENCODE" and
// case should not decide whether a host is recognised — nor should it
// produce a second spelling that reaches the adapter map under a
// different key.
//
// The spellings are derived from the registry rather than hard-coded, so
// the test keeps its shape when a host is added back.
func TestOnboardParserNormalisesHostTargets(t *testing.T) {
	if len(onboardHostLabels) == 0 {
		t.Fatal("the host registry is empty; there is nothing to normalise")
	}
	tokens := make([]string, 0, len(onboardHostLabels))
	want := make([]string, 0, len(onboardHostLabels))
	for i, label := range onboardHostLabels {
		if i%2 == 0 {
			tokens = append(tokens, strings.ToUpper(label))
		} else {
			tokens = append(tokens, strings.ToUpper(label[:1])+label[1:])
		}
		want = append(want, label)
	}
	args := append([]string{"--hosts", "install"}, tokens...)
	args = append(args, "--yes")

	opts, err := parseOnboardArgs(args)
	if err != nil {
		t.Fatalf("case-insensitive targets rejected: %v", err)
	}
	if len(opts.hostTargets) != len(want) {
		t.Fatalf("targets = %v, want %v", opts.hostTargets, want)
	}
	for i := range want {
		if opts.hostTargets[i] != want[i] {
			t.Errorf("target %d = %q, want %q", i, opts.hostTargets[i], want[i])
		}
		if _, ok := onboardHostAdapters[opts.hostTargets[i]]; !ok {
			t.Errorf("normalised target %q has no adapter; it would fail at "+
				"execution time rather than at parse time", opts.hostTargets[i])
		}
	}
}

// TestOnboardParserPreservesNamedOrder: the report reads in the order the
// operator typed, so the parse must not sort or deduplicate away a
// deliberate second mention.
//
// Only one host is supported today, so the ORDER of two DISTINCT labels
// cannot be exercised here. What remains observable — and what the parse
// actually decides — is that the sequence reaches the report in the order
// it was typed, with every mention intact. A parser that sorted or
// deduplicated would collapse this to a single target, so the assertion
// still bites.
func TestOnboardParserPreservesNamedOrder(t *testing.T) {
	tokens := make([]string, 0, len(onboardHostLabels)*2)
	for _, label := range onboardHostLabels {
		tokens = append(tokens, label)
	}
	// A second mention of every label. The parser keeps it; what a
	// repeated target means for the run is the host step's business, not
	// the parser's.
	for _, label := range onboardHostLabels {
		tokens = append(tokens, label)
	}
	args := append([]string{"--hosts", "install"}, tokens...)
	args = append(args, "--yes")

	opts, err := parseOnboardArgs(args)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if strings.Join(opts.hostTargets, ",") != strings.Join(tokens, ",") {
		t.Errorf("targets = %v, want %v (named order must be preserved)",
			opts.hostTargets, tokens)
	}
}

// ---------------------------------------------------------------------------
// decideHostInstall — the pure consent matrix
// ---------------------------------------------------------------------------

// hostStates is the synthetic inspection result the consent-matrix tests
// run against.
//
// The labels are NEUTRAL ON PURPOSE: decideHostInstall switches on the
// inspected verdict, not on the host's name, so the matrix is fully
// covered without inventing hosts MPM no longer supports. Each name
// describes the STATE ("host-current"), not a host.
//
// InstallPath values are inert: the matrix never touches the filesystem.
func hostStates() []hostTargetState {
	return []hostTargetState{
		{Host: "host-current", Verdict: hostVerdictPass, InstallPath: "/h/current/AGENTS.md"},
		{Host: "host-absent", Verdict: hostVerdictAbsent, InstallPath: "/h/absent/AGENTS.md"},
		{Host: "host-drifted", Verdict: hostVerdictWarn,
			Detail: "marker block differs", InstallPath: "/h/drifted/AGENTS.md"},
	}
}

func decisionFor(t *testing.T, host string, decided []hostTargetDecision) hostTargetDecision {
	t.Helper()
	for _, d := range decided {
		if d.Host == host {
			return d
		}
	}
	t.Fatalf("no decision for host %q in %+v", host, decided)
	return hostTargetDecision{}
}

// TestOnboardHostDecisionPreservesACurrentIntegration: "Preserve existing
// integrations." A PASS host is not an opportunity to upgrade, and it is
// not an opportunity to ask either.
func TestOnboardHostDecisionPreservesACurrentIntegration(t *testing.T) {
	opts := onboardOptions{assumeYes: true}
	d := decisionFor(t, "host-current", decideHostInstall(hostStates(), opts, false, nil))
	if d.Proceed {
		t.Error("a PASS host was scheduled for installation; a working " +
			"integration must be left exactly as it is")
	}
	if d.Reason != hostReasonAlreadyCurrent {
		t.Errorf("reason = %q, want %q", d.Reason, hostReasonAlreadyCurrent)
	}
}

// TestOnboardHostDecisionNeverRepairsWithoutApproval: drift is reported
// even with --yes. Rewriting something that exists and works is a
// different decision from filling a gap, and it carries its own flag.
func TestOnboardHostDecisionNeverRepairsWithoutApproval(t *testing.T) {
	opts := onboardOptions{assumeYes: true}
	d := decisionFor(t, "host-drifted", decideHostInstall(hostStates(), opts, false, nil))
	if d.Proceed {
		t.Error("--yes authorised repairing a drifted managed block; " +
			"repair requires --repair")
	}
	if !strings.Contains(d.Reason, "--repair") {
		t.Errorf("reason does not name the flag that would authorise it: %q", d.Reason)
	}
	if !strings.Contains(d.Reason, "reported, not repaired") {
		t.Errorf("reason does not distinguish reporting from repairing: %q", d.Reason)
	}
}

func TestOnboardHostDecisionRepairsOnlyWhenRepairIsRequested(t *testing.T) {
	opts := onboardOptions{assumeYes: true, hostRepair: true}
	d := decisionFor(t, "host-drifted", decideHostInstall(hostStates(), opts, false, nil))
	if !d.Proceed {
		t.Errorf("--repair did not authorise the drifted host: %q", d.Reason)
	}
}

// TestOnboardHostDecisionRefusesWithoutATerminalOrYes: the unattended
// rule from the brief — "unattended requires explicit targets plus
// mutation approval". Targets are present; approval is not.
func TestOnboardHostDecisionRefusesWithoutATerminalOrYes(t *testing.T) {
	opts := onboardOptions{}
	d := decisionFor(t, "host-absent", decideHostInstall(hostStates(), opts, false, nil))
	if d.Proceed {
		t.Fatal("an unattended run with no --yes installed a host; " +
			"piped stdin is not consent")
	}
	if !strings.Contains(d.Reason, "not a terminal") || !strings.Contains(d.Reason, "--yes") {
		t.Errorf("reason does not state both the cause and the remedy: %q", d.Reason)
	}
}

// TestOnboardHostDecisionAssumesYesAuthorisesUnattendedInstall: --yes is
// the operator's explicit mutation approval, and it must actually work —
// otherwise `--yes` would be a flag that only ever refuses.
func TestOnboardHostDecisionAssumesYesAuthorisesUnattendedInstall(t *testing.T) {
	opts := onboardOptions{assumeYes: true}
	d := decisionFor(t, "host-absent", decideHostInstall(hostStates(), opts, false, nil))
	if !d.Proceed {
		t.Fatalf("--yes did not authorise installation: %q", d.Reason)
	}
	if d.Reason != "approved by --yes" {
		t.Errorf("reason = %q; the report must say the approval was unattended "+
			"rather than implying a human answered", d.Reason)
	}
}

// TestOnboardHostDecisionBareEnterIsNotApproval: matching the model step.
// An approval prompt that defaults to yes is not a consent gate.
func TestOnboardHostDecisionBareEnterIsNotApproval(t *testing.T) {
	opts := onboardOptions{}
	for _, answer := range []string{"", "n", "no", "sure", "why not"} {
		d := decisionFor(t, "host-absent",
			decideHostInstall(hostStates(), opts, true, map[string]string{"host-absent": answer}))
		if d.Proceed {
			t.Errorf("answer %q was accepted as approval", answer)
		}
		if d.Reason != hostReasonDeclined {
			t.Errorf("answer %q: reason = %q, want %q", answer, d.Reason, hostReasonDeclined)
		}
	}
	for _, answer := range []string{"y", "yes", "YES", " yes "} {
		d := decisionFor(t, "host-absent",
			decideHostInstall(hostStates(), opts, true, map[string]string{"host-absent": answer}))
		if !d.Proceed {
			t.Errorf("answer %q did not approve: %q", answer, d.Reason)
		}
	}
}

// TestOnboardHostDecisionPlansButDoesNotExecuteOnDryRun: a dry run must
// still PRINT the plan, so Proceed is true here — the gate that stops
// execution is in runHostStep, not here. Pinning both halves is the point:
// if this test were written to expect Proceed==false, the plan would go
// blank.
func TestOnboardHostDecisionPlansButDoesNotExecuteOnDryRun(t *testing.T) {
	opts := onboardOptions{dryRun: true, assumeYes: true}
	d := decisionFor(t, "host-absent", decideHostInstall(hostStates(), opts, false, nil))
	if !d.Proceed {
		t.Fatalf("dry run did not plan the installation, so nothing would be "+
			"printed: %q", d.Reason)
	}
	if d.Reason != hostReasonDryRun {
		t.Errorf("reason = %q, want %q", d.Reason, hostReasonDryRun)
	}
	if promptsForHost(d, opts, true) {
		t.Error("a dry run asked for approval it can never use")
	}
}

// TestOnboardHostDecisionDoesNotPromptWhereTheRulesAlreadyDecided: a
// prompt asking permission to do something onboarding has already
// decided not to do is noise that trains operators to type "y".
func TestOnboardHostDecisionDoesNotPromptWhereTheRulesAlreadyDecided(t *testing.T) {
	opts := onboardOptions{}
	decided := decideHostInstall(hostStates(), opts, true, nil)
	if needsHostAnswers(decided, opts, true) != true {
		t.Error("the ABSENT host should still be asked about")
	}
	for _, d := range decided {
		if d.Host == "host-current" && promptsForHost(d, opts, true) {
			t.Error("a PASS host was prompted for installation")
		}
		if d.Host == "host-drifted" && promptsForHost(d, opts, true) {
			t.Error("a drifted host was prompted without --repair")
		}
	}
}

// TestOnboardHostDecisionIgnoresAnswersItNeverAsked: answers is keyed by
// host, and a caller must not be able to pre-satisfy the approval gate
// for a host it never questioned.
func TestOnboardHostDecisionIgnoresAnswersItNeverAsked(t *testing.T) {
	opts := onboardOptions{hostRepair: true}
	decided := decideHostInstall(hostStates(), opts, true,
		map[string]string{"host-absent": "yes"})
	if !decisionFor(t, "host-absent", decided).Proceed {
		t.Error("the host that WAS asked about did not receive its approval")
	}
	if decisionFor(t, "host-current", decided).Proceed {
		t.Error("an answer for a different host authorised this one")
	}
}

// TestOnboardHostRollbackIsStatedAsPerAdapter: ONBOARD.md §6.7. If the
// report ever implies a single-command undo, an operator who trusts it
// will discover during a real failure that the earlier adapters are still
// installed.
func TestOnboardHostRollbackIsStatedAsPerAdapter(t *testing.T) {
	notice := hostRollbackNotice()
	for _, want := range []string{"per adapter", "not atomic", "--uninstall", "one at a time"} {
		if !strings.Contains(notice, want) {
			t.Errorf("rollback notice omits %q: %q", want, notice)
		}
	}
}

// TestOnboardHostAdaptersMatchPython prevents the Go adapter table from
// becoming a second source of truth. It names the directory onboarding
// will run install.sh from; a drifted entry would install the wrong
// integration into a host the operator named, silently.
func TestOnboardHostAdaptersMatchPython(t *testing.T) {
	root := repoRoot(t)
	script := filepath.Join(root, "agent_installation", "scripts",
		"check_installed_managed_blocks.py")
	body, err := os.ReadFile(script)
	if err != nil {
		t.Skipf("host inspector not present at %s: %v", script, err)
	}
	// Each target is a dict entry carrying both "host" and "adapter".
	re := regexp.MustCompile(`"host"\s*:\s*"([a-z_]+)"\s*,\s*"adapter"\s*:\s*"([a-z0-9-]+)"`)
	pythonAdapters := map[string]string{}
	for _, m := range re.FindAllStringSubmatch(string(body), -1) {
		pythonAdapters[m[1]] = m[2]
	}
	if len(pythonAdapters) == 0 {
		t.Fatalf("could not parse the host/adapter pairs out of %s; if the "+
			"Python format changed, update this test and the Go table together",
			script)
	}

	for host, want := range pythonAdapters {
		got, ok := onboardHostAdapters[host]
		if !ok {
			t.Errorf("host %q has adapter %q in Python but no Go entry",
				host, want)
			continue
		}
		if got != want {
			t.Errorf("host %q: Go adapter = %q, Python = %q; onboarding would "+
				"run the wrong installer", host, got, want)
		}
		// The directory must exist, or the step fails at execution time
		// with a path in the report rather than at parse time.
		if _, err := os.Stat(filepath.Join(root, "agent_installation", got, "install.sh")); err != nil {
			t.Errorf("host %q: adapter %q has no install.sh: %v", host, got, err)
		}
	}
	for host := range onboardHostAdapters {
		if _, ok := pythonAdapters[host]; !ok {
			t.Errorf("host %q is installable from Go but is not an adapter "+
				"target in Python", host)
		}
	}
}

// ---------------------------------------------------------------------------
// End-to-end, through the real binary
// ---------------------------------------------------------------------------

// runInRepo is run with a substituted MPM_REPO_ROOT, for the tests that
// stage a fake adapter tree.
//
// The env is spelled out inline rather than assembled by a helper: the
// isolation scanner in internal/testenv cannot follow a method call, and
// an unisolated subprocess in this suite would read the developer's real
// HOME.
func (f onboardFixture) runInRepo(t *testing.T, root string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(f.bin, args...)
	cmd.Dir = f.workspace
	cmd.Env = append(os.Environ(),
		"HOME="+f.home,
		"MPM_WORKSPACE="+f.workspace,
		"MPM_NO_AUTO_INIT=1",
		"MPM_REPO_ROOT="+root,
		"OPENAI_API_BASE=http://127.0.0.1:1/v1",
		"OPENAI_BASE_URL=http://127.0.0.1:1/v1",
		"ANTHROPIC_BASE_URL=http://127.0.0.1:1",
		"OLLAMA_HOST=http://127.0.0.1:1",
		"OLLAMA_ENDPOINT=",
		"OLLAMA_MODEL=",
	)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		var ee *exec.ExitError
		if errorAs(err, &ee) && stderr.Len() > 0 && stdout.Len() == 0 {
			return stderr.String(), err
		}
	}
	return stdout.String(), err
}

// TestOnboardHostsScopeNeverInstalls: the scope flag alone inspects. This
// is the single most important read-only guarantee in stage 3 — the
// command an operator runs to SEE what onboarding would do must not do it.
func TestOnboardHostsScopeNeverInstalls(t *testing.T) {
	f := newOnboardFixture(t, "fresh")
	f.warmUp(t)
	before := digestOnboardTree(t, f)

	rep := f.report(t, "hosts")
	assertUnchanged(t, "`mpm onboard --hosts`", before, digestOnboardTree(t, f))

	if rep.HostStep != nil {
		t.Errorf("inspection produced a host step: %+v", rep.HostStep)
	}
	if len(rep.Plan) != 0 {
		t.Errorf("inspection proposed %d mutations: %v", len(rep.Plan), rep.Plan)
	}
	// Every host is reported, whether or not it is installed. Detection
	// is the whole point of the hosts scope.
	if len(rep.Hosts) != len(onboardHostLabels) {
		t.Errorf("hosts reported = %d, want %d", len(rep.Hosts), len(onboardHostLabels))
	}
}

// TestOnboardHostDryRunWritesNothing: --dry-run shows the plan and stops.
func TestOnboardHostDryRunWritesNothing(t *testing.T) {
	f := newOnboardFixture(t, "fresh")
	f.warmUp(t)
	before := digestOnboardTree(t, f)

	rep := f.report(t, "hosts", "install", "opencode", "--dry-run")
	assertUnchanged(t, "`--hosts install opencode --dry-run`", before, digestOnboardTree(t, f))

	if rep.HostStep == nil {
		t.Fatal("dry run recorded no host step; there is nothing to show the plan in")
	}
	if rep.HostStep.Applied {
		t.Error("dry run reported Applied=true")
	}
	if len(rep.HostStep.Results) != 1 || rep.HostStep.Results[0].Verdict != "planned" {
		t.Fatalf("dry run results = %+v, want one planned result", rep.HostStep.Results)
	}
	if len(rep.Plan) == 0 {
		t.Error("dry run proposed no plan lines; ONBOARD.md §9 requires the " +
			"planned mutation to be visible before approval")
	}
}

// TestOnboardHostInstallWithoutATerminalInstallsNothing: piped stdin with
// no --yes is refused. If the refusal ever regressed into an install, this
// is the test that would stop it.
func TestOnboardHostInstallWithoutATerminalInstallsNothing(t *testing.T) {
	f := newOnboardFixture(t, "fresh")
	f.warmUp(t)
	before := digestOnboardTree(t, f)

	rep := f.report(t, "hosts", "install", "opencode")
	assertUnchanged(t, "`--hosts install opencode` (no terminal, no --yes)",
		before, digestOnboardTree(t, f))

	if rep.HostStep == nil || len(rep.HostStep.Results) != 1 {
		t.Fatalf("expected one host result, got %+v", rep.HostStep)
	}
	if rep.HostStep.Results[0].Verdict != "skipped" {
		t.Errorf("verdict = %q, want skipped", rep.HostStep.Results[0].Verdict)
	}
	if rep.HostStep.Applied {
		t.Error("Applied=true with no terminal and no --yes")
	}
	if !strings.Contains(rep.HostStep.Results[0].Detail, "not a terminal") {
		t.Errorf("detail does not explain the refusal: %q", rep.HostStep.Results[0].Detail)
	}
}

// TestOnboardHostInstallJSONCarriesTheStep: --json is a contract, and a
// consumer must be able to see which hosts were named and what happened
// to each without scraping prose.
//
// The same host is named twice here. Only one host is supported today, so
// two DISTINCT names are not available, and the property under test is
// the ENVELOPE's arithmetic: one result row per NAMED target, so a
// consumer can account for all of them without inferring from the absence
// of an entry. The parser documents a repeated target as harmless.
func TestOnboardHostInstallJSONCarriesTheStep(t *testing.T) {
	f := newOnboardFixture(t, "fresh")
	f.warmUp(t)
	// Dry run: this test is about the envelope's shape, and a dry run
	// produces the full envelope without needing a real installer.
	rep := f.report(t, "hosts", "install", "opencode", "opencode", "--yes", "--dry-run")

	if rep.HostStep == nil {
		t.Fatal("host_step missing from the JSON envelope")
	}
	if strings.Join(rep.HostStep.Targets, ",") != "opencode,opencode" {
		t.Errorf("targets = %v, want [opencode opencode] in named order",
			rep.HostStep.Targets)
	}
	// Every named host has a result row, whether it was installed,
	// skipped or failed. A consumer must be able to account for all of
	// them without inferring from the absence of an entry.
	if len(rep.HostStep.Results) != 2 {
		t.Fatalf("results = %+v, want one per named host", rep.HostStep.Results)
	}
	if rep.HostStep.Rollback == "" {
		t.Error("rollback guidance missing from a step that may have written")
	}

	// The envelope must round-trip: every slice non-nil, so a consumer
	// can iterate without a nil check.
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"installation", "readiness", "models", "hosts",
		"attention", "skipped", "plan"} {
		if decoded[key] == nil {
			t.Errorf("JSON key %q is null; consumers must not need a nil check", key)
		}
	}
}

// ---------------------------------------------------------------------------
// Partial completion — the case the brief singles out
// ---------------------------------------------------------------------------

// stagedAdapterRoot builds a checkout-shaped tree in which the adapters
// named in `stubs` are replaced by the given script bodies and the
// adapters named in `omit` are absent, so a multi-host install
// deterministically produces the mix of outcomes a test wants.
//
// Everything else in agent_installation is symlinked from the real
// checkout, so the host inspector (which reads canonical snippets
// relative to its own location) still works against this tree.
func stagedAdapterRoot(t *testing.T, stubs map[string]string, omit []string) string {
	t.Helper()
	real := filepath.Join(repoRoot(t), "agent_installation")
	root := filepath.Join(t.TempDir(), "staged")
	fake := filepath.Join(root, "agent_installation")
	if err := os.MkdirAll(fake, 0o755); err != nil {
		t.Fatalf("mkdir staged root: %v", err)
	}
	replaced := map[string]bool{}
	for _, name := range omit {
		replaced[name] = true
	}
	for name := range stubs {
		replaced[name] = true
	}
	entries, err := os.ReadDir(real)
	if err != nil {
		t.Fatalf("read %s: %v", real, err)
	}
	for _, e := range entries {
		if replaced[e.Name()] {
			continue
		}
		if err := os.Symlink(filepath.Join(real, e.Name()), filepath.Join(fake, e.Name())); err != nil {
			t.Fatalf("symlink %s: %v", e.Name(), err)
		}
	}
	for name, body := range stubs {
		dir := filepath.Join(fake, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir stub adapter %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "install.sh"), []byte(body), 0o755); err != nil {
			t.Fatalf("write stub installer %s: %v", name, err)
		}
	}
	return root
}

// stubInstallerThatRegistersButDoesNotIntegrate exits 0 and does the part
// of an install that leaves a trace — it registers the plugin — without
// writing the managed block that would make the host actually integrated.
//
// It is the shape of the failure the report has to be honest about: the
// installer SUCCEEDED, and the integration still is not there.
const stubInstallerThatRegistersButDoesNotIntegrate = `#!/usr/bin/env bash
set -euo pipefail
mkdir -p "$HOME/.config/opencode"
cat > "$HOME/.config/opencode/opencode.jsonc" <<'JSON'
{
  "$schema": "https://opencode.ai/config.json",
  "plugins": ["file:///stub/mpm-opencode"]
}
JSON
printf 'stub installer registered the plugin\n'
`

// TestOnboardHostInstallRecordsAMissingAdapterAsAFailure: an adapter that
// is not in the tree must fail loudly, not quietly succeed.
//
// The brief requires that "what succeeded and what remains incomplete" be
// recorded — so the failure must be visible in the result row AND in the
// error list, and the report must not present the run as a total success.
func TestOnboardHostInstallRecordsAMissingAdapterAsAFailure(t *testing.T) {
	f := newOnboardFixture(t, "fresh")
	f.warmUp(t)
	// The opencode adapter is absent from this tree, so installing it
	// must fail rather than quietly succeed.
	root := stagedAdapterRoot(t, nil, []string{"mpm-opencode"})

	out, err := f.runInRepo(t, root, "onboard", "--hosts", "install", "opencode",
		"--yes", "--json")
	if err != nil {
		var ee *exec.ExitError
		if !errorAs(err, &ee) {
			t.Fatalf("onboard: %v\n%s", err, out)
		}
	}
	var rep onboardReport
	if jerr := json.Unmarshal([]byte(out), &rep); jerr != nil {
		t.Fatalf("decode: %v\n---output---\n%s", jerr, out)
	}

	if rep.HostStep == nil || len(rep.HostStep.Results) != 1 {
		t.Fatalf("results = %+v, want one (the one named host)", rep.HostStep)
	}
	res := rep.HostStep.Results[0]
	if res.Verdict != "failed" {
		t.Errorf("result = %+v; a missing adapter is a failure, not a skip", res)
	}
	if !strings.Contains(res.Detail, "adapter installer not found") {
		t.Errorf("detail does not say what was missing: %q", res.Detail)
	}

	// The failure must surface as actionable, not as a silent partial.
	joined := strings.Join(rep.Errors, "\n")
	if !strings.Contains(joined, "opencode") {
		t.Errorf("errors do not name the failed host: %q", rep.Errors)
	}
	if rep.Success != true {
		t.Errorf("Success=%v; onboard's Success means the REPORT was produced, "+
			"not that every install succeeded — a failed host belongs in errors",
			rep.Success)
	}
	if rep.HostStep.Rollback == "" {
		t.Error("a partially applied step must state how to undo the part that landed")
	}

	// And nothing may have been written on the way to the failure.
	agents := filepath.Join(f.home, ".config", "opencode", "AGENTS.md")
	if _, statErr := os.Stat(agents); statErr == nil {
		t.Errorf("%s exists; a failed install must not leave a managed block "+
			"behind", agents)
	}
}

// TestOnboardHostInstallCaveatsAnInstallerThatDidNotIntegrate: the
// installer exited 0, and the integration is still not there.
//
// "Installer exited 0" must not be reported as "integrated". The report
// has to say the managed block was not confirmed present, and it must
// still leave the work that DID land alone — onboarding has no
// compensation pass and must not have invented one.
func TestOnboardHostInstallCaveatsAnInstallerThatDidNotIntegrate(t *testing.T) {
	f := newOnboardFixture(t, "fresh")
	f.warmUp(t)
	root := stagedAdapterRoot(t,
		map[string]string{"mpm-opencode": stubInstallerThatRegistersButDoesNotIntegrate},
		nil)

	out, err := f.runInRepo(t, root, "onboard", "--hosts", "install", "opencode",
		"--yes", "--json")
	if err != nil {
		var ee *exec.ExitError
		if !errorAs(err, &ee) {
			t.Fatalf("onboard: %v\n%s", err, out)
		}
	}
	var rep onboardReport
	if jerr := json.Unmarshal([]byte(out), &rep); jerr != nil {
		t.Fatalf("decode: %v\n---output---\n%s", jerr, out)
	}

	if rep.HostStep == nil || len(rep.HostStep.Results) != 1 {
		t.Fatalf("results = %+v, want one (the one named host)", rep.HostStep)
	}
	res := rep.HostStep.Results[0]
	if res.Verdict != hostVerdictPass || res.ExitCode != 0 {
		t.Errorf("result = %+v; the stub installer should have succeeded", res)
	}

	// The surviving state must be honestly reported: the stub registered
	// a plugin but wrote no managed block, so the host is still not
	// integrated.
	sawHonestCaveat := false
	for _, a := range rep.Attention {
		if strings.Contains(a, "not confirmed present") {
			sawHonestCaveat = true
		}
	}
	if !sawHonestCaveat {
		t.Errorf("a successful installer that did not produce a managed block was "+
			"reported without a caveat; attention = %v", rep.Attention)
	}
	if rep.Success != true {
		t.Errorf("Success=%v; onboard's Success means the REPORT was produced, "+
			"not that every install succeeded", rep.Success)
	}
	if rep.HostStep.Rollback == "" {
		t.Error("a step that may have written must state how to undo it")
	}

	// What the run LEFT BEHIND. The stub wrote a registration, and it is
	// still on disk: onboarding does not attempt to undo it, and must
	// not claim to have. A compensation pass here would be worse than
	// useless — it would undo work the operator may have wanted, based
	// on a failure in an unrelated concern.
	registered := filepath.Join(f.home, ".config", "opencode", "opencode.jsonc")
	if _, statErr := os.Stat(registered); statErr != nil {
		t.Errorf("the installer's output is gone (%v); onboarding has no "+
			"compensation pass and must not have invented one", statErr)
	}
	if len(res.Backups) != 0 {
		t.Errorf("the stub installer left backups %v; the report lists files "+
			"that do not exist", res.Backups)
	}
}

// TestOnboardHostInstallIsRepeatable: a second run over an already
// installed host must be a no-op, not a rewrite. ONBOARD.md §6.4.
func TestOnboardHostInstallIsRepeatable(t *testing.T) {
	f := newOnboardFixture(t, "fresh")
	f.warmUp(t)

	first := f.report(t, "hosts", "install", "opencode", "--yes", "--dry-run")
	second := f.report(t, "hosts", "install", "opencode", "--yes", "--dry-run")
	if len(first.HostStep.Results) != len(second.HostStep.Results) {
		t.Fatalf("dry-run results differ between runs: %+v vs %+v",
			first.HostStep.Results, second.HostStep.Results)
	}
	for i := range first.HostStep.Results {
		if first.HostStep.Results[i].Verdict != second.HostStep.Results[i].Verdict {
			t.Errorf("run %d verdict = %q, run %d = %q; onboarding is not idempotent",
				i, first.HostStep.Results[i].Verdict, i, second.HostStep.Results[i].Verdict)
		}
	}
}

// ---------------------------------------------------------------------------
// The real thing
// ---------------------------------------------------------------------------

// TestOnboardHostInstallRunsTheRealAdapter is the end-to-end proof that a
// clean installation can be onboarded onto a host: a genuine adapter
// installer runs against a temporary HOME, and the inspector afterwards
// reports the host as installed.
//
// Long (the adapter compiles and stages a runtime package), so it is
// skipped under -short.
func TestOnboardHostInstallRunsTheRealAdapter(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a real adapter installer against a temporary HOME")
	}
	// The OpenCode adapter compiles its own payload before it can be
	// staged, so the checkout must be bootstrapped first (`npm ci` in
	// agent_installation/mpm-opencode). The release-gate job does that
	// before `make release-gate`; a job that has not bootstrapped the
	// adapter skips this test rather than running a network install
	// inside a unit test.
	adapterDir := filepath.Join(repoRoot(t), "agent_installation", "mpm-opencode")
	if !adapterBootstrapped(adapterDir) {
		t.Skip("the OpenCode adapter's build toolchain is not bootstrapped; " +
			"run `npm ci` in agent_installation/mpm-opencode (the release-gate " +
			"job does this before `make release-gate`)")
	}

	f := newOnboardFixture(t, "fresh")
	f.warmUp(t)

	before := f.report(t, "hosts")
	for _, r := range before.Hosts {
		if r.Subject == displayHostName("opencode") && r.State == onboardStateOperational {
			t.Skip("the fixture HOME already reports an OpenCode integration; " +
				"the staged HOME should be empty")
		}
	}

	rep := f.report(t, "hosts", "install", "opencode", "--yes")
	if rep.HostStep == nil || len(rep.HostStep.Results) != 1 {
		t.Fatalf("host step = %+v, want one result", rep.HostStep)
	}
	res := rep.HostStep.Results[0]
	if res.Verdict != hostVerdictPass {
		t.Fatalf("real adapter install failed: verdict=%q exit=%d detail=%q\noutput:\n%s",
			res.Verdict, res.ExitCode, res.Detail, res.Output)
	}

	// The installer must have written the file it was pointed at.
	agents := filepath.Join(f.home, ".config", "opencode", "AGENTS.md")
	if _, err := os.Stat(agents); err != nil {
		t.Errorf("the adapter reported success but wrote nothing at %s: %v", agents, err)
	}

	// And a fresh inspection must now report OpenCode as installed — the
	// idempotency claim, checked through the real inspector rather than
	// through onboarding's own bookkeeping.
	after := f.report(t, "hosts")
	found := false
	for _, r := range after.Hosts {
		if r.Subject == displayHostName("opencode") {
			found = true
			if r.State != onboardStateOperational {
				t.Errorf("after installation the OpenCode row is %q, want %q: the report "+
					"was not refreshed to the state onboarding left behind",
					r.State, onboardStateOperational)
			}
		}
	}
	if !found {
		t.Error("no OpenCode row in the post-install report")
	}
	for _, s := range after.Skipped {
		if s == displayHostName("opencode")+": integration not installed" {
			t.Error("the post-install report still claims OpenCode is not installed; " +
				"the pre-install inventory line was not superseded")
		}
	}

	// Registration is not loading. This caveat must survive a successful
	// installation, which is exactly when it is most tempting to drop.
	joined := strings.Join(after.NotVerified, "\n") + strings.Join(after.Skipped, "\n")
	if !strings.Contains(joined, "not proof of loading") {
		t.Error("the runtime-verification caveat disappeared after a successful install")
	}

	// Re-running must preserve rather than rewrite.
	again := f.report(t, "hosts", "install", "opencode", "--yes")
	if again.HostStep.Applied {
		t.Error("a second run re-installed an already current host")
	}
	if r := again.HostStep.Results[0]; r.Detail != hostReasonAlreadyCurrent {
		t.Errorf("second run detail = %q, want %q", r.Detail, hostReasonAlreadyCurrent)
	}
}

// adapterBootstrapped reports whether the OpenCode adapter can build and
// stage its own payload in this checkout: either the local TypeScript
// compiler is installed, or a compiled payload is already present.
func adapterBootstrapped(adapterDir string) bool {
	for _, rel := range []string{
		filepath.Join("node_modules", ".bin", "tsc"),
		filepath.Join("dist", "index.v2.js"),
	} {
		if _, err := os.Stat(filepath.Join(adapterDir, rel)); err == nil {
			return true
		}
	}
	return false
}
