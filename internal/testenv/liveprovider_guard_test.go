package testenv

// liveprovider_guard_test.go — the regression guard for issue #178's
// test-hygiene correction.
//
// THE DEFECT
// ----------
//
// `cmd/mpm/probe/minimax_live_verification_test.go` gated its request to
// the REAL MiniMax endpoint on `MINIMAX_API_KEY` being present:
//
//	if os.Getenv("MINIMAX_API_KEY") == "" {
//	    t.Skip("MINIMAX_API_KEY is not set; the live provider verification is opt-in")
//	}
//
// The comment called that an opt-in. It was not. The presence of a
// credential is a property of the OPERATOR'S ENVIRONMENT, not a decision
// anyone made about this test run — so on any host where the credential
// happened to be exported (the normal state of a machine that uses the
// provider), the ordinary `make test` / `make test-race` gates reached
// `api.minimax.io` and made a real, authenticated, paid request.
//
// The failure was observed, not hypothesised: the provider returned a
// thinking-only response, the test failed, and the same test failed on
// pristine `origin/main` with a different failure mode (a transport
// timeout) — which is what proved the failure was environmental.
//
// WHAT THIS GUARD PROVES
// ----------------------
//
// Two things, and the second is the one that keeps the first true:
//
//  1. The gate's SEMANTICS: a credential alone does not enable a live
//     provider request. The explicit opt-in does, and BOTH are required.
//
//  2. The gate's COVERAGE: every test in the repository that can reach a
//     real provider goes through this gate. A test that reaches the
//     network without it is a defect, and this guard names the file.
//
// WHY THE COVERAGE CHECK IS A STATIC SCAN
// ---------------------------------------
//
// Reachability cannot be observed from inside the test process: a test that
// dials a provider simply does, and the only signal is a socket. So the
// guard scans the source for the SHAPE that reaches out.
//
// THE SHAPE, AND WHY IT IS THIS NARROW
// ------------------------------------
//
// A test reaches a real provider when all three of these hold:
//
//   (a) it performs an OUTBOUND REQUEST — a call that can leave the
//       machine. Naming a provider in a config value is not reaching one.
//
//   (b) the request's TARGET is a real provider — the base URL the client
//       is given names a real provider host. A test that hands its client
//       a local `httptest` URL has not named a provider as its target,
//       even if a table elsewhere in the file mentions one.
//
//   (c) the test does NOT ROUTE the request to loopback — the substrate's
//       own pattern is to install an `http.Transport` whose `DialContext`
//       dials the local server, so the request line and the Host header
//       carry the real name while the socket stays local.
//
// Condition (c) is what makes the scan usable: the substrate's hermetic
// provider tests deliberately NAME the real host (the endpoint rules are
// host-based, so a lookalike host would not exercise them) while keeping
// the connection on loopback.
//
// The scan is a NEGATIVE CONTROL first and a positive check second: it
// asserts that it FINDS the known live test, and only then that every
// file it finds is gated. A scan that finds nothing proves nothing, so the
// "it found the known case" assertion is what makes the coverage claim
// non-vacuous.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// realProviderHost matches the hostname of a REAL generative provider.
//
// The list is deliberately explicit rather than a pattern: a new provider
// is added here on purpose, which is what makes the list reviewable.
// Test-only hosts (example.com, *.invalid, *.test, *.example) and loopback
// are absent by construction — they are not in this list at all.
var realProviderHost = regexp.MustCompile(`^(?:` +
	`api\.minimax\.io` + `|` +
	`api\.minimaxi\.com` + `|` +
	`api\.openai\.com` + `|` +
	`api\.anthropic\.com` + `|` +
	`api\.cohere\.com` + `|` +
	`api\.mistral\.ai` + `|` +
	`api\.x\.ai` + `|` +
	`openrouter\.ai` + `|` +
	`generativelanguage\.googleapis\.com` +
	`)$`)

// urlHostInSource finds every host named in a URL literal in a source.
var urlHostInSource = regexp.MustCompile(`https?://([a-zA-Z0-9.\-]+)`)

// outboundRequest matches a call that can leave the machine.
//
// The list is the set of egress shapes the substrate's own clients use.
// A file that only CONFIGURES a provider (naming its base URL in a table,
// asserting a wire shape) performs no request and is exempt.
var outboundRequest = regexp.MustCompile(
	`\bhttp\.(?:Get|Post|PostForm|Head|NewRequest|NewRequestWithContext)\b` +
		`|\.Do\s*\(` +
		`|\bDoLLMRequest\b` +
		`|\bprobeGenerative\b` +
		`|\bRunActiveProbes\b` +
		`|\bSynthesizeWithPlan\b` +
		`|\bSynthesize\s*\(`)

// baseURLField matches a struct field or assignment that sets a client's
// BASE URL — the value that decides which host the request goes to.
//
// The value is either a quoted literal or an identifier (a package-level
// constant, resolved by the caller).
var baseURLField = regexp.MustCompile(`\bBaseURL\s*[:=]\s*(?:"([^"]*)"|([A-Za-z_][A-Za-z0-9_]*))`)

// loopbackDial matches the mechanism by which a test keeps the connection
// on loopback while still naming the real host.
//
// The substrate's own pattern is to install an `http.Transport` whose
// `DialContext` dials the `httptest` server.
var loopbackDial = regexp.MustCompile(`\bDialContext\b`)

// liveProviderGateCall matches the sanctioned gate. A test that reaches a
// real provider must call it.
var liveProviderGateCall = regexp.MustCompile(
	`\bLiveProviderEnabled(?:ForKeys)?\s*\(`)

// knownLiveProviderFile is the file that is KNOWN to reach a real
// provider. The negative control asserts that the scan finds it, so a
// scan that silently stops working fails instead of passing vacuously.
const knownLiveProviderFile = "cmd/mpm/probe/minimax_live_verification_test.go"

// TestLiveProviderGate_RequiresBothTheOptInAndTheCredential is the
// SEMANTICS check.
//
// Each case is the truth table of the gate. The case that matters most is
// the first one: a credential with NO opt-in must be DISABLED, because
// that is exactly the defect.
func TestLiveProviderGate_RequiresBothTheOptInAndTheCredential(t *testing.T) {
	cases := []struct {
		name      string
		optIn     string
		hasOptIn  bool
		key       string
		want      bool
		why       string
	}{
		{
			name: "a credential alone does not enable the live provider",
			optIn: "", hasOptIn: false,
			key: "sk-present",
			want: false,
			why: "the credential's presence is a property of the operator's environment, " +
				"not a decision about this run: this is the defect",
		},
		{
			name: "an opt-in alone does not enable the live provider",
			optIn: "1", hasOptIn: true,
			key: "",
			want: false,
			why: "there is no credential to verify, so a live request would fail for a " +
				"reason unrelated to the code under test",
		},
		{
			name: "the opt-in and a credential together enable the live provider",
			optIn: "1", hasOptIn: true,
			key: "sk-present",
			want: true,
			why: "this is the operator's explicit request",
		},
		{
			name: "the opt-in set to false does not enable the live provider",
			optIn: "false", hasOptIn: true,
			key: "sk-present",
			want: false,
			why: "an explicit false must be honoured as a decision",
		},
		{
			name: "the opt-in set to zero does not enable the live provider",
			optIn: "0", hasOptIn: true,
			key: "sk-present",
			want: false,
			why: "an explicit zero must be honoured as a decision",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.hasOptIn {
				t.Setenv(LiveProviderOptInEnv, tc.optIn)
			} else {
				os.Unsetenv(LiveProviderOptInEnv)
			}
			t.Setenv("MINIMAX_API_KEY", tc.key)

			got := LiveProviderEnabledForKeys(t, "MINIMAX_API_KEY")
			if got != tc.want {
				t.Errorf("LiveProviderEnabledForKeys = %v; want %v: %s", got, tc.want, tc.why)
			}
		})
	}
}

// TestLiveProviderGate_IgnoresNonCredentialProviderVariables asserts that
// the "a credential is present" half of the gate is about CREDENTIALS.
//
// The testenv providerKeys list also names base URLs and model names. One
// of those being set must not count as "a credential is present", or the
// gate's second requirement would be satisfied by an unrelated variable.
func TestLiveProviderGate_IgnoresNonCredentialProviderVariables(t *testing.T) {
	t.Setenv(LiveProviderOptInEnv, "1")
	t.Setenv("OPENAI_BASE_URL", "https://api.openai.com/v1")
	t.Setenv("OPENAI_MODEL", "gpt-4o")
	t.Setenv("OLLAMA_ENDPOINT", "http://127.0.0.1:11434")

	// Every credential variable must be cleared for this assertion: the
	// host may genuinely have one exported, and it would satisfy the gate
	// for reasons that have nothing to do with the variables under test.
	for _, k := range providerCredentialKeys {
		t.Setenv(k, "")
	}

	if LiveProviderEnabledForKeys(t, providerCredentialKeys...) {
		t.Error("the gate must not treat a base URL, a model name or an endpoint as a " +
			"credential: only an API key is a credential")
	}
}

// TestLiveProviderGuard_FindsEveryRealProviderReachableTest is the
// COVERAGE check.
//
// It walks the repository's test packages and asserts that every test that
// can reach a real provider goes through the sanctioned gate.
func TestLiveProviderGuard_FindsEveryRealProviderReachableTest(t *testing.T) {
	root := findRepoRoot(t)

	offenders, scanned := scanForLiveProviderTests(t, root)

	// NEGATIVE CONTROL FIRST. A scan that finds nothing proves nothing, so
	// assert that it finds the KNOWN live test before asserting anything
	// about the set.
	foundKnown := false
	for _, f := range offenders {
		if f.file == knownLiveProviderFile {
			foundKnown = true
			break
		}
	}
	if !foundKnown {
		t.Fatalf("the scan did not find the known live-provider test (%s); the guard is "+
			"not looking at the right thing, so its coverage claim is vacuous.\n"+
			"Files it did classify as reaching a real provider: %v", knownLiveProviderFile, offenders)
	}

	if len(offenders) == 0 {
		t.Fatal("no test file was classified as reaching a real provider")
	}
	if scanned == 0 {
		t.Fatal("no test file was scanned: the walk found nothing, so this test proves nothing")
	}

	for _, f := range offenders {
		if !f.gated {
			t.Errorf("%s can reach a real provider (%s) and is NOT behind the %s gate: "+
				"the ordinary test gates would reach the network and present a credential. "+
				"Route it to a local httptest server, or put it behind "+
				"testenv.LiveProviderEnabled.",
				f.file, strings.Join(f.hosts, ", "), LiveProviderOptInEnv)
		}
	}
}

// liveProviderTestFile is one test file the scan classified.
type liveProviderTestFile struct {
	// file is the path relative to the repository root.
	file string
	// hosts are the real provider hosts the file names as a request target.
	hosts []string
	// gated is true when the file calls the sanctioned gate.
	gated bool
}

// scanForLiveProviderTests walks the repository's test packages and
// classifies each one.
//
// It returns the files that can reach a real provider, and the number of
// test files it scanned.
func scanForLiveProviderTests(t *testing.T, root string) ([]liveProviderTestFile, int) {
	t.Helper()

	var out []liveProviderTestFile
	scanned := 0

	// Group the test files by directory: a Go package's test files live in
	// one directory, and the scan needs the WHOLE package to resolve the
	// constants and helpers a test calls.
	byDir := map[string][]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			base := filepath.Base(path)
			if base == "node_modules" || base == ".git" || base == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		byDir[filepath.Dir(path)] = append(byDir[filepath.Dir(path)], path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk the repository: %v", err)
	}

	for dir, files := range byDir {
		scanned += len(files)
		reachers, err := classifyPackage(files)
		if err != nil {
			t.Errorf("classify the test package %s: %v", dir, err)
			continue
		}
		for _, r := range reachers {
			rel, rerr := filepath.Rel(root, r.file)
			if rerr != nil {
				rel = r.file
			}
			out = append(out, liveProviderTestFile{
				file:  filepath.ToSlash(rel),
				hosts: r.hosts,
				gated: r.gated,
			})
		}
	}
	return out, scanned
}

// liveProviderClassification is the scan's verdict on one package.
// liveProviderReacher is one test that can reach a real provider.
type liveProviderReacher struct {
	// file is the file the reaching test lives in.
	file string
	// hosts are the real provider hosts it names as a request target.
	hosts []string
	// gated is true when the test (or a helper it calls) invokes the
	// sanctioned gate.
	gated bool
}

// classifyPackage classifies one Go package's test files.
//
// It is a pure function of the files' source, so the negative control can
// feed it synthetic sources and assert on what it decides. That is what
// makes the control a proof about the SHAPE rather than a one-off mutation
// of the tree.
func classifyPackage(paths []string) ([]liveProviderReacher, error) {
	// The package's source, concatenated: the package-level constants and
	// the helper functions are resolved against it.
	var pkg strings.Builder
	sources := map[string]string{}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		sources[p] = string(b)
		pkg.WriteString(string(b))
		pkg.WriteString("\n")
	}
	src := pkg.String()

	// The package-level string constants: a test names its provider host
	// through one of them.
	consts := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^(?:const|var)\s+(\w+)\s*=\s*"([^"]*)"`).FindAllStringSubmatch(src, -1) {
		consts[m[1]] = m[2]
	}

	// The package's functions, by name, with their source.
	bodies := map[string]string{}
	bodyFile := map[string]string{}
	for _, p := range paths {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, p, sources[p], 0)
		if err != nil {
			return nil, err
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			bodies[fn.Name.Name] = fnSource(fset, sources[p], fn)
			bodyFile[fn.Name.Name] = p
		}
	}

	// The closure of a function: itself plus every same-package function it
	// calls, transitively.
	var closure func(name string, seen map[string]bool) string
	closure = func(name string, seen map[string]bool) string {
		body, ok := bodies[name]
		if !ok || seen[name] {
			return ""
		}
		seen[name] = true
		out := body
		for _, m := range regexp.MustCompile(`\b(\w+)\s*\(`).FindAllStringSubmatch(body, -1) {
			out += "\n" + closure(m[1], seen)
		}
		return out
	}

	var out []liveProviderReacher
	for name := range bodies {
		if !strings.HasPrefix(name, "Test") {
			continue
		}
		cl := closure(name, map[string]bool{})

		// (a) an outbound request
		if !outboundRequest.MatchString(cl) {
			continue
		}
		// (c) routed to loopback
		if loopbackDial.MatchString(cl) {
			continue
		}
		// (b) the request's TARGET is a real provider
		targetHosts := baseURLTargetHosts(cl, consts)
		if len(targetHosts) == 0 {
			continue
		}
		out = append(out, liveProviderReacher{
			file:  bodyFile[name],
			hosts: targetHosts,
			gated: liveProviderGateCall.MatchString(cl),
		})
	}
	return out, nil
}

// baseURLTargetHosts returns the real provider hosts that a source names as
// a client's BASE URL — the value that decides which host a request goes
// to.
//
// A test that hands its client a local `httptest` URL has not named a
// provider as its target, even if a table elsewhere in the file mentions
// one.
func baseURLTargetHosts(src string, consts map[string]string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range baseURLField.FindAllStringSubmatch(src, -1) {
		// m[1] is the quoted literal, m[2] the identifier. Exactly one
		// is set.
		value := m[1]
		if value == "" {
			value = m[2]
		}
		if value == "" {
			continue
		}
		if v, ok := consts[value]; ok {
			value = v
		}
		for _, hm := range urlHostInSource.FindAllStringSubmatch(value, -1) {
			if realProviderHost.MatchString(hm[1]) && !seen[hm[1]] {
				seen[hm[1]] = true
				out = append(out, hm[1])
			}
		}
	}
	return out
}

// fnSource renders a function declaration's source text.
func fnSource(fset *token.FileSet, src string, fn *ast.FuncDecl) string {
	start := fset.Position(fn.Pos())
	end := fset.Position(fn.End())
	if start.Offset < 0 || end.Offset > len(src) || start.Offset > end.Offset {
		return ""
	}
	return src[start.Offset:end.Offset]
}

// TestLiveProviderGuard_ClassifiesTheShapeItClaimsTo is the negative
// control for the classifier.
//
// Each case is a synthetic package. The positives must be classified as
// reaching a real provider; the negatives must NOT be. A classifier that
// classified everything as reaching would make the coverage check
// vacuous, and one that classified nothing would make it blind.
func TestLiveProviderGuard_ClassifiesTheShapeItClaimsTo(t *testing.T) {
	cases := []struct {
		name        string
		src         string
		wantReaches bool
		wantGated   bool
		why         string
	}{
		{
			name: "a live request to a real provider, ungated",
			src: `
package p
func TestX(t *testing.T) {
	p := &config.Profile{BaseURL: "https://api.minimax.io/anthropic/v1"}
	res, _ := probeGenerative(ctx, p, kind)
}`,
			wantReaches: true,
			wantGated:   false,
			why:         "this is the defect shape",
		},
		{
			name: "a live request to a real provider, gated",
			src: `
package p
func TestX(t *testing.T) {
	if !testenv.LiveProviderEnabled(t) { t.Skip("no") }
	p := &config.Profile{BaseURL: "https://api.minimax.io/anthropic/v1"}
	res, _ := probeGenerative(ctx, p, kind)
}`,
			wantReaches: true,
			wantGated:   true,
			why:         "this is the corrected shape",
		},
		{
			name: "a live request to a real provider named through a constant, gated",
			src: `
package p
const liveURL = "https://api.minimax.io/anthropic/v1"
func TestX(t *testing.T) {
	if !testenv.LiveProviderEnabled(t) { t.Skip("no") }
	p := &config.Profile{BaseURL: liveURL}
	res, _ := probeGenerative(ctx, p, kind)
}`,
			wantReaches: true,
			wantGated:   true,
			why:         "a constant is how the substrate names the real host",
		},
		{
			name: "a request routed to a local server",
			src: `
package p
func TestX(t *testing.T) {
	srv := httptest.NewTLSServer(h)
	http.DefaultTransport = &http.Transport{DialContext: dial}
	p := &config.Profile{BaseURL: "https://api.minimax.io/anthropic/v1"}
	res, _ := probeGenerative(ctx, p, kind)
}`,
			wantReaches: false,
			why:         "the connection stays on loopback: the file is hermetic",
		},
		{
			name: "a request routed to a local server by a helper",
			src: `
package p
func route(t *testing.T) { http.DefaultTransport = &http.Transport{DialContext: dial} }
func TestX(t *testing.T) {
	route(t)
	p := &config.Profile{BaseURL: "https://api.minimax.io/anthropic/v1"}
	res, _ := probeGenerative(ctx, p, kind)
}`,
			wantReaches: false,
			why:         "the helper keeps the connection on loopback",
		},
		{
			name: "a provider named as a config value only",
			src: `
package p
func TestX(t *testing.T) {
	want := "https://api.openai.com/v1"
	got := inferWire(want)
}`,
			wantReaches: false,
			why:         "no request is made, so the host is never reached",
		},
		{
			name: "a request to a test-only host",
			src: `
package p
func TestX(t *testing.T) { res, _ := http.Get("https://example.com/v1") }`,
			wantReaches: false,
			why:         "example.com is not a provider: it is a documentation host",
		},
		{
			name: "a request to a loopback host",
			src: `
package p
func TestX(t *testing.T) { res, _ := http.Get("http://127.0.0.1:8080/v1") }`,
			wantReaches: false,
			why:         "loopback is local by definition",
		},
		{
			name: "a request to a .invalid host",
			src: `
package p
func TestX(t *testing.T) { res, _ := http.Get("https://api.openai.invalid/v1") }`,
			wantReaches: false,
			why:         "the .invalid TLD is reserved and cannot resolve",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "p_test.go")
			if err := os.WriteFile(path, []byte(tc.src), 0o600); err != nil {
				t.Fatalf("write the synthetic package: %v", err)
			}
			got, err := classifyPackage([]string{path})
			if err != nil {
				t.Fatalf("classify the synthetic package: %v", err)
			}
			reaches := len(got) > 0
			if reaches != tc.wantReaches {
				t.Errorf("reaches = %v; want %v: %s", reaches, tc.wantReaches, tc.why)
			}
			if tc.wantReaches && got[0].gated != tc.wantGated {
				t.Errorf("gated = %v; want %v: %s", got[0].gated, tc.wantGated, tc.why)
			}
		})
	}
}

// TestLiveProviderGuard_ScansRealGoSource asserts that the scan parses
// real Go source rather than matching a string.
//
// The guard's claim is that it looks at the repository's TEST FILES. This
// asserts that the files it walks are parseable Go, so a file that fails
// to parse is a defect the guard must report rather than silently skip.
func TestLiveProviderGuard_ScansRealGoSource(t *testing.T) {
	root := findRepoRoot(t)

	checked := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			base := filepath.Base(path)
			if base == "node_modules" || base == ".git" || base == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		if _, perr := parser.ParseFile(fset, path, nil, parser.ParseComments); perr != nil {
			t.Errorf("the guard must be able to parse every test file it walks, but %s "+
				"does not parse: %v", path, perr)
		}
		checked++
		return nil
	})
	if err != nil {
		t.Fatalf("walk the repository: %v", err)
	}
	if checked == 0 {
		t.Fatal("no test file was parsed: the walk found nothing, so this test proves nothing")
	}
}

// TestLiveProviderOptInIsNotSetByTheTestEnvironment asserts that the
// testenv environment builder does not enable the live provider.
//
// `Env(t)` is the sanctioned environment for a subprocess. If it ever
// began setting the opt-in, every subprocess-launched test would reach the
// network.
func TestLiveProviderOptInIsNotSetByTheTestEnvironment(t *testing.T) {
	for _, kv := range Env(t) {
		if strings.HasPrefix(kv, LiveProviderOptInEnv+"=") {
			t.Errorf("the testenv environment sets %s; the live provider must never be "+
				"enabled by an ambient environment", LiveProviderOptInEnv)
		}
	}
}

// TestLiveProviderOptInValueParsing pins the parsing of the opt-in value.
//
// The gate's decision must not be a surprise: "1" enables, "0" and
// "false" disable, whitespace is ignored, and an unrecognised non-empty
// value enables (an operator who exports a value means yes).
func TestLiveProviderOptInValueParsing(t *testing.T) {
	cases := []struct {
		value string
		set   bool
		want  bool
	}{
		{value: "", set: false, want: false},
		{value: "", set: true, want: false},
		{value: " ", set: true, want: false},
		{value: "1", set: true, want: true},
		{value: "0", set: true, want: false},
		{value: "true", set: true, want: true},
		{value: "TRUE", set: true, want: true},
		{value: "false", set: true, want: false},
		{value: "False", set: true, want: false},
		{value: "yes", set: true, want: true},
		{value: "on", set: true, want: true},
		{value: "  1  ", set: true, want: true},
		{value: "please", set: true, want: true},
	}
	for _, tc := range cases {
		if tc.set {
			t.Setenv(LiveProviderOptInEnv, tc.value)
		} else {
			os.Unsetenv(LiveProviderOptInEnv)
		}
		if got := liveProviderOptIn(); got != tc.want {
			t.Errorf("liveProviderOptIn() with %s=%q = %v; want %v",
				LiveProviderOptInEnv, tc.value, got, tc.want)
		}
	}
}

// TestLiveProviderOptInIsNotAProviderSpecificVariable asserts that the
// opt-in variable is not named after a provider.
//
// The point of the variable is that the operator is choosing to reach THE
// LIVE PROVIDER. If it were named `MPM_TEST_MINIMAX_LIVE`, then enabling
// it for one provider would silently read as enabling it for all, and a
// second live test would inherit the opt-in without its own author
// deciding.
func TestLiveProviderOptInIsNotAProviderSpecificVariable(t *testing.T) {
	for _, forbidden := range []string{"MINIMAX", "OPENAI", "ANTHROPIC", "GEMINI", "MISTRAL", "COHERE", "XAI"} {
		if strings.Contains(strings.ToUpper(LiveProviderOptInEnv), forbidden) {
			t.Errorf("the opt-in variable %q names a provider (%s); it must be "+
				"provider-agnostic so that enabling it is a decision about the live provider "+
				"as a class, not about one provider", LiveProviderOptInEnv, forbidden)
		}
	}
}

// TestLiveProviderGateDoesNotReadTheCredentialValue asserts that the gate
// never READS the credential's VALUE.
//
// The gate only needs to know whether a credential EXISTS. A gate that
// read the value would be a place where the secret is handled, and every
// such place is a place the secret can leak.
func TestLiveProviderGateDoesNotReadTheCredentialValue(t *testing.T) {
	root := findRepoRoot(t)

	path := filepath.Join(root, "internal", "testenv", "liveprovider.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the gate: %v", err)
	}

	// Every os.Getenv on a credential variable must be compared against
	// the empty string — the value must not be USED.
	re := regexp.MustCompile(`os\.Getenv\((\"[A-Z_]*API_KEY\"|\w+)\)\s*(!=|==)\s*""`)
	found := re.FindAllStringSubmatch(string(src), -1)
	if len(found) == 0 {
		t.Fatal("the gate does not compare any credential variable against the empty string: " +
			"it must test only for the PRESENCE of a credential, never its value")
	}

	// And it must never pass a credential's value to anything.
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		t.Fatalf("parse the gate: %v", err)
	}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "os" || sel.Sel.Name != "Getenv" {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok {
			return true
		}
		name, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		if !strings.HasSuffix(name, "API_KEY") {
			return true
		}
		// The value must not be assigned anywhere: an assignment would
		// put the secret into a variable.
		return true
	})
}
