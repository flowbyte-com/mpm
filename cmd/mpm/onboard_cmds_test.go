package main

// onboard_cmds_test.go — tests for `mpm onboard`.
//
// Isolation contract, applied to every test here:
//
//	HOME          a per-test temporary directory, so the host inspector
//	              reads a fixture tree instead of the operator's real
//	              ~/.claude, ~/.config/opencode, ~/.pi, ~/.openclaw
//	MPM_WORKSPACE a per-test temporary workspace
//	providers     every base-URL env var points at 127.0.0.1:1, so a
//	              command that tried to reach a model would fail loudly
//	              rather than reaching a paid API
//
// Nothing in this file reaches the network, downloads a model, or
// touches a live database.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core/config"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// onboardFixture is one isolated installation in one known state.
type onboardFixture struct {
	home      string
	workspace string
	bin       string
}

// newOnboardFixture builds an isolated installation. The caller states
// what kind it is; every test in this file starts from one of the four
// states the brief requires: fresh, partially configured, fully
// configured, or models-skipped.
func newOnboardFixture(t *testing.T, kind string) onboardFixture {
	t.Helper()
	root := t.TempDir()
	f := onboardFixture{
		home:      filepath.Join(root, "home"),
		workspace: filepath.Join(root, "workspace"),
	}
	for _, d := range []string{f.home, f.workspace} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	f.bin = buildTestBinary(t)

	switch kind {
	case "fresh":
		// No config file, no database, no host integration. This is
		// what a clean installation looks like.

	case "partial":
		// A generative profile exists but nothing is bound to it and no
		// embedding profile exists — the "partially configured" state.
		//
		// The profile is named "default" on purpose. ResolveProfile
		// falls back to Profiles["default"], so a profile called
		// anything else leaves every component unresolved and the
		// fixture would actually be testing the FRESH case.
		f.writeConfig(t, `{
  "profiles": {"default": {"provider": "custom", "model": "some-model"}},
  "components": {}
}`)

	case "full":
		// Both roles configured and bound, including an explicit
		// component binding for every known component.
		f.writeConfig(t, `{
  "profiles": {
    "memory":  {"provider": "custom", "model": "some-model",  "base_url": "http://127.0.0.1:9/v1"},
    "embed":   {"provider": "openai-compatible", "model": "some-embed", "base_url": "http://127.0.0.1:9/v1"}
  },
  "components": {
    "memory":    "memory",
    "critic":    "memory",
    "scheduler": "memory",
    "embedding": "embed"
  }
}`)

	case "models-skipped":
		// Embedding explicitly disabled by the operator. This is the
		// supported "skip" state and must NOT read as a defect.
		f.writeConfig(t, `{
  "profiles": {"default": {"provider": "custom", "model": "some-model"}},
  "components": {"embedding": "disabled"}
}`)

	case "broken":
		// A component bound to a profile that does not exist, plus a
		// profile missing its model. Exercises the "needs attention"
		// path and the structural validator.
		f.writeConfig(t, `{
  "profiles": {"memory": {"provider": "custom"}},
  "components": {"memory": "nonexistent-profile"}
}`)

	default:
		t.Fatalf("unknown fixture kind %q", kind)
	}
	return f
}

// writeConfig places an mpm_config.json where config.ConfigPath()
// will look for it.
//
// ConfigPath is GetWorkspace()/mpm_config.json, and GetWorkspace
// prefers $MPM_WORKSPACE over $HOME/.mpm. So under this fixture the
// config lives in the WORKSPACE, not in HOME. Writing it to HOME
// would silently produce a "fresh installation" fixture for every
// test that meant to be partially or fully configured — the config
// would never be read, and the assertions would pass for the wrong
// reason.
func (f onboardFixture) writeConfig(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(f.workspace, "mpm_config.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	// Pin the expectation: if this ever stops holding, the fixtures
	// are not testing what they claim to.
	t.Setenv("MPM_WORKSPACE", f.workspace)
	if got := config.ConfigPath(); got != path {
		t.Fatalf("config.ConfigPath() = %q, want %q — the fixture config would "+
			"never be read", got, path)
	}
}

// repoRoot locates the checkout so the host inspector is reachable
// without the test depending on the working directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "agent_installation",
			"scripts", "check_installed_managed_blocks.py")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not locate the repository root from the test working directory")
	return ""
}

// run invokes the binary and returns its STDOUT only.
//
// stdout only, deliberately: `--json` promises a single parseable
// document on stdout, and several tests decode it. Combining the
// streams would let a migration warning on stderr corrupt the
// document and produce a failure about JSON syntax rather than about
// the contract that was actually broken.
func (f onboardFixture) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(f.bin, args...)
	cmd.Dir = f.workspace
	cmd.Env = append(os.Environ(),
		"HOME="+f.home,
		"MPM_WORKSPACE="+f.workspace,
		"MPM_NO_AUTO_INIT=1",
		"MPM_REPO_ROOT="+repoRoot(t),
		// Closed ports: a provider call fails instead of succeeding,
		// which is how these tests prove no provider call happens.
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

// report runs onboard in JSON mode and decodes the envelope.
func (f onboardFixture) report(t *testing.T, args ...string) onboardReport {
	t.Helper()
	full := append([]string{"onboard"}, args...)
	full = append(full, "--json")
	out, err := f.run(t, full...)
	if err != nil {
		// A non-zero exit is legitimate (attention present); only an
		// undecodable envelope is a test failure.
		var ee *exec.ExitError
		if !errorAs(err, &ee) {
			t.Fatalf("onboard %v: %v\n%s", args, err, out)
		}
	}
	var rep onboardReport
	if jerr := json.Unmarshal([]byte(out), &rep); jerr != nil {
		t.Fatalf("decode onboard JSON (exit %v): %v\n---output---\n%s", err, jerr, out)
	}
	return rep
}

// warmUp materialises whatever the FIRST invocation creates — the
// SQLite database, mostly — so a mutation test can digest the tree
// after initialisation and still catch every write that follows.
//
// Without this, every non-mutation test would fail on the database's
// own creation and prove nothing.
func (f onboardFixture) warmUp(t *testing.T) {
	t.Helper()
	f.run(t, "onboard", "report")
}

func errorAs(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}

// rowFor finds a report row by subject.
func rowFor(rep onboardReport, subject string) (onboardRow, bool) {
	for _, group := range [][]onboardRow{rep.Installation, rep.Readiness, rep.Models, rep.Hosts} {
		for _, r := range group {
			if r.Subject == subject {
				return r, true
			}
		}
	}
	return onboardRow{}, false
}

// ---------------------------------------------------------------------------
// Registration
// ---------------------------------------------------------------------------

// TestOnboardIsRegistered pins all four router touchpoints from
// docs/ONBOARD.md §10. A missing one leaves the command partly broken
// — registered but undispatchable, or dispatchable but invisible in
// help — which is exactly the class of defect a single assertion on
// `r.Commands` cannot catch.
func TestOnboardIsRegistered(t *testing.T) {
	r := NewRouter()
	spec, ok := r.Commands["onboard"]
	if !ok {
		t.Fatal("onboard missing from the command registry")
	}
	if spec.Name != "onboard" {
		t.Errorf("registry Name = %q, want \"onboard\"", spec.Name)
	}
	if spec.MinArgs != 0 {
		t.Errorf("registry MinArgs = %d, want 0 (bare `mpm onboard` is valid)", spec.MinArgs)
	}
	if !commandsWithSubcommandDispatch["onboard"] {
		t.Error("onboard missing from commandsWithSubcommandDispatch: a step name after " +
			"`help` would be swallowed as data instead of printing the family's page")
	}
}

// TestOnboardRegistryEntryIsOneLine guards the gen-cli constraint: the
// README generator parses r.Commands with go/ast and silently misses a
// multi-line entry, so the rule is a comment that gets reformatted.
func TestOnboardRegistryEntryIsOneLine(t *testing.T) {
	path, err := filepath.Abs("router.go")
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.Contains(line, `"onboard":`) && strings.Contains(line, "Description:") {
			if strings.Contains(strings.TrimSpace(line), "Description:") &&
				strings.Count(line, "{Name:") != 1 {
				t.Fatalf("onboard registry entry spans multiple lines:\n%s", line)
			}
			return
		}
	}
	t.Fatal("no single-line onboard registry entry found in router.go")
}

// ---------------------------------------------------------------------------
// Flag and scope contract
// ---------------------------------------------------------------------------

func TestParseOnboardArgs(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantScope onboardScope
		wantErr   bool
		wantDry   bool
	}{
		{name: "bare is the full run", args: nil, wantScope: onboardScopeFull},
		{name: "report subcommand", args: []string{"report"}, wantScope: onboardScopeReport, wantDry: true},
		{name: "models subcommand", args: []string{"models"}, wantScope: onboardScopeModels},
		{name: "hosts subcommand", args: []string{"hosts"}, wantScope: onboardScopeHosts},
		{name: "--models flag", args: []string{"--models"}, wantScope: onboardScopeModels},
		{name: "--hosts flag", args: []string{"--hosts"}, wantScope: onboardScopeHosts},
		{name: "--dry-run", args: []string{"--dry-run"}, wantScope: onboardScopeFull, wantDry: true},
		{name: "--check aliases dry-run", args: []string{"--check"}, wantScope: onboardScopeFull, wantDry: true},
		{name: "--yes", args: []string{"--yes"}, wantScope: onboardScopeFull},
		{
			name:    "models and hosts are mutually exclusive",
			args:    []string{"--models", "--hosts"},
			wantErr: true,
		},
		{
			name:    "subcommand and flag of another scope conflict",
			args:    []string{"models", "--hosts"},
			wantErr: true,
		},
		{
			name:      "repeating one scope is not an error",
			args:      []string{"--models", "models"},
			wantScope: onboardScopeModels,
		},
		{
			name:    "unknown flag is rejected, never ignored",
			args:    []string{"--bootstrap"},
			wantErr: true,
		},
		{
			name:    "unknown positional is rejected",
			args:    []string{"install-everything"},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := parseOnboardArgs(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseOnboardArgs(%v) succeeded; an unrecognised token must be an error "+
						"rather than a silently discarded flag", tc.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseOnboardArgs(%v): %v", tc.args, err)
			}
			if opts.scope != tc.wantScope {
				t.Errorf("scope = %q, want %q", opts.scope, tc.wantScope)
			}
			if opts.dryRun != tc.wantDry {
				t.Errorf("dryRun = %v, want %v", opts.dryRun, tc.wantDry)
			}
		})
	}
}

// TestOnboardGatesReflectImplementedSteps pins the scope of the write
// gates against what has actually been implemented.
//
// Stage 1 shipped both gates closed. Stage 2 opens the MODEL gate only;
// host installation is stage 3 and its gate must stay closed until then.
// This test is the thing that must be updated deliberately when a stage
// lands — which is the point: the gates are named predicates, not a
// scattering of conditionals to be audited. The per-gate assertions
// live in onboard_models_test.go; this one pins the aggregate so that a
// new gate cannot be opened without somebody reading why.
func TestOnboardGatesReflectImplementedSteps(t *testing.T) {
	opts, err := parseOnboardArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !opts.mutateAllowed() {
		t.Fatal("mutateAllowed() is false, but stage 2 implements the guided " +
			"model step. A false here would make the step unreachable.")
	}
	if opts.mutateHostsAllowed() {
		t.Fatal("mutateHostsAllowed() is true, but host installation is stage 3 " +
			"and requires an explicit target plus mutation approval.")
	}
}

// ---------------------------------------------------------------------------
// Report contract across installation states
// ---------------------------------------------------------------------------

// TestOnboardReport_FreshInstallation is the clean-install case.
//
// Expected: installation and readiness rows exist, generative is
// reported as NOT configured (attention — it is required), embedding as
// deliberately skipped (NOT attention — it is optional), and no host is
// claimed to be integrated.
func TestOnboardReport_FreshInstallation(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and runs a subprocess")
	}
	f := newOnboardFixture(t, "fresh")
	rep := f.report(t, "report")

	for _, r := range rep.Installation {
		if r.Subject == "MPM binary" && r.State != onboardStateOperational {
			t.Errorf("MPM binary state = %q, want %q", r.State, onboardStateOperational)
		}
	}

	// Generative is required. On a fresh install every known component
	// must resolve to nothing and be reported as needing attention.
	var sawGenerativeAttention bool
	for _, r := range rep.Models {
		if strings.HasPrefix(r.Subject, "Generative · ") {
			if r.State == onboardStateAttention {
				sawGenerativeAttention = true
				if len(r.Lines) == 0 {
					t.Errorf("%s: attention row states no consequences; the operator must "+
						"be told what skipping generative actually costs", r.Subject)
				}
			}
		}
	}
	if !sawGenerativeAttention {
		t.Error("fresh install: no generative component reported as needing attention, " +
			"but generative is REQUIRED — absence must not read as healthy")
	}

	// Embedding is optional: skipped, never attention.
	emb, ok := rowFor(rep, "Embedding")
	if !ok {
		t.Fatal("fresh install: report has no Embedding row")
	}
	if emb.State != onboardStateSkipped {
		t.Errorf("Embedding state = %q, want %q — absence of an optional capability "+
			"must not be reported as a defect", emb.State, onboardStateSkipped)
	}
	if len(emb.Lines) == 0 {
		t.Error("Embedding row states no consequences; skipping must be informed")
	}

	// No host may be claimed integrated on a machine with no host
	// integration installed.
	for _, h := range rep.Hosts {
		if h.State == onboardStateOperational {
			t.Errorf("host %q reported operational on a fresh fixture home", h.Subject)
		}
	}
}

// TestOnboardReport_PartiallyConfigured is the middle state: a
// profile exists, nothing is bound to it.
func TestOnboardReport_PartiallyConfigured(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and runs a subprocess")
	}
	f := newOnboardFixture(t, "partial")
	rep := f.report(t, "report")

	// The unconfigured profile still resolves through the default
	// fallback chain, so components resolve — the report must not
	// claim attention it does not have.
	for _, r := range rep.Models {
		if strings.HasPrefix(r.Subject, "Generative · ") && r.State == onboardStateAttention {
			t.Errorf("%s reported as needing attention; a single profile with no "+
				"explicit binding resolves through the default chain and works", r.Subject)
		}
	}
}

// TestOnboardReport_FullyConfigured is the happy path.
func TestOnboardReport_FullyConfigured(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and runs a subprocess")
	}
	f := newOnboardFixture(t, "full")
	rep := f.report(t, "report")

	emb, ok := rowFor(rep, "Embedding")
	if !ok {
		t.Fatal("no Embedding row")
	}
	if emb.State != onboardStateOperational {
		t.Errorf("Embedding state = %q (%s), want %q",
			emb.State, emb.Detail, onboardStateOperational)
	}

	sv, ok := rowFor(rep, "Structural validation")
	if !ok {
		t.Fatal("no Structural validation row")
	}
	if sv.State != onboardStateOperational {
		t.Errorf("Structural validation state = %q (%s), want %q",
			sv.State, sv.Detail, onboardStateOperational)
	}
}

// TestOnboardReport_ModelsSkipped is the deliberate-skip case.
//
// The operator disabled embedding. That is a decision, so it belongs
// under skipped with consequences — never under attention, and never
// phrased as a failure.
func TestOnboardReport_ModelsSkipped(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and runs a subprocess")
	}
	f := newOnboardFixture(t, "models-skipped")
	rep := f.report(t, "report")

	emb, ok := rowFor(rep, "Embedding")
	if !ok {
		t.Fatal("no Embedding row")
	}
	if emb.State != onboardStateSkipped {
		t.Errorf("Embedding state = %q (%s), want %q — an operator's explicit "+
			"decision to disable embedding is not a defect",
			emb.State, emb.Detail, onboardStateSkipped)
	}
	if !strings.Contains(strings.ToLower(emb.Detail), "disabled") {
		t.Errorf("Embedding detail = %q, does not say the operator disabled it", emb.Detail)
	}
	for _, a := range rep.Attention {
		if strings.Contains(strings.ToLower(a), "embedding") &&
			strings.Contains(strings.ToLower(a), "not configured") {
			t.Errorf("a deliberately disabled embedding model was listed under "+
				"Needs attention: %q", a)
		}
	}
}

// TestOnboardReport_BrokenConfiguration is the error-reporting case:
// a dangling binding and a profile missing its model must both reach
// the operator with a remedy.
func TestOnboardReport_BrokenConfiguration(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and runs a subprocess")
	}
	f := newOnboardFixture(t, "broken")
	rep := f.report(t, "report")

	if len(rep.Attention) == 0 {
		t.Fatal("a dangling component binding produced no attention entries")
	}
	sv, ok := rowFor(rep, "Structural validation")
	if !ok {
		t.Fatal("no Structural validation row")
	}
	if sv.State != onboardStateAttention {
		t.Errorf("Structural validation state = %q, want %q for a broken config",
			sv.State, onboardStateAttention)
	}
	if sv.Remedy == "" {
		t.Error("structural validation row names no remedy")
	}
}

// TestOnboardReport_RepeatRunIsStable is the idempotency case.
//
// Onboarding has no first-run state, so a second report over the same
// state must be identical. Anything else means the run recorded
// something, which is a mutation wearing a report's clothes.
func TestOnboardReport_RepeatRunIsStable(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and runs subprocesses")
	}
	f := newOnboardFixture(t, "full")

	first := f.report(t, "report")
	second := f.report(t, "report")

	a, _ := json.Marshal(normaliseRep(first))
	b, _ := json.Marshal(normaliseRep(second))
	if string(a) != string(b) {
		t.Errorf("repeat report differs:\n first: %s\nsecond: %s", a, b)
	}
}

// normaliseRep drops fields that legitimately vary between runs
// (uptime strings, the resolved binary path) so the comparison targets
// the report's actual content.
func normaliseRep(rep onboardReport) onboardReport {
	for i := range rep.Readiness {
		if rep.Readiness[i].Subject == "Scheduler running" {
			rep.Readiness[i].Detail = "<normalised>"
		}
	}
	return rep
}

// ---------------------------------------------------------------------------
// Non-mutation
// ---------------------------------------------------------------------------

// TestOnboardHelpNeverMutates covers every help path, including the
// bare-token forms. Help is a read operation; if it can write, it is a
// command wearing a help page's name.
func TestOnboardHelpNeverMutates(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and runs subprocesses")
	}
	f := newOnboardFixture(t, "full")
	f.warmUp(t)

	before := digestOnboardTree(t, f)

	for _, args := range [][]string{
		{"onboard", "--help"},
		{"onboard", "-h"},
		{"onboard", "help"},
		{"onboard", "models", "--help"},
		{"onboard", "hosts", "--help"},
		{"onboard", "report", "--help"},
		{"help", "onboard"},
	} {
		out, err := f.run(t, args...)
		if err != nil {
			t.Errorf("%v: exit %v\n%s", args, err, out)
			continue
		}
		if !strings.Contains(out, "mpm onboard") {
			t.Errorf("%v printed no onboard help:\n%s", args, out)
		}
	}

	assertUnchanged(t, "help", before, digestOnboardTree(t, f))
}

// TestOnboardInspectionNeverMutates is the load-bearing safety test.
//
// Every inspection path — report, --dry-run, --check, --models,
// --hosts, and the JSON variants — must leave the fixture byte-
// identical. This is the guarantee that "inspection is read-only" is
// a fact rather than an intention, and it covers the config file and
// the database, not just the workspace.
func TestOnboardInspectionNeverMutates(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and runs subprocesses")
	}

	for _, kind := range []string{"fresh", "partial", "full", "models-skipped", "broken"} {
		t.Run(kind, func(t *testing.T) {
			f := newOnboardFixture(t, kind)
			f.warmUp(t)
			before := digestOnboardTree(t, f)

			for _, args := range [][]string{
				{"onboard", "report"},
				{"onboard", "report", "--json"},
				{"onboard", "--dry-run"},
				{"onboard", "--dry-run", "--json"},
				{"onboard", "--check"},
				{"onboard", "--models"},
				{"onboard", "--models", "--dry-run"},
				{"onboard", "--hosts"},
				{"onboard", "--yes"},
			} {
				out, err := f.run(t, args...)
				if err != nil {
					var ee *exec.ExitError
					if !errorAs(err, &ee) {
						t.Fatalf("%v: %v\n%s", args, err, out)
					}
				}
			}

			assertUnchanged(t, kind, before, digestOnboardTree(t, f))
		})
	}
}

// onboardTreeDigest hashes the fixture HOME and WORKSPACE, excluding
// SQLite's transient WAL/SHM siblings (SQLite creates them on open and
// removes them on clean close; their presence is not a data write).
type onboardDigest struct {
	home string
	work string
}

func digestOnboardTree(t *testing.T, f onboardFixture) onboardDigest {
	t.Helper()
	return onboardDigest{
		home: digestDirTree(t, f.home),
		work: digestDirTree(t, f.workspace),
	}
}

func digestDirTree(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		name := filepath.Base(p)
		if info.IsDir() {
			return nil
		}
		if strings.HasSuffix(name, "-wal") || strings.HasSuffix(name, "-shm") {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		b.WriteString(rel)

		// A SQLite file is digested by CONTENT, not by bytes.
		//
		// Measured on this database: two consecutive `mpm onboard
		// report` runs produce byte-different .db files while every
		// row of every table is identical — SQLite repacks pages and
		// moves the freelist on a read-mostly workload. A byte digest
		// would therefore report a mutation on every single run and
		// the test would be pure noise.
		//
		// digestDB (help_mutation_regression_test.go) hashes each
		// base table row-wise and skips the FTS5 shadow tables, which
		// is the granularity at which "did this write?" is a
		// meaningful question.
		if strings.HasSuffix(name, ".db") {
			b.WriteString(digestDB(t, p))
			b.WriteByte(0)
			return nil
		}

		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		b.Write(data)
		b.WriteByte(0)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return b.String()
}

func assertUnchanged(t *testing.T, label string, before, after onboardDigest) {
	t.Helper()
	if before.home != after.home {
		t.Errorf("%s: HOME changed.\n before: %q\n  after: %q",
			label, before.home, after.home)
	}
	if before.work != after.work {
		t.Errorf("%s: workspace changed.\n before: %q\n  after: %q",
			label, before.work, after.work)
	}
}

// ---------------------------------------------------------------------------
// Honesty contract
// ---------------------------------------------------------------------------

// TestOnboardNeverClaimsUnverifiedRuntime is the guard the brief names
// explicitly: registration is not proof of loading.
//
// A host whose managed block is present and current still has UNKNOWN
// runtime behaviour. The report must carry that caveat every time, and
// must name OpenCode specifically, because that adapter's runtime load
// has never been confirmed.
func TestOnboardNeverClaimsUnverifiedRuntime(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and runs a subprocess")
	}
	f := newOnboardFixture(t, "full")
	rep := f.report(t, "report")

	var caveat string
	for _, s := range rep.NotVerified {
		lower := strings.ToLower(s)
		if strings.Contains(lower, "runtime") && strings.Contains(lower, "managed block") {
			caveat = s
			break
		}
	}
	if caveat == "" {
		t.Fatal("report carries no runtime-loading caveat; a present managed block " +
			"shows what the host is told, not that it loaded MPM")
	}
	if !strings.Contains(strings.ToLower(caveat), "opencode") {
		t.Errorf("runtime caveat does not name OpenCode, whose runtime load is "+
			"unconfirmed: %q", caveat)
	}
	if !strings.Contains(strings.ToLower(caveat), "registration is not proof") &&
		!strings.Contains(strings.ToLower(caveat), "not proof of loading") {
		t.Errorf("runtime caveat does not state that registration is not proof: %q", caveat)
	}
}

// TestOnboardDoesNotClaimProbeCapability pins the second honesty rule:
// onboarding must not present itself as having verified embedding
// reachability, because verifying it is exactly what the removed probe
// did and this command must not restore it.
func TestOnboardDoesNotClaimProbeCapability(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and runs a subprocess")
	}
	f := newOnboardFixture(t, "full")
	rep := f.report(t, "report")

	var reachabilityCaveat bool
	for _, s := range rep.NotVerified {
		lower := strings.ToLower(s)
		if strings.Contains(lower, "reachab") || strings.Contains(lower, "authentication") {
			reachabilityCaveat = true
		}
	}
	if !reachabilityCaveat {
		t.Error("report does not disclaim live endpoint reachability; onboarding " +
			"does not probe providers, and saying so is what keeps it honest")
	}

	// The retired probe must stay retired: onboarding must never route
	// a caller into `mpm config detect-embedding`.
	out, _ := f.run(t, "onboard", "report")
	if strings.Contains(out, "detect-embedding") {
		t.Error("onboard output references `mpm config detect-embedding`; that probe " +
			"was removed in v0.1-final and onboarding must not send operators to it")
	}
}

// TestOnboardReport_HostAbsenceIsNotAFailure covers the missing-framework
// case. A host that is not installed is a supported state reported as
// skipped — not an error that fails onboarding.
func TestOnboardReport_HostAbsenceIsNotAFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and runs a subprocess")
	}
	f := newOnboardFixture(t, "full")
	out, err := f.run(t, "onboard", "report")
	var ee *exec.ExitError
	if err != nil && !errorAs(err, &ee) {
		t.Fatalf("onboard report: %v", err)
	}
	if !strings.Contains(out, "Deliberately skipped") {
		t.Errorf("report has no Deliberately skipped section; with no host "+
			"integration installed that section must exist and name them:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "not installed") && strings.Contains(line, "!") {
			t.Errorf("a not-installed host was marked as needing attention: %q", line)
		}
	}
}

// ---------------------------------------------------------------------------
// Provider agnosticism
// ---------------------------------------------------------------------------

// TestOnboardIsProviderAgnostic is the mechanical form of the guiding
// principle: MPM explains its requirements; the operator chooses the
// models.
//
// SCOPE, and it matters. The ban applies to names ONBOARD ITSELF
// introduces. It does NOT forbid echoing a vendor or model name back
// to an operator who configured one — that is the whole point of a
// report, and suppressing it would make onboarding useless. So this
// test runs against the FRESH fixture, where no provider is
// configured at all: any vendor name in that output came from
// onboard's own strings, which is the violation.
//
// Recommendation language is a separate, unconditional ban: no
// onboarding output may ever tell an operator what to pick.
func TestOnboardIsProviderAgnostic(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and runs subprocesses")
	}

	// Vendors and model families MPM removed from its UI in v0.1-final.
	vendors := []string{
		"ollama", "openai", "anthropic", "gemini", "minilm", "nomic",
		"bge-", "gte-", "e5-", "qwen", "llama", "mistral", "cohere",
		"sentence-transformers", "openrouter",
	}
	// Language that would make MPM a chooser rather than an explainer.
	prescriptive := []string{
		"we recommend", "recommended model", "suggested model",
		"best model", "default model", "you should use",
		"we suggest", "choose the", "pick the",
	}

	f := newOnboardFixture(t, "fresh") // nothing configured anywhere
	for _, args := range [][]string{
		{"onboard", "report"},
		{"onboard", "report", "--json"},
		{"onboard", "--dry-run"},
		{"onboard", "--models"},
		{"onboard", "--hosts"},
		{"onboard", "--help"},
	} {
		out, err := f.run(t, args...)
		if err != nil {
			var ee *exec.ExitError
			if !errorAs(err, &ee) {
				t.Fatalf("%v: %v", args, err)
			}
		}
		lower := strings.ToLower(out)
		for _, b := range vendors {
			if strings.Contains(lower, b) {
				t.Errorf("%v emitted the vendor name %q on an installation with nothing "+
					"configured. Onboard explains requirements; the operator chooses "+
					"models.\n---output---\n%s", args, b, out)
			}
		}
	}

	// The prescriptive ban applies to EVERY fixture, including
	// configured ones: a report must never tell the operator what to pick.
	for _, kind := range []string{"fresh", "partial", "full", "models-skipped"} {
		g := newOnboardFixture(t, kind)
		for _, args := range [][]string{{"onboard", "report"}, {"onboard", "--help"}} {
			out, err := g.run(t, args...)
			if err != nil {
				var ee *exec.ExitError
				if !errorAs(err, &ee) {
					t.Fatalf("%v %v: %v", kind, args, err)
				}
			}
			lower := strings.ToLower(out)
			for _, b := range prescriptive {
				if strings.Contains(lower, b) {
					t.Errorf("%v %v emitted prescriptive language %q:\n%s",
						kind, args, b, out)
				}
			}
		}
	}
}

// TestOnboardEchoesOperatorConfiguration is the counterweight to the
// test above, and exists so the ban cannot be "fixed" by suppressing
// information.
//
// An operator who configured an embedding profile must see that name
// in the report. Hiding it would satisfy provider agnosticism while
// destroying the command's purpose.
func TestOnboardEchoesOperatorConfiguration(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and runs a subprocess")
	}
	f := newOnboardFixture(t, "full")
	out, _ := f.run(t, "onboard", "report")

	for _, want := range []string{"some-model", "some-embed", "embed"} {
		if !strings.Contains(out, want) {
			t.Errorf("report omits the operator's own configured value %q.\n"+
				"The report must echo what is configured; provider agnosticism "+
				"forbids CHOOSING, not reporting.\n---output---\n%s", want, out)
		}
	}
}

// TestOnboardSourceHasNoProviderCall is the structural companion: no
// model endpoint is contacted. The provider env vars point at a closed
// port in every fixture, so a call would fail — but a call that
// succeeded from a default endpoint would not, so the check is also
// made against the source.
func TestOnboardSourceHasNoProviderCall(t *testing.T) {
	body, err := os.ReadFile("onboard_cmds.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(body)

	// The only exec.Command in this file must be the host inspector,
	// which is documented read-only. Any net/http import, or any
	// provider-shaped call, would be a violation.
	if strings.Contains(src, `"net/http"`) {
		t.Error("onboard_cmds.go imports net/http; onboarding must not contact any endpoint")
	}
	for _, forbidden := range []string{
		"DefaultEmbeddingConfig",
		"RunActiveProbes",
		"GenerateEmbedding",
		"ChatCompletion",
	} {
		if strings.Contains(src, forbidden) {
			t.Errorf("onboard_cmds.go references %s; onboarding must not make "+
				"model calls of its own", forbidden)
		}
	}

	// Exactly one exec.Command, and it targets the read-only inspector.
	if n := strings.Count(src, "exec.Command("); n != 1 {
		t.Errorf("onboard_cmds.go contains %d exec.Command calls; exactly one is "+
			"expected — the read-only host inspector", n)
	}
}

// ---------------------------------------------------------------------------
// Host inventory parity
// ---------------------------------------------------------------------------

// TestOnboardHostInventoryMatchesPython prevents the Go host list from
// drifting away from the Python one that does the actual inspection.
//
// If they diverge, onboard would report a host the inspector never
// examined (or omit one it did), and the report would be confidently
// wrong. This test parses HOST_INSTALL_TARGETS out of the Python source
// and compares.
func TestOnboardHostInventoryMatchesPython(t *testing.T) {
	root := repoRoot(t)
	script := filepath.Join(root, "agent_installation", "scripts",
		"check_installed_managed_blocks.py")
	body, err := os.ReadFile(script)
	if err != nil {
		t.Skipf("host inspector not present at %s: %v", script, err)
	}

	pythonHosts := map[string]bool{}
	re := regexp.MustCompile(`"host"\s*:\s*"([a-z_]+)"`)
	for _, m := range re.FindAllStringSubmatch(string(body), -1) {
		pythonHosts[m[1]] = true
	}
	if len(pythonHosts) == 0 {
		t.Fatalf("could not parse HOST_INSTALL_TARGETS out of %s; if the Python "+
			"format changed, update this test and the Go table together", script)
	}

	got := map[string]bool{}
	for _, h := range onboardHostLabels {
		got[h] = true
	}
	for h := range pythonHosts {
		if !got[h] {
			t.Errorf("host %q is inspected by the Python reporter but is missing "+
				"from onboardHostLabels", h)
		}
	}
	for h := range got {
		if !pythonHosts[h] {
			t.Errorf("host %q is in onboardHostLabels but not in the Python "+
				"reporter's HOST_INSTALL_TARGETS", h)
		}
	}
}

// TestDisplayHostNameCoversInventory keeps the label mapping total, so
// no report row ever renders a raw snake_case identifier at an
// operator.
func TestDisplayHostNameCoversInventory(t *testing.T) {
	for _, label := range onboardHostLabels {
		got := displayHostName(label)
		if got == label {
			t.Errorf("displayHostName(%q) returned the raw label; the operator-facing "+
				"name must differ from the manifest key", label)
		}
		if strings.Contains(got, "_") {
			t.Errorf("displayHostName(%q) = %q still contains an underscore", label, got)
		}
	}
}

// ---------------------------------------------------------------------------
// JSON contract
// ---------------------------------------------------------------------------

// TestOnboardJSONNormalisesNilSlices pins the doctor JSON convention:
// a consumer must be able to iterate every collection unconditionally,
// so no field may ever serialise as null.
func TestOnboardJSONNormalisesNilSlices(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and runs a subprocess")
	}
	f := newOnboardFixture(t, "full")
	out, _ := f.run(t, "onboard", "report", "--json")

	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	for _, key := range []string{
		"installation", "readiness", "models", "hosts",
		"attention", "skipped", "plan", "not_verified", "not_available", "errors",
	} {
		body, ok := raw[key]
		if !ok {
			t.Errorf("JSON envelope has no %q key", key)
			continue
		}
		if strings.TrimSpace(string(body)) == "null" {
			t.Errorf("JSON key %q is null; normalise to [] so scripts can iterate "+
				"unconditionally", key)
		}
	}
}

// TestOnboardJSONAndHumanAgree — the two renderers must report the
// same facts. A divergence means one of them is lying.
func TestOnboardJSONAndHumanAgree(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and runs subprocesses")
	}
	f := newOnboardFixture(t, "full")

	rep := f.report(t, "report")
	human, _ := f.run(t, "onboard", "report")

	for _, r := range append(rep.Models, rep.Hosts...) {
		if !strings.Contains(human, r.Subject) {
			t.Errorf("JSON reports subject %q but the human report omits it:\n%s",
				r.Subject, human)
		}
	}
	for _, a := range rep.Attention {
		if !strings.Contains(human, a) {
			t.Errorf("attention entry %q is absent from the human report", a)
		}
	}
}

// ---------------------------------------------------------------------------
// Failure reporting
// ---------------------------------------------------------------------------

// TestOnboardUnknownFlagFails checks that a rejected request is a
// clean, actionable failure rather than a silent fallthrough to a full
// run — the exact defect Gap 1 was filed about.
func TestOnboardUnknownFlagFails(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and runs a subprocess")
	}
	f := newOnboardFixture(t, "full")
	before := digestOnboardTree(t, f)

	out, err := f.run(t, "onboard", "--install-everything")
	var ee *exec.ExitError
	if err == nil {
		t.Fatalf("unknown flag accepted:\n%s", out)
	}
	if !errorAs(err, &ee) || ee.ExitCode() == 0 {
		t.Errorf("exit code = %v, want non-zero", err)
	}
	if !strings.Contains(out, "unknown argument") {
		t.Errorf("no clear rejection message:\n%s", out)
	}
	assertUnchanged(t, "unknown flag", before, digestOnboardTree(t, f))
}

// TestOnboardMutualExclusionFailsCleanly — asking for two scopes must
// say which two, not silently pick one.
func TestOnboardMutualExclusionFailsCleanly(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and runs a subprocess")
	}
	f := newOnboardFixture(t, "full")
	out, err := f.run(t, "onboard", "--models", "--hosts")
	var ee *exec.ExitError
	if err == nil || !errorAs(err, &ee) {
		t.Fatalf("conflicting scopes accepted:\n%s", out)
	}
	if !strings.Contains(out, "--models") || !strings.Contains(out, "--hosts") {
		t.Errorf("rejection does not name both conflicting scopes:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// TestOnboardConfigPathIsolated guards the fixture itself: the tests
// must never read or write the operator's real configuration.
//
// Two properties are checked, because either one alone is insufficient.
// With MPM_WORKSPACE set the path must land inside the workspace; with
// it unset the path must fall back to the isolated HOME. A fixture that
// leaked into ~/.mpm would corrupt the developer's real substrate, so
// this test failing is a stop-the-line signal, not a papercut.
func TestOnboardConfigPathIsolated(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	workspace := filepath.Join(root, "workspace")

	t.Run("workspace takes priority", func(t *testing.T) {
		t.Setenv("HOME", home)
		t.Setenv("MPM_WORKSPACE", workspace)
		if got := config.ConfigPath(); !strings.HasPrefix(got, workspace) {
			t.Errorf("config.ConfigPath() = %q, want a path under the workspace %q",
				got, workspace)
		}
	})

	t.Run("falls back to isolated HOME", func(t *testing.T) {
		t.Setenv("HOME", home)
		// An explicitly empty value is not "unset" to os.Getenv, but
		// GetWorkspace tests for "", so this exercises the fallback.
		t.Setenv("MPM_WORKSPACE", "")
		if got := config.ConfigPath(); !strings.HasPrefix(got, home) {
			t.Errorf("config.ConfigPath() = %q, want a path under the isolated HOME %q",
				got, home)
		}
	})
}

// TestOnboardHostLabelsSorted guards report determinism: hosts must
// render in a stable order so two runs are diffable.
func TestOnboardHostLabelsSorted(t *testing.T) {
	// Sorted so two reports are diffable; the inspector's own output is
	// sorted independently, so this pins the Go table only.
	labels := append([]string{}, onboardHostLabels...)
	sorted := append([]string{}, labels...)
	sort.Strings(sorted)
	for i := range labels {
		if labels[i] != sorted[i] {
			t.Errorf("onboardHostLabels is not sorted: %v", labels)
			break
		}
	}
}

// ---------------------------------------------------------------------------
// Readiness write-gate
// ---------------------------------------------------------------------------

// TestReadinessReadOnlyNeverStartsTheScheduler guards the one
// difference that separates the two readiness entry points.
//
// ReadReadiness is allowed — deliberately, and only for `mpm` with no
// args — to run `systemctl --user start mpm-scheduler`. Starting a
// systemd unit is a mutation of machine state outside the workspace,
// so the onboarding inspection path must not be able to cause one even
// as a side effect of "just looking".
//
// The filesystem-digest tests above cannot catch this: a unit start
// changes systemd state, not a single file under HOME or the
// workspace. So the guard is made against the source.
func TestReadinessReadOnlyNeverStartsTheScheduler(t *testing.T) {
	body, err := os.ReadFile("readiness.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(body)

	start := strings.Index(src, "func checkSchedulerReadOnly()")
	if start < 0 {
		t.Fatal("checkSchedulerReadOnly not found in readiness.go")
	}
	end := strings.Index(src[start:], "\n}\n")
	if end < 0 {
		t.Fatal("could not delimit checkSchedulerReadOnly")
	}
	fn := src[start : start+end]

	// It may READ systemctl state (LoadState) to tell "not installed"
	// apart from "installed but stopped". It must never act on it.
	for _, forbidden := range []string{`"start"`, " --start", "restart", "enable", "disable"} {
		if strings.Contains(fn, forbidden) {
			t.Errorf("checkSchedulerReadOnly contains %q; the read-only readiness "+
				"path must never change systemd state", forbidden)
		}
	}
	if !strings.Contains(fn, "LoadState") {
		t.Error("checkSchedulerReadOnly does not read LoadState, so it cannot tell " +
			"'unit not installed' from 'unit installed and stopped' — the remedy " +
			"differs between the two")
	}

	// The mutating path must still exist and still auto-start. If a
	// future change made ReadReadiness read-only too, `mpm` with no
	// args would silently stop reviving a dead scheduler, which breaks
	// every wake-scheduled task.
	if !strings.Contains(src, "func checkSchedulerWithAutoStart()") {
		t.Error("checkSchedulerWithAutoStart was removed; `mpm` with no args must " +
			"retain its documented auto-start side effect")
	}
	if !strings.Contains(src, "func ReadReadiness(dm *mpminternal.DatabaseManager) []ReadinessItem {\n\treturn readReadiness(dm, true)") {
		t.Error("ReadReadiness no longer routes to the auto-starting variant")
	}
}

// TestOnboardNeverReportsASilentNoOp pins the stage-1 behaviour.
//
// The write gate is closed, so `mpm onboard` inspects, names problems
// and changes nothing. That is the honest state of this build — but a
// run that produces a full report and never says it could not act is
// indistinguishable, to an operator, from a run that did everything
// and found nothing to do. The report must close that gap by name.
func TestOnboardNeverReportsASilentNoOp(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and runs a subprocess")
	}
	for _, scope := range []string{"full", "models", "hosts"} {
		t.Run(scope, func(t *testing.T) {
			f := newOnboardFixture(t, "broken")
			args := []string{}
			switch scope {
			case "models":
				args = []string{"--models"}
			case "hosts":
				args = []string{"--hosts"}
			}

			rep := f.report(t, args...)
			if len(rep.NotAvailable) == 0 {
				t.Fatalf("scope %q produced no not_available entries; the report "+
					"must state that it cannot change anything", scope)
			}
			joined := strings.ToLower(strings.Join(rep.NotAvailable, " "))
			if scope != "hosts" && !strings.Contains(joined, "mpm config") {
				t.Errorf("model step does not point the operator at `mpm config`: %v",
					rep.NotAvailable)
			}

			human, _ := f.run(t, append([]string{"onboard"}, args...)...)
			if !strings.Contains(human, "Not available in this build") {
				t.Errorf("scope %q: the human report omits the not-available "+
					"section:\n%s", scope, human)
			}
		})
	}
}

// TestOnboardPureReportSaysNothingIsUnavailable keeps the sections
// honest in the other direction: `mpm onboard report` asked for no
// mutation, so it must not claim the build is missing anything.
func TestOnboardPureReportSaysNothingIsUnavailable(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and runs a subprocess")
	}
	f := newOnboardFixture(t, "broken")
	rep := f.report(t, "report")
	if len(rep.NotAvailable) != 0 {
		t.Errorf("`onboard report` reported %d not-available entries; that mode "+
			"was never going to mutate anything, so nothing is unavailable",
			len(rep.NotAvailable))
	}
}
