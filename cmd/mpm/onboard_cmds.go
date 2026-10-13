// Package main — onboard_cmds.go
//
// `mpm onboard` — guided setup and verification of a working MPM
// installation. Implements docs/ONBOARD.md.
//
//   mpm onboard [--models | --hosts] [--dry-run] [--json] [--yes]
//   mpm onboard report     read-only inspection (same as --dry-run)
//   mpm onboard models     model-configuration step only
//   mpm onboard hosts      host-integration step only
//
// Subcommand table (args[0] dispatch):
//
//	report   read-only inspection; never mutates anything
//	models   generative + embedding model configuration step
//	hosts    framework integration step (explicit targets only)
//
// and the equivalent flag forms --dry-run / --models / --hosts.
// Exactly one of the three scopes may be selected; they are mutually
// exclusive because "run just the models step" and "run just the host
// step" are different requests, and silently running both would
// violate the brief's "whichever is passed runs alone".
//
// ---------------------------------------------------------------------------
// GUIDING PRINCIPLE
//
//	MPM explains its requirements; the operator chooses the models.
//
// Onboard may say what an embedding model is FOR and what degrades
// without one. It may not name a vendor, suggest a model, discover
// one, or apply a default. This preserves the v0.1-final decision to
// remove provider/model catalogues, automatic selection, branded
// menus and the embedding probe — and does not revert it.
//
// Concretely, in this file:
//   - no vendor name appears in any string literal;
//   - no model name appears in any string literal;
//   - nothing in this file calls a model endpoint;
//   - the retired `mpm config detect-embedding` probe is not revived,
//     not reimplemented, and not consulted.
//
// A test (TestOnboardIsProviderAgnostic) enforces the first two.
//
// ---------------------------------------------------------------------------
// WHAT THIS FILE REUSES (nothing here is a second configuration system)
//
//	guided model entry     mpm config  (no args = interactive wizard)
//	named profiles         mpm config profile add/set/get/list/remove
//	component binding      mpm config component set <c> <p>
//	capability binding     mpm config capability set <cap> <component>
//	structural validation  validateConfigStructure (handlers_config.go)
//	resolution outcome     Config.ResolveProfile / ResolveEmbeddingConfig
//	readiness verdict      ReadReadinessReadOnly (readiness.go)
//	host inspection        agent_installation/scripts/
//	                       check_installed_managed_blocks.py --json
//
// The host inspection is delegated rather than reimplemented on
// purpose. That script is the substrate's existing read-only managed
// -block diagnostic; reimplementing its probe logic in Go would create
// a second place where host state is interpreted, and the two would
// eventually disagree.
//
// ---------------------------------------------------------------------------
// CONSENT BOUNDARY (docs/ONBOARD.md §6, §11)
//
//	Detection is read-only. Detection never installs.
//
// `reconcile_managed_blocks.py` states the invariant this command
// exists inside: "MPM never creates the integration it would then
// have to reconcile." Onboard is a deliberate, bounded exception and
// is NOT a way around it: nothing discovered here becomes an install
// target without an operator naming that host. There is no --hosts
// auto. Commit 1 implements the read-only report only; the install
// path is added in a later commit and only behind per-host approval.
//
// ---------------------------------------------------------------------------
// HONESTY CONTRACT (docs/ONBOARD.md §7)
//
// The report separates three states and never collapses them:
//
//	Operational          configured and structurally valid
//	Deliberately skipped the operator declined it; consequences printed
//	Needs attention      configured but failing resolution/validation
//
// In particular, a host row reporting PASS means "the managed block
// is present and byte-current". It does NOT mean the host loads MPM
// at runtime. `RuntimeVerified` is carried on every row and is false
// in this commit; OpenCode's runtime loading in particular remains
// unverified and must stay that way until an actual tool invocation
// through that adapter is observed.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/config"
	"github.com/flowbyte-com/mpm-core/usererror"
)

// ---------------------------------------------------------------------------
// Scope
// ---------------------------------------------------------------------------

type onboardScope string

const (
	onboardScopeFull   onboardScope = "full"
	onboardScopeReport onboardScope = "report"
	onboardScopeModels onboardScope = "models"
	onboardScopeHosts  onboardScope = "hosts"
)

// onboardOptions is the fully-parsed request. Parsing is strict:
// unknown flags are an error, never silently ignored, per the
// flag-truthfulness contract (doctor_flag_contract.go:3-6 — "a flag
// that silently does nothing is a defect").
type onboardOptions struct {
	scope onboardScope

	// dryRun forces the read-only path regardless of scope. It is the
	// recommended first run: full plan, zero writes.
	dryRun bool

	// json emits the machine-readable envelope. Never routes through
	// the human renderer.
	json bool

	// assumeYes is non-interactive confirmation. It does NOT widen
	// what may be done: unattended operation still requires explicit
	// host targets (docs/ONBOARD.md §6.3).
	assumeYes bool

	// hostInstall selects `mpm onboard hosts install <target>...`, and
	// hostTargets are the hosts the operator NAMED. There is no "auto"
	// form and no flag that expands a detected set into targets; the
	// parser rejects the attempt by name rather than quietly treating it
	// as a literal hostname that does not exist.
	hostInstall bool
	hostTargets []string

	// hostRepair authorises rewriting a host whose managed section has
	// DRIFTED. It is separate from install because repair replaces
	// something that exists and works, which is a different decision
	// from filling a gap (ONBOARD.md §6.4: drift is reported; acting on
	// it is a separate, named approval).
	hostRepair bool
}

// The per-step write gates (mutateModelsAllowed, mutateHostsAllowed and
// mutateAllowed) live in onboard_models.go, next to the step that obeys
// them.

// ---------------------------------------------------------------------------
// Argument parsing
// ---------------------------------------------------------------------------

// parseOnboardArgs is a hand-rolled parser rather than flag.FlagSet
// because the subcommand/flag interleaving is positional
// (`mpm onboard hosts --json`), which flag.FlagSet stops at. Unknown
// tokens are errors.
func parseOnboardArgs(args []string) (onboardOptions, error) {
	opts := onboardOptions{scope: onboardScopeFull}

	explicit := map[onboardScope]bool{}
	setScope := func(s onboardScope) error {
		if explicit[s] {
			return nil
		}
		for other := range explicit {
			if other != s {
				return fmt.Errorf(
					"--%s and --%s select different steps; run one at a time",
					scopeFlagName(other), scopeFlagName(s))
			}
		}
		explicit[s] = true
		opts.scope = s
		return nil
	}

	for _, a := range args {
		switch a {
		case "report":
			if err := setScope(onboardScopeReport); err != nil {
				return opts, err
			}
		case "models":
			if err := setScope(onboardScopeModels); err != nil {
				return opts, err
			}
		case "hosts":
			if err := setScope(onboardScopeHosts); err != nil {
				return opts, err
			}
		case "--models":
			if err := setScope(onboardScopeModels); err != nil {
				return opts, err
			}
		case "--hosts":
			if err := setScope(onboardScopeHosts); err != nil {
				return opts, err
			}
		case "--dry-run", "-n":
			opts.dryRun = true
		case "--report":
			if err := setScope(onboardScopeReport); err != nil {
				return opts, err
			}
		case "--yes", "-y":
			opts.assumeYes = true
		case "--repair":
			opts.hostRepair = true
		case "--json", "-j":
			opts.json = true
		case "--check":
			// Accepted alias for --dry-run, borrowed from install.sh's
			// vocabulary. An alias is honest; a flag that changes
			// nothing is not.
			opts.dryRun = true
		case "install":
			if opts.scope != onboardScopeHosts {
				return opts, fmt.Errorf(
					"`install` applies to the host step only; run `mpm onboard --hosts install <target>`")
			}
			opts.hostInstall = true
		default:
			if !strings.HasPrefix(a, "-") {
				// Collected regardless of `install`, so that naming a
				// host without it produces "that host was named but
				// nothing will be installed" below, rather than a bare
				// "unknown argument" that leaves the operator guessing
				// whether the target was recognised.
				target, err := normaliseHostTarget(a)
				if err != nil {
					return opts, err
				}
				opts.hostTargets = append(opts.hostTargets, target)
				continue
			}
			return opts, fmt.Errorf(
				"unknown argument %q\n"+
					"  usage: mpm onboard [report|models|hosts] [--dry-run] [--json] [--yes]\n"+
					"  run `mpm onboard help` for the full command table", a)
		}
	}

	if opts.hostInstall && len(opts.hostTargets) == 0 {
		return opts, fmt.Errorf(
			"`install` needs at least one host named on the command line.\n"+
				"  targets: %s\n"+
				"  there is deliberately no `--hosts auto`: MPM is installed on machines that have\n"+
				"  never used any MPM integration, so a detected set is not evidence of consent",
			strings.Join(onboardHostLabels, ", "))
	}
	if opts.hostRepair && !opts.hostInstall {
		return opts, fmt.Errorf(
			"`--repair` only applies to `mpm onboard --hosts install`; nothing else rewrites a host")
	}
	if !opts.hostInstall {
		for _, t := range opts.hostTargets {
			return opts, fmt.Errorf("host %q was named without `install`; `mpm onboard --hosts` only inspects", t)
		}
	}

	// `mpm onboard report` is read-only by definition. Making that
	// structural rather than a convention: the scope name cannot be
	// paired with a mutating run.
	if opts.scope == onboardScopeReport {
		opts.dryRun = true
	}
	return opts, nil
}

// autoInstallWords are the tokens an operator reaches for when they want
// "just set up the ones I have". Each is rejected by name, with the
// reason, rather than treated as an unknown host and reported as a typo.
// The distinction matters: "you misspelled a host" and "MPM will not
// sweep your machine" are different messages, and conflating them would
// leave the operator to guess which rule they hit.
//
// The rule is ONBOARD.md §6.1/§6.3: MPM is installed on any machine that
// has any MPM integration, so a "is the mpm binary present" probe is
// satisfied on hosts that have never heard of MPM. An auto-sweep would
// defeat the gate that keeps the reconcile path from expanding MPM into
// frameworks the operator never chose.
var autoInstallWords = map[string]bool{
	"auto": true, "all": true, "any": true, "every": true,
	"*": true, "detected": true, "found": true, "installed": true,
}

// normaliseHostTarget validates one host label, rejecting the
// auto-sweep vocabulary explicitly and normalising case so `Pi` and `pi`
// are the same target rather than two.
func normaliseHostTarget(tok string) (string, error) {
	lower := strings.ToLower(strings.TrimSpace(tok))
	if autoInstallWords[lower] {
		return "", fmt.Errorf(
			"host target %q is refused: onboarding has no auto-sweep.\n"+
				"  name the hosts you want, one at a time: %s\n"+
				"  a host that is present but unnamed is skipped, not installed",
			tok, strings.Join(onboardHostLabels, ", "))
	}
	for _, known := range onboardHostLabels {
		if lower == known {
			return known, nil
		}
	}
	return "", fmt.Errorf(
		"unknown host target %q\n  known targets: %s\n  run `mpm onboard --hosts` to see which are present",
		tok, strings.Join(onboardHostLabels, ", "))
}

func scopeFlagName(s onboardScope) string {
	switch s {
	case onboardScopeModels:
		return "models"
	case onboardScopeHosts:
		return "hosts"
	case onboardScopeReport:
		return "report"
	}
	return "full"
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

func handleOnboard(args []string) int {
	if len(args) > 0 && args[0] == "help" {
		printOnboardHelp()
		return 0
	}

	// --help / -h are rewritten to the literal token "help" by the
	// router's parseFlags before any handler sees argv. `mpm onboard
	// --help` therefore arrives here as args == ["help"].
	jsonFlag, rest := ExtractJSONFlag(args)
	opts, err := parseOnboardArgs(rest)
	if err != nil {
		usererror.Error("%v", err)
		return 1
	}
	if jsonFlag {
		opts.json = true
	}

	rep, err := collectOnboardReport(opts)
	if err != nil {
		if opts.json {
			emitOnboardJSON(onboardReport{Success: false, Mode: string(opts.scope), Errors: []string{err.Error()}})
		} else {
			usererror.Error("onboard: %v", err)
		}
		return 1
	}

	// The guided steps run AFTER the read-only report is collected, so
	// the report they render is the state as it was found — not the
	// state onboarding made it look like. Re-collection after a mutation
	// would be a nicer ending, but it would also mean the sections above
	// were describing a machine state that no longer exists, and the
	// delta would have to be reconstructed from the step outcome.
	// Instead the step records what it did and what it did not finish,
	// and the exit code reflects the combination.
	if opts.scope == onboardScopeModels || opts.scope == onboardScopeFull {
		runModelStep(opts, &rep)
	}
	if opts.hostInstall {
		runHostStep(opts, &rep)
	}

	if opts.json {
		emitOnboardJSON(rep)
	} else {
		renderOnboardReport(rep)
	}
	return onboardExitCode(rep)
}

// onboardExitCode: 0 when nothing needs attention, 1 otherwise.
// Deliberately does not distinguish WARN from FAIL — `mpm onboard` is
// a setup aid, not a gate; `mpm doctor` owns graded exits.
func onboardExitCode(rep onboardReport) int {
	if len(rep.Attention) > 0 || len(rep.Errors) > 0 {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------
// Report model
// ---------------------------------------------------------------------------

// onboardReport is the single source of truth for both renderers.
// Every slice is initialised to empty (never nil) so `--json`
// consumers can iterate unconditionally — the doctor JSON contract
// normalises nil slices the same way.
type onboardReport struct {
	// Success means the REPORT was produced — not that the installation
	// is healthy. That is doctor's convention and it is kept here so a
	// consumer has one rule rather than two. Actionable state is read
	// from Attention; a run that needs attention is still a successful
	// report.
	Success bool   `json:"success"`
	Mode    string `json:"mode"`
	DryRun  bool   `json:"dry_run"`

	// Installation is MPM's own presence and workspace health.
	Installation []onboardRow `json:"installation"`
	Readiness    []onboardRow `json:"readiness"`
	Models       []onboardRow `json:"models"`
	Hosts        []onboardRow `json:"hosts"`

	// Attention lists what requires action. Skipped lists what the
	// operator (or the design) deliberately did not do. They are
	// different: one is a defect, the other is a decision.
	Attention []string `json:"attention"`
	Skipped   []string `json:"skipped"`

	// Plan names every mutation a run WOULD make. It is populated by
	// --dry-run and by the guided steps; in a pure inspection it is
	// legitimately empty, because a pure inspection proposes nothing.
	Plan []string `json:"plan"`

	// ModelPlan is what the guided model step would do, for models and
	// full scope. Non-nil means onboarding has something to say about
	// configuration — including when the answer is "nothing to do",
	// which is itself worth saying out loud.
	ModelPlan *modelStepPlan `json:"model_plan,omitempty"`

	// ModelStep records the guided model step's outcome: the plan, the
	// consent decision, and what actually resolved afterwards. Populated
	// for models and full scope.
	ModelStep *modelStepOutcome `json:"model_step,omitempty"`

	// HostStep records the host step's outcome: the hosts the operator
	// NAMED, the decision taken for each, and what each installer
	// actually did. Populated only when targets were named — a plain
	// inspection has no step to record.
	HostStep *hostStepOutcome `json:"host_step,omitempty"`

	// NotVerified names things this command structurally cannot
	// check. Always populated; an empty list here would be the bug.
	NotVerified []string `json:"not_verified"`

	// NotAvailable names requested-but-unimplemented steps. It exists
	// to prevent the silent no-op: a command that inspects, finds
	// problems, names remedies — and then does nothing about them —
	// must say so. Without this, `mpm onboard` on a misconfigured
	// install would read as "onboarding ran and here is your state",
	// when in fact no step has the ability to change it yet.
	NotAvailable []string `json:"not_available"`

	Errors []string `json:"errors"`
}

// onboardRow is one report line: a subject, a verdict, and detail.
// Verdict vocabulary is deliberately three-valued and matches the
// report contract — "operational", "skipped", "needs attention".
type onboardRow struct {
	Subject string   `json:"subject"`
	State   string   `json:"state"` // operational | skipped | attention
	Detail  string   `json:"detail"`
	Lines   []string `json:"lines,omitempty"`
	Remedy  string   `json:"remedy,omitempty"`
}

// Row states.
const (
	onboardStateOperational = "operational"
	onboardStateSkipped     = "skipped"
	onboardStateAttention   = "attention"
)

// ---------------------------------------------------------------------------
// Collection — read-only
// ---------------------------------------------------------------------------

// collectOnboardReport gathers every fact the report renders. It
// performs NO writes: no config save, no database mutation, no file
// creation, no process launch except the read-only host inspector
// (which is documented read-only and is invoked with --json).
//
// collectOnboardReport is the single place mutation would ever be
// introduced, which is why the no-write property is testable: the
// whole function is exercised against an isolated workspace with a
// filesystem digest before and after.
func collectOnboardReport(opts onboardOptions) (onboardReport, error) {
	rep := onboardReport{
		Mode:         string(opts.scope),
		DryRun:       opts.dryRun || !opts.mutateAllowed(),
		Attention:    []string{},
		Skipped:      []string{},
		Plan:         []string{},
		NotVerified:  []string{},
		NotAvailable: []string{},
		Errors:       []string{},
	}

	dm := getDBConcrete()

	collectInstallation(&rep, dm)
	collectReadiness(&rep, dm)
	if opts.scope != onboardScopeHosts {
		collectModels(&rep, dm)
	}
	if opts.scope != onboardScopeModels {
		collectHosts(&rep)
	}

	// The gates are per-step, not global: each step reports what IT
	// cannot do, so "not available" always names the missing thing
	// rather than a general capability.
	//
	// For the host step the interesting case is not the gate but the
	// TARGETS. `mpm onboard --hosts` inspects and proposes; it installs
	// nothing because the operator named no host. Saying "installation
	// is unavailable" there would be false — it is available and simply
	// was not asked for — and saying nothing would leave an inspect-only
	// run looking like a silent no-op.
	switch opts.scope {
	case onboardScopeHosts, onboardScopeFull:
		switch {
		case !opts.mutateHostsAllowed():
			rep.NotAvailable = append(rep.NotAvailable,
				"host installation: this build inspects integrations but cannot install them. "+
					"Run the per-host installer directly, one host at a time")
		case !opts.hostInstall:
			rep.NotAvailable = append(rep.NotAvailable,
				"host installation: available, but no host was named. "+
					"Run `mpm onboard --hosts install <target>` — targets are never inferred from what was detected")
		}
	}
	if !opts.mutateModelsAllowed() {
		switch opts.scope {
		case onboardScopeModels, onboardScopeFull:
			rep.NotAvailable = append(rep.NotAvailable,
				"model configuration: this build reports model state but cannot change it. "+
					"Configure with `mpm config` (no arguments runs the wizard)")
		}
	}
	if !opts.mutateModelsAllowed() {
		switch opts.scope {
		case onboardScopeModels, onboardScopeFull:
			rep.NotAvailable = append(rep.NotAvailable,
				"model configuration: this build reports model state but cannot change it. "+
					"Configure with `mpm config` (no arguments runs the wizard)")
		}
	}

	rep.Success = len(rep.Errors) == 0
	return rep, nil
}

// collectInstallation reports MPM's own presence: binary, data root,
// workspace, database. These are the facts that make every other row
// meaningful — if the database is unreachable, the model rows are
// advisory and the report says so rather than presenting them as
// operational.
func collectInstallation(rep *onboardReport, dm *mpminternal.DatabaseManager) {
	if exe, err := os.Executable(); err == nil {
		if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			exe = resolved
		}
		rep.Installation = append(rep.Installation, onboardRow{
			Subject: "MPM binary",
			State:   onboardStateOperational,
			Detail:  exe,
		})
	} else {
		rep.Installation = append(rep.Installation, onboardRow{
			Subject: "MPM binary",
			State:   onboardStateAttention,
			Detail:  "path could not be resolved: " + err.Error(),
		})
	}

	if dir := config.GetMPMDir(); dir != "" {
		rep.Installation = append(rep.Installation, onboardRow{
			Subject: "Data root",
			State:   onboardStateOperational,
			Detail:  dir,
		})
	} else {
		rep.Installation = append(rep.Installation, onboardRow{
			Subject: "Data root",
			State:   onboardStateAttention,
			Detail:  "could not be resolved (is $HOME set?)",
			Remedy:  "export HOME, or set MPM_DIR",
		})
	}

	if path := config.ConfigPath(); path != "" {
		row := onboardRow{
			Subject: "Configuration",
			State:   onboardStateOperational,
			Detail:  path,
		}
		if _, err := os.Stat(path); err != nil {
			row.State = onboardStateSkipped
			row.Detail = "not written yet — the defaults are in use"
			row.Remedy = "run `mpm config` to create it"
		}
		rep.Installation = append(rep.Installation, row)
	}

	if dm == nil {
		rep.Installation = append(rep.Installation, onboardRow{
			Subject: "Database",
			State:   onboardStateAttention,
			Detail:  "unavailable — the model and readiness rows below are advisory only",
			Remedy:  "check MPM_DB_PATH / MPM_WORKSPACE, then `mpm doctor`",
		})
		rep.Attention = append(rep.Attention,
			"database unavailable: model and readiness state could not be verified")
		return
	}
	row := onboardRow{Subject: "Database", State: onboardStateOperational, Detail: "open"}
	if err := dm.SQLDB().Ping(); err != nil {
		row.State = onboardStateAttention
		row.Detail = "unreachable: " + err.Error()
		row.Remedy = "run `mpm doctor` for diagnostics"
		rep.Attention = append(rep.Attention, "database unreachable: "+err.Error())
	}
	rep.Installation = append(rep.Installation, row)
}

// collectReadiness delegates to the substrate's readiness service.
//
// ReadReadinessReadOnly, not ReadReadiness: the latter may start the
// scheduler systemd unit, which is a mutation and therefore forbidden
// in an inspection or dry-run. See readiness.go for the full
// rationale.
func collectReadiness(rep *onboardReport, dm *mpminternal.DatabaseManager) {
	items := ReadReadinessReadOnly(dm)
	for _, it := range items {
		row := onboardRow{
			Subject: it.Name,
			State:   readinessState(it),
			Detail:  it.Detail,
			Remedy:  it.Hint,
		}
		if it.Hint != "" {
			row.Lines = []string{it.Hint}
		}
		rep.Readiness = append(rep.Readiness, row)
		if !it.OK && it.Level != readinessLevelInfo {
			rep.Attention = append(rep.Attention,
				fmt.Sprintf("%s: %s", it.Name, it.Detail))
		}
	}
}

// readinessState maps a ReadinessItem onto the report vocabulary. The
// INFO level means "optional subsystem absent", which the report
// contract classifies as deliberately skipped — never as a defect,
// because absence of an optional feature must not make the substrate
// look unhealthy.
func readinessState(it ReadinessItem) string {
	if it.OK {
		return onboardStateOperational
	}
	if it.Level == readinessLevelInfo {
		return onboardStateSkipped
	}
	return onboardStateAttention
}

// collectModels reports the two model roles and their consequences.
//
// Generative is REQUIRED; embedding is OPTIONAL. That asymmetry is
// the substrate's existing contract (service_doctor.go:738-744), not
// an onboarding invention, and the consequence text below mirrors it
// exactly. Getting this backwards would make skipping embeddings look
// like a failure, which the design explicitly forbids.
//
// No provider is contacted. Resolution is pure over the on-disk
// config: Config.ResolveProfile for generative components and
// ResolveEmbeddingConfig for embedding — which is NOT routed through
// ResolveProfile, and must not inherit profiles.default.
func collectModels(rep *onboardReport, dm *mpminternal.DatabaseManager) {
	cfg, err := config.LoadConfig()
	if err != nil {
		rep.Models = append(rep.Models, onboardRow{
			Subject: "Configuration",
			State:   onboardStateAttention,
			Detail:  "could not be loaded: " + err.Error(),
			Remedy:  "check " + config.ConfigPath(),
		})
		rep.Attention = append(rep.Attention, "configuration could not be loaded: "+err.Error())
		return
	}

	// -- structural validation (the existing contract, not a new one)
	v := validateConfigStructure(cfg)
	if v.Failed() {
		rep.Attention = append(rep.Attention, fmt.Sprintf(
			"configuration has %d structural issue(s); run `mpm config validate`", v.Issues))
		rep.Models = append(rep.Models, onboardRow{
			Subject: "Structural validation",
			State:   onboardStateAttention,
			Detail:  fmt.Sprintf("%d issue(s) — run `mpm config validate`", v.Issues),
			Lines:   v.Lines,
			Remedy:  "mpm config validate",
		})
	} else {
		rep.Models = append(rep.Models, onboardRow{
			Subject: "Structural validation",
			State:   onboardStateOperational,
			Detail:  "configuration is structurally valid",
		})
	}

	// -- generative: every known component resolves to a profile
	for _, comp := range knownComponents {
		res := cfg.ResolveProfile(comp)
		row := onboardRow{
			Subject: "Generative · " + comp,
			Detail:  describeGenerative(res),
		}
		switch {
		case !res.Valid:
			row.State = onboardStateAttention
			row.Remedy = fmt.Sprintf(
				"binding points at a profile that does not exist: `mpm config component unset %q` or define it",
				comp)
		case res.Profile == nil:
			row.State = onboardStateAttention
			row.Lines = generativeConsequences()
			row.Remedy = "mpm config"
		default:
			row.State = onboardStateOperational
			row.Detail += " · " + localityOf(res.Profile.BaseURL, res.Profile.Provider)
		}
		if row.State == onboardStateAttention {
			rep.Attention = append(rep.Attention,
				fmt.Sprintf("generative component %q is not configured", comp))
		}
		rep.Models = append(rep.Models, row)
	}

	// -- embedding: optional, resolved through its own resolver
	emb := mpminternal.ResolveEmbeddingConfig(cfg)
	row := onboardRow{
		Subject: "Embedding",
		Detail:  describeEmbedding(emb),
	}
	switch {
	case emb.IntentionallyDisabled:
		row.State = onboardStateSkipped
		row.Detail = "intentionally disabled by the operator"
		row.Lines = embeddingConsequences()
		row.Remedy = "re-enable with: `mpm config component set embedding <profile>`"
		rep.Skipped = append(rep.Skipped, "embedding: disabled by operator")
	case emb.Status == mpminternal.EmbeddingStatusConfigured:
		row.State = onboardStateOperational
	case emb.Status == mpminternal.EmbeddingStatusMisconfigured:
		row.State = onboardStateAttention
		if emb.LastError != nil {
			row.Detail = "misconfigured: " + emb.LastError.Error()
		}
		row.Remedy = "mpm config"
		rep.Attention = append(rep.Attention, "embedding configuration is misconfigured: "+row.Detail)
	default:
		// Absent or unreachable. Absent is the supported "not
		// configured" state and is a skip, not a defect. Unreachable
		// is different: something IS configured and it did not work.
		if emb.Status == mpminternal.EmbeddingStatusUnreachable {
			row.State = onboardStateAttention
			row.Remedy = "verify the endpoint is reachable, then re-run `mpm onboard`"
			rep.Attention = append(rep.Attention, "embedding endpoint configured but unreachable")
		} else {
			row.State = onboardStateSkipped
			row.Lines = embeddingConsequences()
			row.Remedy = "optional — configure later with `mpm config`"
			rep.Skipped = append(rep.Skipped, "embedding: not configured")
		}
	}
	rep.Models = append(rep.Models, row)

	// -- the memory-store heterogeneity gap, stated before it bites.
	//
	// What is true here CHANGED when the embedding_model migration landed
	// (2026-10-13), so this text is derived from the live store rather
	// than asserted as a constant. An earlier version hardcoded "memories
	// has no embedding_model column"; after the migration that sentence
	// became false and onboarding kept printing it — a report that
	// misstates the substrate is worse than one that is silent, because
	// the operator has no way to tell which parts to trust.
	//
	// The state that matters is per-row, not per-schema: the compatibility
	// filter compares embedding_model to the ACTIVE model and excludes
	// what it cannot prove, so a store where the column exists but rows
	// are unlabelled still returns nothing from semantic recall.
	if emb.Status == mpminternal.EmbeddingStatusConfigured && emb.ProviderName != "" {
		rep.NotVerified = append(rep.NotVerified,
			"embedding model identity of already-stored memory vectors: "+
				memoryVectorFingerprintState(dm, emb))
	}

	rep.NotVerified = append(rep.NotVerified,
		"live reachability and authentication of the configured endpoints — `mpm doctor` probes those; onboard does not")
}

// memoryFingerprintCounts reports (column present, live vectors, labelled
// vectors). `live` counts rows eligible for retrieval — soft-deleted rows
// are excluded, since the backfill's pending predicate excludes them too
// and counting them would overstate the work.
func memoryFingerprintCounts(dm *mpminternal.DatabaseManager) (haveColumn bool, live, labelled int, err error) {
	db := dm.SQLDB()
	if !memoriesHaveModelColumn(db) {
		return false, 0, 0, nil
	}
	err = db.QueryRow(`
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN embedding_model IS NOT NULL
		                          AND TRIM(embedding_model) <> '' THEN 1 ELSE 0 END), 0)
		  FROM memories
		 WHERE deleted_at IS NULL AND embedding IS NOT NULL AND embedding != 'null'`,
	).Scan(&live, &labelled)
	return true, live, labelled, err
}

// memoryVectorFingerprintState describes, from the live store, how much of
// `memories` can currently be proven comparable to the active embedding
// model — and what to run when the answer is "not all of it".
//
// It reports a fact rather than a reassurance. Three distinct states, each
// needing a different response:
//
//   - no column: pre-migration store. Nothing is provable and a model
//     change leaves mixed-generation vectors undetectable.
//   - column present, rows unlabelled: the migration applied but the
//     backfill has not run. Semantic recall over `memories` returns
//     nothing until it does — the compatibility filter excludes what it
//     cannot prove. This is the state a freshly deployed binary finds.
//   - fully labelled: nothing outstanding.
//
// A count is reported wherever one exists, because "some rows are
// unlabelled" is not actionable in the way "146 rows are" is.
func memoryVectorFingerprintState(dm *mpminternal.DatabaseManager, emb *mpminternal.EmbeddingConfig) string {
	const remedy = "Run `mpm ops backfill-embeddings` to re-embed them under " +
		"the active model; add --force-reembed to re-embed rows that already carry a fingerprint."

	if dm == nil {
		return "not checked — the database was unavailable to this report. " + remedy
	}
	haveColumn, live, labelled, err := memoryFingerprintCounts(dm)
	if err != nil {
		return "not checked (" + err.Error() + "). " + remedy
	}
	if !haveColumn {
		return "`memories` has no embedding_model column, so changing the model " +
			"leaves mixed-generation vectors undetectable. " + remedy
	}
	if live == 0 {
		// Nothing stored yet — there is no heterogeneity to report, and
		// saying "all 0 rows carry a fingerprint" would read as a finding
		// rather than as the absence of one.
		return "no stored memory vectors yet, so there is nothing to re-embed. " +
			"Vectors written from now on record the active model's fingerprint."
	}
	if labelled == live {
		return fmt.Sprintf("all %d live memory vectors carry the active model's fingerprint (%s)",
			live, emb.ProviderName)
	}
	return fmt.Sprintf(
		"%d of %d live memory vectors carry no embedding_model and are therefore "+
			"excluded from semantic recall until re-embedded (active model %s). %s",
		live-labelled, live, emb.ProviderName, remedy)
}

// generativeConsequences states what is lost without a generative
// profile. These are substrate facts, listed so skipping is an
// informed decision rather than a silent one.
func generativeConsequences() []string {
	return []string{
		"unavailable: synthesis, admission, compact lesson synthesis, critic review",
		"unaffected: memory add and recall, BM25/FTS5 search, scheduling, audit, doctor",
		"`mpm doctor` reports this as WARN, not FAIL — it is a real functional loss",
	}
}

// embeddingConsequences states what is lost without an embedding
// profile. Contrast with the generative list on purpose: this is a
// quality upgrade declined, not a broken substrate.
func embeddingConsequences() []string {
	return []string{
		"unavailable: semantic recall (--semantic), vector ranking, the vector index, embedding backfill",
		"unaffected: BM25/FTS5 recall and all structured retrieval continue unchanged",
		"`mpm doctor` reports this as INFO by design — absence of an optional feature is not a defect",
	}
}

func describeGenerative(res config.ProfileResolution) string {
	if res.ConfiguredName == "disabled" {
		return "bound to \"disabled\" — this component resolves to nothing"
	}
	if res.Profile == nil {
		return "not configured"
	}
	d := fmt.Sprintf("%s → %s · %s",
		orDefault(res.ConfiguredName, "(default profile)"),
		res.Profile.Model,
		res.Profile.Provider)
	if res.Profile.BaseURL != "" {
		d += " · " + res.Profile.BaseURL
	}
	return d + " (" + string(res.Source) + ")"
}

func describeEmbedding(emb *mpminternal.EmbeddingConfig) string {
	if emb == nil {
		return "unknown"
	}
	switch emb.Source {
	case mpminternal.EmbeddingSourceProfile:
		return fmt.Sprintf("%s → %s · %s (profile)",
			emb.ProfileName, emb.ProviderName, emb.Status)
	case mpminternal.EmbeddingSourceEnvFallback:
		return fmt.Sprintf("%s (environment fallback)", emb.ProviderName)
	case mpminternal.EmbeddingSourceDisabled:
		return "disabled"
	}
	return "not configured"
}

// localityOf labels an endpoint local or remote.
//
// This is EXPLANATORY FRAMING ONLY, derived from the same locality
// rule the substrate uses when deciding whether a credential is
// required (model_factory.go:103-110). Nothing is persisted from it
// and it is not a configuration field. The point of asking is that it
// changes what the operator must supply next — a local endpoint
// usually needs no credential and keeps data on the host.
func localityOf(baseURL, provider string) string {
	s := strings.ToLower(baseURL + " " + provider)
	for _, marker := range []string{"localhost", "127.0.0.1", "0.0.0.0", "11434", "lmstudio", "ollama"} {
		if strings.Contains(s, marker) {
			return "local endpoint (credential usually optional, data stays on this host)"
		}
	}
	if baseURL == "" {
		return "remote endpoint (credential usually required, data leaves this host)"
	}
	return "remote endpoint (credential usually required, data leaves this host)"
}

func orDefault(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// ---------------------------------------------------------------------------
// Host inspection — read-only, delegated
// ---------------------------------------------------------------------------

// onboardHostLabels are the framework identifiers onboard reports.
// They mirror HOST_INSTALL_TARGETS in
// agent_installation/scripts/check_installed_managed_blocks.py, which
// is the authoritative list. TestOnboardHostInventoryMatchesPython
// parses that Python file and fails if the two drift, so this table
// can never quietly become a second source of truth.
var onboardHostLabels = []string{"claude_code", "openclaw", "opencode", "pi"}

// collectHosts runs the substrate's read-only managed-block
// diagnostic and maps its verdict vocabulary onto the report.
//
// This is the DETECTION step of the consent boundary. It discovers,
// it proposes, it stops. It never installs, never repairs, and never
// rewrites an installed file — which is a property of the script it
// delegates to, not a promise this function makes about itself.
func collectHosts(rep *onboardReport) {
	rows, err := inspectInstalledHosts()
	if err != nil {
		rep.Hosts = append(rep.Hosts, onboardRow{
			Subject: "Host inspection",
			State:   onboardStateAttention,
			Detail:  "unavailable: " + err.Error(),
			Remedy:  "run `python3 agent_installation/scripts/check_installed_managed_blocks.py` directly",
		})
		rep.NotVerified = append(rep.NotVerified,
			"framework integrations: the inspection tooling was not locatable from this binary")
		return
	}

	for _, r := range rows {
		row := onboardRow{
			Subject: displayHostName(r.Host),
			Detail:  fmt.Sprintf("%s · %s · %s", r.Verdict, r.File, r.InstallPath),
			Remedy:  r.Repair,
		}
		switch r.Verdict {
		case "PASS":
			row.State = onboardStateOperational
			row.Detail = "managed block present and current · " + r.InstallPath
		case "WARN", "ERROR":
			row.State = onboardStateAttention
			rep.Attention = append(rep.Attention, fmt.Sprintf(
				"%s managed block needs attention: %s", displayHostName(r.Host), r.Verdict))
		default: // ABSENT
			row.State = onboardStateSkipped
			row.Detail = "not installed"
			rep.Skipped = append(rep.Skipped,
				displayHostName(r.Host)+": integration not installed")
		}
		if r.Detail != "" {
			row.Lines = append(row.Lines, r.Detail)
		}
		rep.Hosts = append(rep.Hosts, row)
	}

	// The honest caveat, always emitted. A PASS row means the managed
	// block is present and byte-current. It does NOT mean the host
	// loads MPM at runtime, and reporting it as though it did is the
	// specific dishonesty this command must avoid.
	rep.NotVerified = append(rep.NotVerified,
		"host RUNTIME loading: registration is not proof of loading. A present, current "+
			"managed block shows what the host is TOLD, not that it loaded MPM. OpenCode in "+
			"particular has no confirmed runtime load; verify by observing a real "+
			"`mpm_memory/*` tool invocation through that adapter")
	rep.Skipped = append(rep.Skipped,
		"host runtime verification: registration is not proof of loading (no adapter is started by onboard)")
}

// installedHostResult mirrors one entry of the inspector's JSON.
type installedHostResult struct {
	Host        string `json:"host"`
	File        string `json:"file"`
	Block       string `json:"block"`
	InstallPath string `json:"install_path"`
	Verdict     string `json:"verdict"`
	Detail      string `json:"detail"`
	Repair      string `json:"repair"`
	MarkerState string `json:"marker_state"`
}

// inspectInstalledHosts invokes the existing read-only diagnostic.
//
// The script is invoked with --home so tests can point it at a
// fixture tree without reading the real installed files, and with
// --json so this function does not parse a human table.
func inspectInstalledHosts() ([]installedHostResult, error) {
	script, err := locateHostInspector()
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory: %w", err)
	}

	cmd := exec.Command("python3", script, "--json", "--home", home)
	// A defensive env: the inspector imports the reconciler, which is
	// also imported by install paths. Nothing here should reach the
	// network; make that explicit rather than incidental.
	cmd.Env = append(os.Environ(), "MPM_ONBOARD_READ_ONLY=1")

	out, runErr := cmd.Output()
	if runErr != nil {
		// Exit 2 is "tooling error", 1 is "WARN or ERROR observed".
		// Only the former means we got no usable data.
		// Exit 1 means "WARN or ERROR observed" — the inspector still
		// printed a usable document on stdout. Exit 2 is a tooling
		// error and we have nothing to parse.
		var ee *exec.ExitError
		if !errors.As(runErr, &ee) || ee.ExitCode() != 1 {
			return nil, fmt.Errorf("inspector failed: %w", runErr)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("inspector produced no output")
	}
	var rows []installedHostResult
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("parse inspector output: %w", err)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Host < rows[j].Host })
	return rows, nil
}

// locateHostInspector finds agent_installation/scripts/ in the
// checkout, honouring MPM_REPO_ROOT first.
//
// An installed binary outside a checkout legitimately cannot find
// it. That is reported as "inspection unavailable" with the manual
// command, never as "no hosts installed" — the difference between
// "nothing is there" and "I could not look" must survive.
func locateHostInspector() (string, error) {
	rel := filepath.Join("agent_installation", "scripts", "check_installed_managed_blocks.py")
	return resolveRepoRelative(rel)
}

// resolveRepoRelative finds a repo-relative path, honouring
// MPM_REPO_ROOT first and otherwise walking up from the executable and
// then the working directory.
//
// The inspector and the adapter installers must agree on where the
// checkout is. Sharing the resolution is what keeps them from disagreeing
// about which install.sh they are talking about.
func resolveRepoRelative(rel string) (string, error) {

	if root := os.Getenv("MPM_REPO_ROOT"); root != "" {
		candidate := filepath.Join(root, rel)
		if onboardPathExists(candidate) {
			return candidate, nil
		}
		return "", fmt.Errorf("MPM_REPO_ROOT=%s has no %s", root, rel)
	}

	// Walk up from the executable, then from the working directory.
	var starts []string
	if exe, err := os.Executable(); err == nil {
		if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			exe = resolved
		}
		starts = append(starts, filepath.Dir(exe))
	}
	if wd, err := os.Getwd(); err == nil {
		starts = append(starts, wd)
	}

	for _, start := range starts {
		dir := start
		for i := 0; i < 8; i++ {
			candidate := filepath.Join(dir, rel)
			if onboardPathExists(candidate) {
				return candidate, nil
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return "", fmt.Errorf("agent_installation/scripts/ not found near the binary or working directory")
}

// displayHostName turns a manifest host_label into something an
// operator recognises, without implying any provider relationship.
// onboardFileExists is a local helper because the package-level
// fileExists lives in a _test.go file and is therefore not compiled
// into the binary.
// onboardPathExists reports whether anything exists at path, file or
// directory. The resolver is shared by the inspector (which needs a
// script) and the adapter root (which is a directory), so requiring a
// regular file here would have made the adapter lookup fail against a
// perfectly good checkout.
func onboardPathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func onboardFileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

func displayHostName(label string) string {
	switch label {
	case "claude_code":
		return "Claude Code"
	case "opencode":
		return "OpenCode"
	case "pi":
		return "Pi"
	case "openclaw":
		return "OpenClaw"
	}
	return label
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

func stateMarker(state string) string {
	switch state {
	case onboardStateOperational:
		return "✓"
	case onboardStateSkipped:
		return "○"
	case onboardStateAttention:
		return "!"
	}
	return " "
}

func renderOnboardReport(rep onboardReport) {
	fmt.Println()
	fmt.Println("MPM · Onboarding report")
	fmt.Println()

	renderSection("Installation", rep.Installation)
	renderSection("Readiness", rep.Readiness)
	renderSection("Models", rep.Models)
	renderSection("Hosts", rep.Hosts)

	if len(rep.Plan) > 0 {
		fmt.Println("Plan")
		if rep.DryRun {
			fmt.Println("  (nothing above was written — this is a dry run)")
		} else if rep.ModelStep != nil && !rep.ModelStep.Applied {
			fmt.Println("  (proposed — nothing was applied; see the step outcome below for why)")
		} else {
			fmt.Println("  (recorded before approval — see the step outcome below for what was actually applied)")
		}
		for _, step := range rep.Plan {
			fmt.Printf("  → %s\n", step)
		}
		fmt.Println()
	}

	renderModelStep(rep)
	renderHostStep(rep)

	// The scriptable equivalent is printed whenever onboarding could not
	// prompt. Declining to be unattended is only useful if the operator
	// is handed the other way to do it.
	if rep.ModelStep != nil && !rep.ModelStep.Applied && strings.Contains(rep.ModelStep.Reason, "not a terminal") {
		fmt.Println("Without a terminal")
		for _, c := range nonInteractiveModelCommands() {
			fmt.Printf("  %s\n", c)
		}
		fmt.Println()
	}

	if len(rep.Skipped) > 0 {
		fmt.Println("Deliberately skipped")
		for _, s := range rep.Skipped {
			fmt.Printf("  ○ %s\n", s)
		}
		fmt.Println()
	}

	if len(rep.Attention) > 0 {
		fmt.Println("Needs attention")
		for _, s := range rep.Attention {
			fmt.Printf("  ! %s\n", s)
		}
		fmt.Println()
	}

	if len(rep.NotAvailable) > 0 {
		fmt.Println("Not available in this build")
		for _, s := range rep.NotAvailable {
			fmt.Printf("  · %s\n", s)
		}
		fmt.Println()
	}

	if len(rep.NotVerified) > 0 {
		fmt.Println("Not verified by onboarding")
		for _, s := range rep.NotVerified {
			fmt.Printf("  · %s\n", s)
		}
		fmt.Println()
	}
}

// renderModelStep prints the guided step's outcome.
//
// Applied and completed are rendered separately on purpose. A run where
// the wizard saved a generative profile and left the optional embedding
// profile unconfigured is a real, useful outcome — but rendering it as
// "✓ model configuration complete" would tell the operator something
// untrue, and rendering it as a failure would train them to distrust a
// run that did exactly what they approved.
func renderModelStep(rep onboardReport) {
	step := rep.ModelStep
	if step == nil {
		return
	}
	fmt.Println("Model step")
	fmt.Printf("  · %s\n", step.Reason)
	if step.Applied {
		fmt.Printf("  · the existing `mpm config` wizard ran and exited %d\n", step.ExitCode)
	}
	for _, d := range step.Done {
		fmt.Printf("  ✓ %s is settled\n", d)
	}
	// The marker depends on whether onboarding TRIED. Outstanding after a
	// run is a partial completion and warrants "!"; outstanding because the
	// step was declined is just the state the Models section already
	// reports, and marking it "!" here would inflate an optional,
	// deliberately-skipped capability into a defect.
	marker := "·"
	if step.Applied {
		marker = "!"
	}
	for _, r := range step.Remaining {
		fmt.Printf("  %s %s is still outstanding — configure later with `mpm config`\n", marker, r)
	}
	fmt.Println()
}

// renderHostStep prints the host step's per-host outcome.
//
// Each host gets its own line with its own verdict, because the step is
// per-adapter in every meaningful sense: one installer runs, backs up
// its own file, and can be undone only by re-running that same
// installer. Collapsing several hosts into one "done" would misdescribe
// a partial run — the exact failure ONBOARD.md §6.7 warns about.
func renderHostStep(rep onboardReport) {
	step := rep.HostStep
	if step == nil {
		return
	}
	fmt.Println("Host step")
	fmt.Printf("  · targets named by the operator: %s\n", strings.Join(step.Targets, ", "))
	for _, r := range step.Results {
		marker := "·"
		switch r.Verdict {
		case hostVerdictPass:
			marker = "✓"
		case "failed":
			marker = "!"
		}
		fmt.Printf("  %s %-14s %s\n", marker, displayHostName(r.Host), r.Detail)
		if r.ExitCode != 0 {
			fmt.Printf("      installer exited %d\n", r.ExitCode)
		}
		for _, b := range r.Backups {
			fmt.Printf("      backup: %s\n", b)
		}
		if r.Output != "" && r.Verdict == "failed" {
			for _, line := range strings.Split(r.Output, "\n") {
				fmt.Printf("      | %s\n", line)
			}
		}
	}
	if step.Rollback != "" {
		fmt.Printf("  · %s\n", step.Rollback)
	}
	fmt.Println()
}

func renderSection(label string, rows []onboardRow) {
	fmt.Println(label)
	if len(rows) == 0 {
		fmt.Println("  (not inspected in this mode)")
		fmt.Println()
		return
	}
	for _, r := range rows {
		fmt.Printf("  %s %-26s %s\n", stateMarker(r.State), r.Subject, r.Detail)
		for _, line := range r.Lines {
			if r.Remedy != "" && line == r.Remedy {
				continue
			}
			fmt.Printf("      %s\n", line)
		}
		if r.Remedy != "" {
			fmt.Printf("      remedy: %s\n", r.Remedy)
		}
	}
	fmt.Println()
}

func emitOnboardJSON(rep onboardReport) {
	// Normalise nil slices so consumers can iterate unconditionally.
	if rep.Installation == nil {
		rep.Installation = []onboardRow{}
	}
	if rep.Readiness == nil {
		rep.Readiness = []onboardRow{}
	}
	if rep.Models == nil {
		rep.Models = []onboardRow{}
	}
	if rep.Hosts == nil {
		rep.Hosts = []onboardRow{}
	}
	if rep.Attention == nil {
		rep.Attention = []string{}
	}
	if rep.Skipped == nil {
		rep.Skipped = []string{}
	}
	if rep.Plan == nil {
		rep.Plan = []string{}
	}
	if rep.NotVerified == nil {
		rep.NotVerified = []string{}
	}
	if rep.NotAvailable == nil {
		rep.NotAvailable = []string{}
	}
	if rep.Errors == nil {
		rep.Errors = []string{}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(rep)
}

// ---------------------------------------------------------------------------
// Help
// ---------------------------------------------------------------------------

func printOnboardHelp() {
	fmt.Println()
	fmt.Println("mpm onboard — guided setup and verification")
	fmt.Println()
	fmt.Println("Usage: mpm onboard [report|models|hosts] [options]")
	fmt.Println()
	fmt.Println("Runs the same code path on a fresh and on an established installation:")
	fmt.Println("it inspects what is already configured rather than branching on a")
	fmt.Println("first-run state that MPM does not keep. Re-running is safe and changes")
	fmt.Println("nothing that is already working.")
	fmt.Println()
	fmt.Println("Modes")
	fmt.Println("  (none)             full run: models, then hosts, then the report")
	fmt.Println("  report             read-only inspection; never writes anything")
	fmt.Println("  models             model-configuration step only")
	fmt.Println("  hosts              host-integration step only (inspect and propose)")
	fmt.Println("  hosts install      install into hosts you NAME — see below")
	fmt.Println()
	fmt.Println("Options")
	fmt.Println("  --models           same as the `models` subcommand")
	fmt.Println("  --hosts            same as the `hosts` subcommand")
	fmt.Println("  --dry-run, -n      print the full plan and change nothing (recommended first run)")
	fmt.Println("  --check            alias for --dry-run")
	fmt.Println("  --json, -j         machine-readable report; never routes through the human renderer")
	fmt.Println("  --yes, -y          non-interactive confirmation")
	fmt.Println("  --repair           authorise rewriting a host whose managed section has drifted")
	fmt.Println()
	fmt.Println("The three scopes are mutually exclusive: `--models` and --hosts select")
	fmt.Println("different steps, and running one never implies running the other.")
	fmt.Println()
	fmt.Println("Host installation")
	fmt.Println("  mpm onboard --hosts install <target>...")
	fmt.Println()
	fmt.Println("  Targets: " + strings.Join(onboardHostLabels, ", "))
	fmt.Println()
	fmt.Println("  There is deliberately no `--hosts auto`. A host that is present on the")
	fmt.Println("  machine but absent from your target list is skipped, not installed.")
	fmt.Println("  Detection is read-only and never installs: onboarding may write into a")
	fmt.Println("  host only when you name that host and approve the mutation.")
	fmt.Println()
	fmt.Println("  Unattended use (--yes) needs both explicit targets and explicit approval.")
	fmt.Println("  Rollback is per adapter, not atomic — re-run each adapter's")
	fmt.Println("  install.sh --uninstall, one at a time. There is no single-command undo.")
	fmt.Println()
	fmt.Println("What this command will not do")
	fmt.Println("  · contact a model endpoint, download a model, or call a paid API")
	fmt.Println("  · recommend or auto-select a model, or name a preferred provider")
	fmt.Println("  · install a framework integration merely because that framework is present")
	fmt.Println("  · overwrite a working configuration")
	fmt.Println("  · repair a drifted managed block unless you pass --repair")
	fmt.Println()
	fmt.Println("Related")
	fmt.Println("  mpm config          interactive model configuration wizard")
	fmt.Println("  mpm config validate structural validation")
	fmt.Println("  mpm doctor          live probes and graded health checks")
	fmt.Println("  mpm tour            cognitive-verb walkthrough (teaches MPM; does not configure it)")
	fmt.Println()
}
