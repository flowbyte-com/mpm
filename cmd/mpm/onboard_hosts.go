// onboard_hosts.go — stage 3 of `mpm onboard`: opt-in host installation.
//
// THE RULE THIS FILE EXISTS TO ENFORCE. Detection never installs.
//
// `agent_installation/scripts/reconcile_managed_blocks.py` carries the
// invariant that MPM must never opt a machine into a host it does not
// already use, because MPM is installed on any machine with any MPM
// integration — so "the mpm binary exists" is satisfied on hosts that
// have never heard of MPM, and a sweep keyed on it would defeat the
// gate. That guard is unchanged here. Onboarding sits on the other side
// of it and requires something the sweep can never have: the operator
// naming a host, on the command line, and approving the mutation.
//
// Nothing discovered by inspection flows into installation. The two are
// separate steps with an operator decision between them, and
// decideHostInstall — pure, like decideModelStep — is that decision.
//
// PRESERVATION. An adapter that is already current is left exactly as it
// is. A host whose managed section has DRIFTED is reported, not
// rewritten: repairing replaces something that exists and works, which
// is a different decision from filling a gap, so it carries its own
// flag.
//
// ROLLBACK IS NOT ATOMIC. `uninstall.sh` deliberately never touches host
// instruction files, so undoing adapter 3 of 5 means re-running that
// adapter's installer with `--uninstall`, one at a time (ONBOARD.md
// §6.7). There is no orchestrator and no compensation pass. The report
// says so rather than implying a single-command undo exists.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// onboardHostAdapters maps each host label to the adapter directory that
// installs it. Derived from the `adapter` field of HOST_INSTALL_TARGETS
// in agent_installation/scripts/check_installed_managed_blocks.py;
// TestOnboardHostAdaptersMatchPython parses that file and fails on drift,
// so this cannot quietly become a second source of truth.
var onboardHostAdapters = map[string]string{
	"claude_code": "mpm-claude-code",
	"openclaw":    "mpm-memory-openclaw",
	"opencode":    "mpm-opencode",
	"pi":          "mpm-pi",
}

// Host verdicts, as reported by the inspector. Re-declared as constants
// rather than imported so the comparison below reads as the policy it is.
const (
	hostVerdictPass   = "PASS"
	hostVerdictWarn   = "WARN"
	hostVerdictAbsent = "ABSENT"
	hostVerdictError  = "ERROR"
)

// hostTargetState is the inspected state of one host.
type hostTargetState struct {
	Host        string `json:"host"`
	Verdict     string `json:"verdict"`
	Detail      string `json:"detail"`
	Repair      string `json:"repair"`
	InstallPath string `json:"install_path"`
}

// hostTargetDecision is what onboarding will do with one named host.
type hostTargetDecision struct {
	Host    string `json:"host"`
	Proceed bool   `json:"proceed"`
	Reason  string `json:"reason"`
}

// hostInstallResult is the outcome for one host after the step ran.
type hostInstallResult struct {
	Host     string   `json:"host"`
	Adapter  string   `json:"adapter,omitempty"`
	Verdict  string   `json:"verdict"`
	Detail   string   `json:"detail,omitempty"`
	ExitCode int      `json:"exit_code"`
	Backups  []string `json:"backups,omitempty"`
	Output   string   `json:"output,omitempty"`
}

// hostStepOutcome records the whole step: what was named, what each host
// was decided to be, and what actually happened.
//
// Applied is per host, not global. Three hosts named and two installed
// is a partial completion, and reporting it as one success is the failure
// this structure exists to prevent.
type hostStepOutcome struct {
	Targets  []string             `json:"targets"`
	Decided  []hostTargetDecision `json:"decided,omitempty"`
	Results  []hostInstallResult  `json:"results,omitempty"`
	Applied  bool                 `json:"applied"`
	Rollback string               `json:"rollback,omitempty"`
}

// decideHostInstall applies the consent rules to the named targets.
//
// Pure, like decideModelStep: isTTY and answers are inputs, so the
// declined-approval branch is testable without a terminal.
//
// `answers` maps host label to the operator's reply. It is consulted
// only where a prompt would actually be shown, so a caller cannot
// manufacture consent for a host it never asked about.
func decideHostInstall(states []hostTargetState, opts onboardOptions, isTTY bool, answers map[string]string) []hostTargetDecision {
	out := make([]hostTargetDecision, 0, len(states))
	for _, s := range states {
		d := hostTargetDecision{Host: s.Host, Reason: s.Detail}
		switch {
		case s.Verdict == hostVerdictPass:
			// Preserve. ONBOARD.md §6.4: a working integration is left
			// alone even when a newer section is available upstream.
			d.Reason = hostReasonAlreadyCurrent
		case s.Verdict == hostVerdictWarn && !opts.hostRepair:
			// Drift is reported; acting on it is a separate approval.
			d.Reason = hostReasonDrifted
		case opts.dryRun:
			d.Proceed = true
			d.Reason = hostReasonDryRun
		case !isTTY && !opts.assumeYes:
			// ONBOARD.md §6.3: unattended operation needs explicit
			// targets AND explicit mutation approval. Targets are here;
			// approval is not.
			d.Reason = "stdin is not a terminal and --yes was not given; " +
				"nothing was installed"
		case opts.assumeYes:
			d.Proceed = true
			d.Reason = "approved by --yes"
		default:
			answer, asked := answers[s.Host]
			switch {
			case !asked:
				// Not yet questioned. This pass runs BEFORE the prompt,
				// so a missing answer means "waiting", not "declined" —
				// conflating the two made promptsForHost report that
				// there was nothing to ask, and the interactive path
				// installed nothing at all.
				d.Reason = hostReasonAwaitingApproval
			case strings.ToLower(strings.TrimSpace(answer)) == "y",
				strings.ToLower(strings.TrimSpace(answer)) == "yes":
				d.Proceed = true
				d.Reason = "approved interactively"
			default:
				// Bare Enter is not approval, matching the model step.
				d.Reason = "declined; nothing was installed"
			}
		}
		out = append(out, d)
	}
	return out
}

// ---------------------------------------------------------------------------
// Execution
// ---------------------------------------------------------------------------

// runHostStep installs into operator-named hosts.
//
// It never derives a target from what it found. The only hosts it
// touches are the ones in opts.hostTargets, which the parser already
// refused to populate from a sweep word.
func runHostStep(opts onboardOptions, rep *onboardReport) {
	out := &hostStepOutcome{Targets: append([]string{}, opts.hostTargets...)}
	rep.HostStep = out

	results, err := inspectInstalledHosts()
	if err != nil {
		rep.Errors = append(rep.Errors,
			fmt.Sprintf("host inspection failed, so no host can be safely classified: %v", err))
		return
	}
	byHost := map[string]hostTargetState{}
	for _, r := range results {
		byHost[r.Host] = hostTargetState{
			Host: r.Host, Verdict: r.Verdict, Detail: r.Detail,
			Repair: r.Repair, InstallPath: r.InstallPath,
		}
	}
	// Iterate the NAMED order, not the inspector's order, so the report
	// reads in the order the operator typed.
	states := make([]hostTargetState, 0, len(opts.hostTargets))
	for _, t := range opts.hostTargets {
		s, ok := byHost[t]
		if !ok {
			rep.Errors = append(rep.Errors, fmt.Sprintf("host %q was not reported by the inspector", t))
			continue
		}
		states = append(states, s)
	}

	isTTY := isatty(os.Stdin)
	answers := map[string]string{}

	// Print the plan before any approval prompt. ONBOARD.md §9: show
	// planned mutations before asking for approval.
	if !opts.json {
		printHostInstallPlan(states, opts)
	}

	decided := decideHostInstall(states, opts, isTTY, answers)
	// The approval answers can only be gathered after the plan is shown,
	// and only for hosts that are actually going to be asked about.
	if needsHostAnswers(decided, opts, isTTY) {
		for _, d := range decided {
			if !promptsForHost(d, opts, isTTY) {
				continue
			}
			fmt.Printf("  Install the MPM integration into %s? [y/N]: ", displayHostName(d.Host))
			line, _ := readLineFromStdin()
			fmt.Println()
			answers[d.Host] = line
		}
		decided = decideHostInstall(states, opts, isTTY, answers)
	}
	out.Decided = decided

	adapterRoot, rootErr := locateAdapterRoot()
	if rootErr != nil && !opts.dryRun {
		for _, d := range decided {
			if !d.Proceed {
				continue
			}
			out.Results = append(out.Results, hostInstallResult{
				Host: d.Host, Verdict: "failed", ExitCode: -1,
				Detail: rootErr.Error(),
			})
		}
		rep.HostStep = out
		return
	}

	for _, d := range decided {
		rep.Plan = append(rep.Plan, hostPlanLine(states, d))
		if !d.Proceed {
			out.Results = append(out.Results, hostInstallResult{
				Host: d.Host, Verdict: "skipped", Detail: d.Reason,
				Backups: []string{},
			})
			continue
		}
		if opts.dryRun {
			// Proceed is true for a dry run precisely so the PLAN is
			// rendered; executing here would turn --dry-run into an
			// install, which is the one thing the flag exists to prevent.
			out.Results = append(out.Results, hostInstallResult{
				Host: d.Host, Verdict: "planned",
				Detail:  hostPlanLine(states, d),
				Backups: []string{},
			})
			continue
		}
		out.Applied = true
		res := installOneAdapter(adapterRoot, d.Host, byHost[d.Host].InstallPath)
		out.Results = append(out.Results, res)

		switch {
		case res.ExitCode != 0:
			rep.Errors = append(rep.Errors,
				fmt.Sprintf("host %q: installer exited %d", d.Host, res.ExitCode))
		default:
			rep.Skipped = append(rep.Skipped,
				fmt.Sprintf("host %q: installed; registration is not proof of runtime loading", d.Host))
		}
	}

	// The inventory rows and the "integration not installed" skip lines
	// were captured BEFORE anything was installed, so they describe a
	// machine that no longer exists. Refresh them rather than printing a
	// report that says "Pi: integration not installed" directly above
	// "Pi: installed" — two true statements about different moments, laid
	// out so the operator reads them as a contradiction.
	if out.Applied {
		touched := map[string]bool{}
		for _, d := range out.Decided {
			if d.Proceed {
				touched[displayHostName(d.Host)] = true
			}
		}
		if !refreshHostRows(rep, touched) {
			// An installer that exits 0 has told us only that IT
			// succeeded. Without a post-install inspection there is no
			// evidence the managed block exists, and saying so is the
			// difference between reporting an outcome and reporting an
			// exit code.
			rep.Attention = append(rep.Attention,
				"host installation ran, but the result could not be re-inspected; "+
					"the managed blocks were not confirmed present. Re-run `mpm onboard --hosts` "+
					"to see the current state")
		}
	}

	out.Rollback = hostRollbackNotice()
	rep.HostStep = out
}

// refreshHostRows re-inspects the hosts this run touched and rewrites
// their inventory rows and skip lines to describe the state onboarding
// left behind.
//
// Only rows for hosts in `touched` are rewritten. Everything else keeps
// the inspection it was reported with, so the refresh cannot quietly
// reclassify a host this run had no business changing.
//
// A re-inspection that fails leaves the report as it was and reports false,
// so the caller can say the installation is unconfirmed rather than
// silently keeping a pre-install row.
//
// Returns whether the refresh succeeded.
func refreshHostRows(rep *onboardReport, touched map[string]bool) bool {
	rows, err := inspectInstalledHosts()
	if err != nil {
		return false
	}
	rowIdx := map[string]int{}
	for i, r := range rep.Hosts {
		if touched[r.Subject] {
			rowIdx[r.Subject] = i
		}
	}
	for _, r := range rows {
		name := displayHostName(r.Host)
		i, ok := rowIdx[name]
		if !ok {
			continue
		}
		row := &rep.Hosts[i]
		row.Detail = fmt.Sprintf("%s · %s · %s", r.Verdict, r.File, r.InstallPath)
		row.Lines = nil
		if r.Detail != "" {
			row.Lines = append(row.Lines, r.Detail)
		}
		switch r.Verdict {
		case hostVerdictPass:
			row.State = onboardStateOperational
			row.Detail = "managed block present and current · " + r.InstallPath
			row.Remedy = ""
		case hostVerdictWarn, hostVerdictError:
			row.State = onboardStateAttention
			rep.Attention = append(rep.Attention, fmt.Sprintf(
				"host %q: installer completed, but the managed block needs attention (%s)",
				r.Host, r.Verdict))
		default: // still ABSENT — the installer did not take
			row.State = onboardStateSkipped
			row.Detail = "not installed"
			rep.Attention = append(rep.Attention, fmt.Sprintf(
				"host %q: installer exited 0 but the managed block is not confirmed present",
				r.Host))
		}
	}
	// Drop "X: integration not installed" only where the refresh shows
	// the block is present. If the install failed the line is still true,
	// and the error entry above already says why.
	present := map[string]bool{}
	for _, r := range rows {
		if r.Verdict == hostVerdictPass {
			present[displayHostName(r.Host)] = true
		}
	}
	kept := rep.Skipped[:0]
	for _, s := range rep.Skipped {
		drop := false
		for name := range present {
			if touched[name] && s == name+": integration not installed" {
				drop = true
				break
			}
		}
		if !drop {
			kept = append(kept, s)
		}
	}
	rep.Skipped = kept
	return true
}

// hostDecisionDeclinesReason is the set of reasons that mean "the rules
// already settled this; do not ask the operator about it". Prompting for
// approval to repair a host that is already current would be asking the
// operator to authorise something onboarding has decided not to do.
var hostDecisionDeclinesReason = map[string]bool{
	hostReasonAlreadyCurrent: true,
	hostReasonDrifted:        true,
	hostReasonDeclined:       true,
}

const (
	hostReasonAlreadyCurrent = "already current; left exactly as it is"
	hostReasonDrifted        = "managed section has drifted — reported, not repaired. " +
		"re-run with --repair to authorise rewriting it"
	hostReasonDeclined = "declined; nothing was installed"
	hostReasonDryRun   = "dry run: would install, but nothing is executed"

	// hostReasonAwaitingApproval marks a host the rules have not settled
	// and the operator has not yet been asked about. It is distinct from
	// declined because it means the question is still to come.
	hostReasonAwaitingApproval = "awaiting approval"
)

// promptsForHost reports whether a host would actually be asked about.
func promptsForHost(d hostTargetDecision, opts onboardOptions, isTTY bool) bool {
	if !isTTY || opts.assumeYes || opts.dryRun {
		return false
	}
	return !hostDecisionDeclinesReason[d.Reason]
}

// needsHostAnswers reports whether any host still needs a reply.
func needsHostAnswers(decided []hostTargetDecision, opts onboardOptions, isTTY bool) bool {
	for _, d := range decided {
		if promptsForHost(d, opts, isTTY) {
			return true
		}
	}
	return false
}

// installOneAdapter delegates to the adapter's own installer.
//
// Delegation is the whole point: onboarding adds no second payload path,
// no bespoke copy step and no new manifest format. The installer performs
// the backup (`.bak.<TS>`) that rollback depends on; onboarding only
// finds and reports the files it left behind.
func installOneAdapter(adapterRoot, host, installPath string) hostInstallResult {
	adapter, ok := onboardHostAdapters[host]
	res := hostInstallResult{Host: host, Adapter: adapter, Backups: []string{}}
	if !ok {
		res.Verdict = "failed"
		res.ExitCode = -1
		res.Detail = "no adapter is registered for this host"
		return res
	}
	dir := filepath.Join(adapterRoot, adapter)
	script := filepath.Join(dir, "install.sh")
	if _, err := os.Stat(script); err != nil {
		res.Verdict = "failed"
		res.ExitCode = -1
		res.Detail = fmt.Sprintf("adapter installer not found at %s (%v)", script, err)
		return res
	}

	cmd := exec.Command("bash", script)
	cmd.Dir = dir
	// The operator's real environment: an adapter integrates with the
	// host's actual configuration, so rewriting HOME here would install
	// into a fiction. Isolation is a TEST concern, not a runtime one.
	cmd.Env = os.Environ()
	output, runErr := cmd.CombinedOutput()
	res.Output = tailString(string(output), 40)
	res.ExitCode = 0
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			res.ExitCode = ee.ExitCode()
		} else {
			res.ExitCode = -1
			res.Detail = runErr.Error()
		}
	}

	if res.ExitCode == 0 {
		res.Verdict = hostVerdictPass
		res.Detail = "installer completed"
	} else {
		res.Verdict = "failed"
	}

	// Backups are the operator's only route back, so they are reported
	// per adapter rather than summarised. Scoped to the target file's
	// directory: that is where the adapters write them, and a wider scan
	// would pick up unrelated files and imply a rollback that does not
	// exist.
	res.Backups = findAdapterBackups(installPath)
	return res
}

// findAdapterBackups lists the `.bak.*` files an adapter leaves beside
// the file it rewrote.
func findAdapterBackups(path string) []string {
	if path == "" {
		return []string{}
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), "*.bak.*"))
	if err != nil {
		return []string{}
	}
	sort.Strings(matches)
	if matches == nil {
		return []string{}
	}
	return matches
}

// hostRollbackNotice is the per-adapter truth, stated once and repeated
// wherever an install happened.
func hostRollbackNotice() string {
	return "rollback is per adapter, not atomic: re-run each adapter's install.sh --uninstall, " +
		"one at a time. There is no orchestrator and no compensation pass, so a failure " +
		"part-way through leaves the earlier adapters installed."
}

// locateAdapterRoot finds agent_installation/ using the same resolution
// the host inspector uses, so inspection and installation can never
// disagree about which checkout they are talking about.
func locateAdapterRoot() (string, error) {
	dir, err := resolveRepoRelative("agent_installation")
	if err != nil {
		return "", fmt.Errorf("cannot locate agent_installation: %w", err)
	}
	if st, serr := os.Stat(dir); serr != nil || !st.IsDir() {
		return "", fmt.Errorf("adapter directory %s is not present in this build", dir)
	}
	return dir, nil
}

// printHostInstallPlan shows exactly what would be written, per host,
// before any approval is asked for.
func printHostInstallPlan(states []hostTargetState, opts onboardOptions) {
	if len(states) == 0 {
		return
	}
	fmt.Println()
	fmt.Println("Hosts named for installation")
	for _, s := range states {
		fmt.Printf("  · %s (%s) → %s\n", displayHostName(s.Host), s.Verdict, s.InstallPath)
	}
	fmt.Println("  Rollback, if needed:")
	fmt.Println("    " + hostRollbackNotice())
	fmt.Println()
}

func hostPlanLine(states []hostTargetState, d hostTargetDecision) string {
	var path string
	for _, s := range states {
		if s.Host == d.Host {
			path = s.InstallPath
		}
	}
	if !d.Proceed {
		return fmt.Sprintf("%s: %s", displayHostName(d.Host), d.Reason)
	}
	return fmt.Sprintf("run %s/install.sh, which rewrites %s", onboardHostAdapters[d.Host], path)
}

// tailString keeps the last n lines of installer output. A full adapter
// transcript is thousands of lines and would drown the report; the end
// of it is where failures say what they did.
func tailString(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// readLineFromStdin reads one approval reply.
//
// It reads a LINE, not a token: an operator answering "yes" followed by
// Enter is the normal case, and a token reader would leave the newline
// to be misread as the answer to the NEXT host's prompt. Reading the
// line whole is what stops one approval answering two questions.
func readLineFromStdin() (string, error) {
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(line), err
}
