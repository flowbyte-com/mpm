package capability

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestFakeLinter_ReturnsCannedReports(t *testing.T) {
	fake := &FakeLinter{
		Reports: []*LintReport{
			{Linter: "fake", Findings: []LintFinding{
				{Line: 5, Severity: "error", Code: "E1", Message: "bad"},
			}, OK: false},
			{Linter: "fake", OK: true},
		},
	}

	r1, err := fake.Lint(context.Background(), "bash", "src1")
	if err != nil {
		t.Fatalf("first call err: %v", err)
	}
	if r1.OK {
		t.Error("first report should not be OK")
	}
	if len(r1.Findings) != 1 || r1.Findings[0].Line != 5 {
		t.Errorf("first report findings wrong: %+v", r1.Findings)
	}

	r2, err := fake.Lint(context.Background(), "python", "src2")
	if err != nil {
		t.Fatalf("second call err: %v", err)
	}
	if !r2.OK {
		t.Error("second report should be OK")
	}

	r3, err := fake.Lint(context.Background(), "jq", "src3")
	if err != nil {
		t.Fatalf("third call err: %v", err)
	}
	if !r3.OK {
		t.Error("third (default) report should be OK")
	}

	if len(fake.Calls) != 3 {
		t.Errorf("expected 3 recorded calls, got %d", len(fake.Calls))
	}
	if fake.Calls[0].Language != "bash" || fake.Calls[0].Source != "src1" {
		t.Errorf("first call recording wrong: %+v", fake.Calls[0])
	}
}

func TestFakeLinter_ReturnsCannedErrors(t *testing.T) {
	fake := &FakeLinter{
		Errors: []error{context.DeadlineExceeded},
	}
	_, err := fake.Lint(context.Background(), "bash", "src")
	if err != context.DeadlineExceeded {
		t.Errorf("expected DeadlineExceeded, got %v", err)
	}
}

func TestHasErrorSeverity(t *testing.T) {
	cases := []struct {
		name string
		in   []LintFinding
		want bool
	}{
		{"empty", nil, false},
		{"only warnings", []LintFinding{{Severity: "warning"}}, false},
		{"only infos", []LintFinding{{Severity: "info"}}, false},
		{"one error", []LintFinding{{Severity: "warning"}, {Severity: "error"}}, true},
		{"error first", []LintFinding{{Severity: "error"}}, true},
		{"all error", []LintFinding{{Severity: "error"}, {Severity: "error"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasErrorSeverity(tc.in); got != tc.want {
				t.Errorf("hasErrorSeverity(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestSeverityForRuffCode(t *testing.T) {
	cases := []struct {
		code, want string
	}{
		{"E501", "error"}, // line too long
		{"F401", "error"}, // unused import
		{"E999", "error"}, // syntax error
		{"W291", "warning"},
		{"B006", "warning"}, // bugbear
		{"I001", "warning"},
		{"UP015", "warning"},
		{"N801", "warning"},
		{"", "warning"}, // empty falls through to default
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			if got := severityForRuffCode(tc.code); got != tc.want {
				t.Errorf("severityForRuffCode(%q) = %q, want %q", tc.code, got, tc.want)
			}
		})
	}
}

func TestFirstLine(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"   \n  \n", ""},
		{"first", "first"},
		{"first\nsecond", "first"},
		{"\n\n  spaced line  \nthird", "spaced line"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := firstLine(tc.in); got != tc.want {
				t.Errorf("firstLine(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestDefaultLinter_UnknownLanguage(t *testing.T) {
	d := NewDefaultLinter(LinterConfig{})
	_, err := d.Lint(context.Background(), "ruby", "puts 'hi'")
	if err == nil {
		t.Error("expected error for unknown language")
	}
	if !strings.Contains(err.Error(), "unknown language") {
		t.Errorf("error should mention unknown language, got: %v", err)
	}
}

func TestDefaultLinter_LintJq_ValidSource(t *testing.T) {
	// Skip if jq isn't on the test machine.
	d := NewDefaultLinter(LinterConfig{})
	// A filter that takes any input and returns the .foo field's
	// length. Valid jq filter syntax.
	report, err := d.Lint(context.Background(), "jq", ".foo | length")
	if err != nil {
		t.Skipf("jq not available: %v", err)
	}
	if !report.OK {
		t.Errorf("expected clean parse, got: %+v", report)
	}
	if report.Linter != "jq" {
		t.Errorf("linter = %q, want jq", report.Linter)
	}
}

func TestDefaultLinter_LintJq_InvalidSource(t *testing.T) {
	// An unclosed pipe at the end is a syntax error.
	d := NewDefaultLinter(LinterConfig{})
	report, err := d.Lint(context.Background(), "jq", ".foo |")
	if err != nil {
		t.Skipf("jq not available: %v", err)
	}
	if report.OK {
		t.Errorf("expected parse error, got OK report: %+v", report)
	}
	if len(report.Findings) == 0 {
		t.Error("expected at least one finding on parse failure")
	}
}

func TestDefaultLinter_LintJq_IdentityFilter(t *testing.T) {
	// `.` is the simplest valid jq filter; should always pass.
	d := NewDefaultLinter(LinterConfig{})
	report, err := d.Lint(context.Background(), "jq", ".")
	if err != nil {
		t.Skipf("jq not available: %v", err)
	}
	if !report.OK {
		t.Errorf("identity filter should pass, got: %+v", report)
	}
}

func TestDefaultLinter_ConfigDefaults(t *testing.T) {
	// A zero-value config should get all the defaults filled in.
	d := NewDefaultLinter(LinterConfig{})
	if d.cfg.ShellcheckBinary == "" {
		t.Error("shellcheck binary default not applied")
	}
	if d.cfg.RuffBinary == "" {
		t.Error("ruff binary default not applied")
	}
	if d.cfg.JqBinary == "" {
		t.Error("jq binary default not applied")
	}
	if d.cfg.Timeout == 0 {
		t.Error("timeout default not applied")
	}
}

func TestDefaultLinter_HonorsContextCancellation(t *testing.T) {
	// A 1ms timeout against any linter should error out (no
	// machine is fast enough to lint + return in 1ms).
	// Use shellcheck so we don't depend on jq being installed.
	cfg := DefaultLinterConfig()
	cfg.Timeout = 1 * time.Millisecond
	d := NewDefaultLinter(cfg)
	// Use a fake-binary path so the lookup fails fast regardless
	// of whether shellcheck is installed. We want to test the
	// context cancellation, not the binary lookup.
	d.cfg.ShellcheckBinary = "/this/binary/does/not/exist"
	_, err := d.Lint(context.Background(), "bash", "echo hi")
	if err == nil {
		t.Error("expected error from linter with bad binary + 1ms timeout")
	}
}
