package capability

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestFakeDryRunner_ReturnsCanned(t *testing.T) {
	fake := &FakeDryRunner{
		Results: []DryRunResult{
			{Pass: true, Stdout: "ok"},
			{Pass: false, Reason: "boom", Stderr: "traceback..."},
		},
	}

	r1, err := fake.Run(context.Background(), "bash", "echo hi")
	if err != nil {
		t.Fatalf("call 1 err: %v", err)
	}
	if !r1.Pass || r1.Stdout != "ok" {
		t.Errorf("call 1 wrong: %+v", r1)
	}

	r2, err := fake.Run(context.Background(), "python", "print(1)")
	if err != nil {
		t.Fatalf("call 2 err: %v", err)
	}
	if r2.Pass || r2.Reason != "boom" {
		t.Errorf("call 2 wrong: %+v", r2)
	}

	// Default: pass when queue is empty.
	r3, err := fake.Run(context.Background(), "jq", ".")
	if err != nil {
		t.Fatalf("call 3 err: %v", err)
	}
	if !r3.Pass {
		t.Errorf("default should pass, got: %+v", r3)
	}

	if len(fake.Calls) != 3 {
		t.Errorf("expected 3 recorded calls, got %d", len(fake.Calls))
	}
}

func TestFakeDryRunner_ReturnsCannedError(t *testing.T) {
	fake := &FakeDryRunner{Errors: []error{errors.New("infrastructure boom")}}
	_, err := fake.Run(context.Background(), "bash", "echo")
	if err == nil || !strings.Contains(err.Error(), "infrastructure boom") {
		t.Errorf("expected canned error, got %v", err)
	}
}

func TestBwrapDryRunner_ConfigDefaults(t *testing.T) {
	r := NewBwrapDryRunner(BwrapConfig{})
	if r.cfg.BwrapBinary == "" {
		t.Error("bwrap binary default not applied")
	}
	if r.cfg.BashBinary == "" {
		t.Error("bash binary default not applied")
	}
	if r.cfg.PythonBinary == "" {
		t.Error("python binary default not applied")
	}
	if r.cfg.JqBinary == "" {
		t.Error("jq binary default not applied")
	}
	if r.cfg.Timeout == 0 {
		t.Error("timeout default not applied")
	}
	if r.cfg.AllowNetAccess {
		t.Error("default should deny network access")
	}
	if !r.cfg.MountReadOnlyRoot {
		t.Error("default should mount root read-only")
	}
}

func TestBwrapDryRunner_MissingBwrapIsAFailure(t *testing.T) {
	// Point at a binary that definitely doesn't exist; the
	// Forge must see Pass=false (fail-closed). The runner
	// reports a failed result rather than an error because
	// "binary missing" is the same shape as "binary present
	// but exited non-zero" — both are "the proposal did not
	// pass the sandbox check" and the Forge rejects both.
	cfg := DefaultBwrapConfig()
	cfg.BwrapBinary = "/this/binary/does/not/exist/anywhere"
	r := NewBwrapDryRunner(cfg)
	result, err := r.Run(context.Background(), "bash", "echo hi")
	if err != nil {
		t.Fatalf("unexpected err (binary-missing should surface as result, not error): %v", err)
	}
	if result.Pass {
		t.Errorf("missing bwrap should fail, got: %+v", result)
	}
	if result.Reason == "" {
		t.Error("failure should have a non-empty reason")
	}
}

func TestBwrapDryRunner_InterpreterDispatch(t *testing.T) {
	// We don't actually invoke bwrap; we just inspect the
	// (interpreter, args) tuple the runner builds. The dispatch
	// logic is what spec §3.2 step 9 cares about.
	r := NewBwrapDryRunner(BwrapConfig{})

	cases := []struct {
		lang       string
		wantInterp string
		wantArgs   []string
	}{
		{"bash", "/bin/bash", []string{"<src>"}},
		{"python", "/usr/bin/python3", []string{"<src>"}},
		{"jq", "/usr/bin/jq", []string{"-f", "<src>", "null"}},
	}
	for _, tc := range cases {
		t.Run(tc.lang, func(t *testing.T) {
			interp, args, err := r.interpreterFor(tc.lang, "<src>")
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if interp != tc.wantInterp {
				t.Errorf("interp = %q, want %q", interp, tc.wantInterp)
			}
			if len(args) != len(tc.wantArgs) {
				t.Errorf("args length = %d, want %d", len(args), len(tc.wantArgs))
			}
			for i := range args {
				if args[i] != tc.wantArgs[i] {
					t.Errorf("args[%d] = %q, want %q", i, args[i], tc.wantArgs[i])
				}
			}
		})
	}
}

func TestBwrapDryRunner_UnknownLanguage(t *testing.T) {
	r := NewBwrapDryRunner(BwrapConfig{})
	_, err := r.Run(context.Background(), "ruby", "puts 'hi'")
	if err == nil {
		t.Error("expected error for unknown language")
	}
	if !strings.Contains(err.Error(), "unknown language") {
		t.Errorf("error should mention unknown language, got: %v", err)
	}
}

func TestBwrapDryRunner_ContextCancellationSurfacesAsTimeout(t *testing.T) {
	// A 1ms timeout against a non-existent binary should fail
	// (whether the cause is binary-missing or true timeout is
	// ambiguous here, but either way the result is Pass=false).
	// We can't easily test the success path because that would
	// require a real bwrap + a real interpreter. This test
	// just guards against the runner hanging on a bad binary.
	cfg := DefaultBwrapConfig()
	cfg.BwrapBinary = "/this/binary/does/not/exist"
	cfg.Timeout = 1 * time.Millisecond
	r := NewBwrapDryRunner(cfg)
	result, _ := r.Run(context.Background(), "bash", "echo")
	if result.Pass {
		t.Errorf("missing/timeout bwrap should fail, got: %+v", result)
	}
}
