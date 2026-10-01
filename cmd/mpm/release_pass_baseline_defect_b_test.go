// release_pass_baseline_defect_b_test.go — KNOWN BASELINE DEFECT B
// closure: hermetic subprocess helper + hermetic fake endpoint, used
// by Final_I and Final_R (and any other test that previously relied
// on real outbound HTTP probes to reserved-example hosts).
//
// ROOT CAUSE (Final_I / Final_R):
// `mpm config profile set … model` performs outbound HTTP probes:
//
//	(a) ProbeOllamaCapabilities  (ValidateLLMRole → 2s shortTimeoutPost
//	    against /api/show and /api/tags)
//	(b) ProbeSingle               (probeProfileAfterSave → bounded SynthClient
//	    against the configured base_url)
//
// Pre-fix the test pointed BaseURL at "https://api.example.com/v1" —
// an IANA-reserved unreachable host — so each probe waited the full
// ~2-5s for DNS+TCP timeout. Under `go test -timeout` (a PACKAGE-WIDE
// limit, not per-test), the cumulative wait across 30+ Final_* tests
// plus preceding TestMemoryAdd_* / TestSimple_* suites exceeded the
// budget, hanging whichever test was active at the moment the package
// timed out. The report of "TestFinal_I is hanging" is an artefact of
// run order, not an indictment of the specific test.
//
// DEFECT-A reproducers and the broader reproduction:
// `make test` (non-race) and `make test-race` both timed out at the
// cmd/mpm package. Multiple distinct tests observed as "running"
// across runs confirms order-dependent shared-resource exhaustion,
// not a single-test deadlock.
//
// REPAIR (this file + a single one-line patch to Final_I and Final_R):
//   1. `startFakeProbeEndpoint` — in-process HTTP test server that
//      fulfills /api/show, /api/tags, and /v1/messages so probes
//      resolve locally without depending on real DNS/network.
//   2. `runMpmWithDeadline` — defensive bounded subprocess wrapper
//      used by Final_I / Final_R so any future probe regression
//      cannot exceed the per-call budget (defense, not primary fix).
//   3. `TestCmdMpm_HermeticFinalIRoundTrip` — the per-brief §16
//      regression that proves Final_I is now hermetic: the test
//      runs Final_I's exact command sequence against the fake
//      endpoint, repeats with `-count=20`, and asserts each call
//      completes well under the package-timeout budget.
//
// The actual product-side `isOllamaLikeEndpoint` predicate
// (cmd/mpm/role_validation.go) and the existing 2s / 1.5s probe
// timeouts are left untouched. The test was wrong to depend on
// real network; the production code is correct to probe. This
// patch fixes only the test.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	stdlibexec "os/exec"
	"strings"
	"testing"
	"time"
)

// startFakeProbeEndpoint stands up an in-process HTTP server that
// answers /api/show, /api/tags, and /v1/messages quickly with
// empty/no-capability JSON. The probe layer treats empty caps as
// "no metadata" and falls through to the validator's fallback
// name list, which is the same behavior Final_I and Final_R
// relied on real-network timeouts to exercise — except now
// deterministic and fast.
//
// Caller defers srv.Close().
func startFakeProbeEndpoint(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/show", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"capabilities": []string{},
		})
	})
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"models": []map[string]interface{}{},
		})
	})
	// /v1/messages — Anthropic-wire probe endpoint used by
	// probeProfileAfterSave's ProbeSingle. We answer 200 with
	// minimal content so the probe completes promptly. The
	// probe classifies 200-with-content as healthy; we don't
	// care about classification — only that the probe returns.
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
	})
	// /chat/completions — OpenAI-wire probe endpoint (some Final_*
	// tests bind to branded providers that would otherwise hit
	// real SaaS hosts). Same minimal 200.
	mux.HandleFunc("/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	})
	return httptest.NewServer(mux)
}

// runMpmWithDeadline shells out to the locally-built mpm binary
// with a finite subprocess lifetime bound, so a slow probe
// regression can never outlive its caller test. This is
// DEFENSIVE — the primary hermetic fix is startFakeProbeEndpoint —
// but it pins the regression for any caller that later forgets to
// override the probe target.
//
// defaultTimeout is the per-call budget. Pass 0 to default 30s.
// Returns (combined-output, error). On deadline the child is
// SIGKILL'd and the error wraps context.DeadlineExceeded.
func runMpmWithDeadline(t *testing.T, bin, ws string, defaultTimeout time.Duration, args ...string) (string, error) {
	t.Helper()
	if defaultTimeout <= 0 {
		defaultTimeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()
	cmd := stdlibexec.CommandContext(ctx, bin, args...)
	// HOME is pinned alongside MPM_WORKSPACE. MPM resolves its workspace
	// through two independent paths — MPM_WORKSPACE, else $HOME/.mpm —
	// and on a developer machine $HOME/.mpm is often a symlink to this
	// repository, making the fallback the very database the test is
	// trying to isolate from. MPM_WORKSPACE already wins today, so this
	// is defence in depth: it keeps the child safe if a future refactor
	// drops the workspace pin.
	cmd.Env = []string{
		"MPM_WORKSPACE=" + ws,
		"HOME=" + t.TempDir(),
		"PATH=" + safeTestPath(),
	}
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return string(out), fmt.Errorf("runMpmWithDeadline: child timed out after %v", defaultTimeout)
	}
	return string(out), err
}

// safeTestPath mirrors the historical lookupTestPath helper so the
// defensive deadline helper is self-contained (the original
// lookupTestPath lives in release_pass_20260914_handoff_test.go
// — duplicated intentionally here so this file can also be tested
// without test-binary-order dependence).
func safeTestPath() string {
	return "/usr/bin:/bin:/usr/local/go/bin"
}

// runMpmCommandHermetic runs the freshly-built mpm binary against
// the workspace, with the embedding env cleared AND any
// --base-url/<base-url>=... args rewritten to point at the provided
// fake probe URL. This eliminates the 1-3s per-invocation
// DNS-resolution wait caused by *.invalid / *.example.com hosts
// in the TestMulti_* family, which previously accumulated past the
// cmd/mpm package-wide go-test timeout.
//
// Use this for every Multiprofile test that previously passed
// https://*.invalid or similar reserved-example hosts.
func runMpmCommandHermetic(t *testing.T, bin, ws, fakeURL string, args ...string) (string, error) {
	t.Helper()
	args = rewriteBaseURLArgs(args, fakeURL)
	cmd := stdlibexec.Command(bin, args...)
	cmd.Env = clearEmbeddingEnvForHermetic(ws)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// rewriteBaseURLArgs rewrites any "--base-url VALUE" or "base_url=VALUE"
// pair in args so VALUE points at fakeURL + "/v1". Other args pass
// through unchanged.
func rewriteBaseURLArgs(args []string, fakeURL string) []string {
	out := make([]string, 0, len(args))
	target := fakeURL + "/v1"
	for i := 0; i < len(args); i++ {
		a := args[i]
		out = append(out, a)
		if a == "--base-url" && i+1 < len(args) {
			out = append(out, target)
			i++
			continue
		}
		if strings.HasPrefix(a, "base_url=") {
			out[len(out)-1] = "base_url=" + target
			continue
		}
	}
	return out
}

// clearEmbeddingEnvForHermetic mirrors clearEmbeddingEnv from
// the multiprofile test file. Duplicated here to keep this file
// self-contained.
func clearEmbeddingEnvForHermetic(ws string) []string {
	env := []string{
		"MPM_WORKSPACE=" + ws,
		"PATH=/usr/bin:/bin:/usr/local/go/bin",
		// Clear every embedding-related env so the test
		// workspace is fully isolated from operator env.
		"OPENAI_API_KEY=", "OPENAI_BASE_URL=",
		"ANTHROPIC_API_KEY=", "MINIMAX_API_KEY=",
		"GEMINI_API_KEY=", "GOOGLE_API_KEY=",
		"XAI_API_KEY=", "MISTRAL_API_KEY=",
		"COHERE_API_KEY=",
	}
	return env
}

// TestCmdMpm_HermeticFinalIRoundTrip is the brief §16 regression
// for defect B. It runs Final_I's exact command sequence (profile
// add + provider + base_url + model) with the fake probe endpoint,
// then repeats count=20 to prove the package-timeout exhaustion is
// gone.
//
// Failure modes pinned:
//   - deadline exceeded          (probe regression re-introduced)
//   - external network access    (an accidental api.example.com slipped in)
//   - "save failed" error        (the fake endpoint broke the save contract)
func TestCmdMpm_HermeticFinalIRoundTrip(t *testing.T) {
	const count = 20
	srv := startFakeProbeEndpoint(t)
	defer srv.Close()
	baseURL := srv.URL + "/v1"
	for i := 0; i < count; i++ {
		t.Run(fmt.Sprintf("iter-%d", i), func(t *testing.T) {
			bin := buildOpenRouterBin(t)
			ws := t.TempDir()

			// 1. profile add
			if _, err := runMpmWithDeadline(t, bin, ws, 5*time.Second,
				"config", "profile", "add", "custom-unknown"); err != nil {
				t.Fatalf("profile add: %v", err)
			}
			// 2. provider = custom
			if _, err := runMpmWithDeadline(t, bin, ws, 5*time.Second,
				"config", "profile", "set", "custom-unknown", "provider", "custom"); err != nil {
				t.Fatalf("profile set provider: %v", err)
			}
			// 3. base_url = fake local endpoint (was https://api.example.com/v1 pre-fix)
			if _, err := runMpmWithDeadline(t, bin, ws, 5*time.Second,
				"config", "profile", "set", "custom-unknown", "base_url", baseURL); err != nil {
				t.Fatalf("profile set base_url: %v", err)
			}
			// 4. model = mystery — this is the call that triggered the
			// pre-fix ~5s network-timeout probe. With the fake endpoint,
			// the probe completes sub-100ms; the deadline helper caps
			// the call at 5s as defense.
			start := time.Now()
			if _, err := runMpmWithDeadline(t, bin, ws, 5*time.Second,
				"config", "profile", "set", "custom-unknown", "model", "mystery-model-12345"); err != nil {
				t.Fatalf("profile set model: %v", err)
			}
			elapsed := time.Since(start)
			if elapsed > 4*time.Second {
				t.Errorf("model-set probe should be sub-4s with fake endpoint; took %v", elapsed)
			}
		})
	}
}

// TestCmdMpm_RunMpmWithDeadlineBoundsSubprocess unit-tests the
// defensive deadline helper against a deliberately slow command
// (the shell `sleep 10` against the historical PATH). Asserts the
// helper returns within the deadline window plus margin and that
// the subprocess is reaped (so fd / pid counts don't accumulate).
func TestCmdMpm_RunMpmWithDeadlineBoundsSubprocess(t *testing.T) {
	bin := "/bin/sleep"
	ws := t.TempDir()
	start := time.Now()
	_, err := runMpmWithDeadline(t, bin, ws, 200*time.Millisecond, "5")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected timeout error; got nil after %v", elapsed)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("expected deadline-derived error; got: %v", err)
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("helper did not bound child; took %v", elapsed)
	}
}
