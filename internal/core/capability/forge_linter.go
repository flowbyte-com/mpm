package capability

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// =============================================================================
// forge_linter.go — source linter integration (spec §3.2 step 5)
//
// The linter is a security + quality gate. For each source_language
// the Forge shells out to the canonical tool for that ecosystem:
//
//   * bash   → shellcheck -f json -        (reads from stdin)
//   * python → ruff check - --stdin-filename=script.py
//   * jq     → jq . <<< 'source'           (parse check)
//
// Linter findings are recorded as structured FieldErrors so a
// failed lint points to the exact line, column, and linter rule.
// The Forge rejects on findings of severity="error" only; warnings
// and infos are recorded but do not block promotion. The intent
// is "lint says this code is broken" vs. "lint says this code
// could be cleaner" — the former is a gate, the latter is advisory.
//
// The Linter interface is pluggable. Production uses DefaultLinter
// (real os/exec). Tests inject FakeLinter. This is the same shape
// as the SecretScanner split — keeps the unit-test boundary clean.
// =============================================================================

// LintFinding is one issue reported by an external linter. The
// shape mirrors the JSON output of shellcheck and ruff; the jq
// linter is parse-only and produces no findings on success.
type LintFinding struct {
	Line     int    `json:"line"`
	Column   int    `json:"column"`
	Code     string `json:"code"`
	Severity string `json:"severity"` // "error" | "warning" | "info"
	Message  string `json:"message"`
}

// LintReport is the aggregate result for one (language, source) pair.
// OK is true iff there are no findings of severity="error".
type LintReport struct {
	Linter   string
	Findings []LintFinding
	OK       bool
}

// Linter is the pluggable interface. The Forge uses this so tests
// can inject a fake that simulates shellcheck/ruff without requiring
// the binaries on the test machine.
type Linter interface {
	// Lint runs the appropriate linter for the given language
	// against the source code. The source is passed via stdin
	// (not a temp file) so the linter sees exactly what the
	// agent proposed, not a re-encoded copy.
	//
	// The implementation MUST honour ctx cancellation. A linter
	// that runs unbounded is a denial-of-service vector — agents
	// can submit 64KB sources that take 10 minutes to lint.
	//
	// Returns *LintReport and a non-nil error only on infrastructure
	// failures (binary missing, context cancelled, etc.). Findings
	// are NOT errors — a report with errors has OK=false but err==nil.
	Lint(ctx context.Context, language, source string) (*LintReport, error)
}

// LinterConfig lets the operator tune the linter integration. The
// Forge owns one of these; tests can pass a zero-value Config and
// get the default binaries with default timeouts.
type LinterConfig struct {
	ShellcheckBinary string        // default: "shellcheck"
	RuffBinary       string        // default: "ruff"
	JqBinary         string        // default: "jq"
	Timeout          time.Duration // default: 30s
}

// DefaultLinterConfig returns the production defaults. Tests that
// don't need a real linter should skip this and use FakeLinter.
func DefaultLinterConfig() LinterConfig {
	return LinterConfig{
		ShellcheckBinary: "shellcheck",
		RuffBinary:       "ruff",
		JqBinary:         "jq",
		Timeout:          30 * time.Second,
	}
}

// DefaultLinter is the production Linter. It shells out to the
// configured binaries and parses JSON output. If a binary is
// missing, Lint returns an infrastructure error (NOT a finding) —
// the Forge treats that as a hard reject (fail-closed for toolchain).
type DefaultLinter struct {
	cfg LinterConfig
}

// NewDefaultLinter builds a DefaultLinter with the given config.
// A zero-value config is filled in with DefaultLinterConfig().
func NewDefaultLinter(cfg LinterConfig) *DefaultLinter {
	def := DefaultLinterConfig()
	if cfg.ShellcheckBinary == "" {
		cfg.ShellcheckBinary = def.ShellcheckBinary
	}
	if cfg.RuffBinary == "" {
		cfg.RuffBinary = def.RuffBinary
	}
	if cfg.JqBinary == "" {
		cfg.JqBinary = def.JqBinary
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = def.Timeout
	}
	return &DefaultLinter{cfg: cfg}
}

// Lint dispatches to the language-specific linter. Unknown
// languages return an infrastructure error (should never happen
// because Validate() already gates the language enum).
func (d *DefaultLinter) Lint(ctx context.Context, language, source string) (*LintReport, error) {
	switch language {
	case "bash":
		return d.lintShellcheck(ctx, source)
	case "python":
		return d.lintRuff(ctx, source)
	case "jq":
		return d.lintJqParse(ctx, source)
	default:
		return nil, fmt.Errorf("capability: linter: unknown language %q", language)
	}
}

// lintShellcheck shells out to shellcheck -f json. JSON output is
// the most parseable form; we read stdout and unmarshal.
func (d *DefaultLinter) lintShellcheck(ctx context.Context, source string) (*LintReport, error) {
	c, cancel := context.WithTimeout(ctx, d.cfg.Timeout)
	defer cancel()

	cmd := exec.CommandContext(c, d.cfg.ShellcheckBinary, "-f", "json", "-")
	cmd.Stdin = strings.NewReader(source)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	// shellcheck exits 0 (clean), 1 (issues found), 2 (fatal
	// config error), 3+ (other errors). Non-zero exit is not
	// itself an error — we just need to parse the JSON.
	if err != nil && stdout.Len() == 0 {
		return nil, fmt.Errorf("capability: shellcheck: %w (stderr=%q)",
			err, stderr.String())
	}

	// shellcheck JSON shape: {"comments":[{...}]}
	// Each comment has: file, line, endLine, column, endColumn,
	// level ("error"|"warning"|"info"|"style"), code, message, fix.
	var parsed struct {
		Comments []struct {
			Line    int    `json:"line"`
			Column  int    `json:"column"`
			Level   string `json:"level"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
		return nil, fmt.Errorf("capability: shellcheck: parse json: %w", err)
	}

	report := &LintReport{Linter: "shellcheck"}
	for _, c := range parsed.Comments {
		report.Findings = append(report.Findings, LintFinding{
			Line:     c.Line,
			Column:   c.Column,
			Code:     c.Code,
			Severity: c.Level,
			Message:  c.Message,
		})
	}
	report.OK = !hasErrorSeverity(report.Findings)
	return report, nil
}

// lintRuff shells out to `ruff check --stdin-filename=...`. Ruff
// emits JSON: [{"code","message","location":{row,column},...}].
func (d *DefaultLinter) lintRuff(ctx context.Context, source string) (*LintReport, error) {
	c, cancel := context.WithTimeout(ctx, d.cfg.Timeout)
	defer cancel()

	cmd := exec.CommandContext(c, d.cfg.RuffBinary,
		"check", "--stdin-filename=script.py", "-", "--output-format=json")
	cmd.Stdin = strings.NewReader(source)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	// ruff exits 0 (clean), 1 (issues), 2 (error). Same as
	// shellcheck: non-zero is not necessarily an infrastructure
	// error if we got JSON.
	if err != nil && stdout.Len() == 0 {
		return nil, fmt.Errorf("capability: ruff: %w (stderr=%q)",
			err, stderr.String())
	}

	// Empty output is valid for clean code.
	if strings.TrimSpace(stdout.String()) == "" {
		return &LintReport{Linter: "ruff", OK: true}, nil
	}

	// ruff JSON shape: [{code, message, location, ...}]
	var parsed []struct {
		Code     string `json:"code"`
		Message  string `json:"message"`
		Location struct {
			Row    int `json:"row"`
			Column int `json:"column"`
		} `json:"location"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
		return nil, fmt.Errorf("capability: ruff: parse json: %w", err)
	}

	report := &LintReport{Linter: "ruff"}
	for _, r := range parsed {
		severity := severityForRuffCode(r.Code)
		report.Findings = append(report.Findings, LintFinding{
			Line:     r.Location.Row,
			Column:   r.Location.Column,
			Code:     r.Code,
			Severity: severity,
			Message:  r.Message,
		})
	}
	report.OK = !hasErrorSeverity(report.Findings)
	return report, nil
}

// severityForRuffCode maps ruff codes (E, W, F, B, ...) to our
// severity scale. E/F = error (parse/import failures). The rest
// (W, B, I, N, UP, ...) are warnings. We err on the side of
// strict — a strict-by-default linter is easier to relax than to
// tighten post-deployment.
func severityForRuffCode(code string) string {
	if len(code) == 0 {
		return "warning"
	}
	switch code[0] {
	case 'E', 'F':
		return "error"
	default:
		return "warning"
	}
}

// lintJqParse checks jq syntax by running the source as a filter
// against a null input (`jq -n -f <tmpfile>`). The source is the
// filter (via a temp file because not every jq version supports
// `-f -` reading the filter from stdin).
//
// We use `-n` (null input) so the test doesn't depend on the
// source being a valid JSON document — the source is the filter,
// null is the data. A parse error means the filter is invalid; we
// capture the first stderr line for the rejection message.
//
// The temp file is removed on every exit path. We use a unique
// pattern (mpm-jq-*.jq) so concurrent Forge calls don't collide.
func (d *DefaultLinter) lintJqParse(ctx context.Context, source string) (*LintReport, error) {
	c, cancel := context.WithTimeout(ctx, d.cfg.Timeout)
	defer cancel()

	tmpFile, err := os.CreateTemp("", "mpm-jq-*.jq")
	if err != nil {
		return nil, fmt.Errorf("capability: jq linter: create temp: %w", err)
	}
	defer os.Remove(tmpFile.Name()) //nolint:errcheck

	if _, err := tmpFile.WriteString(source); err != nil {
		tmpFile.Close()
		return nil, fmt.Errorf("capability: jq linter: write temp: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return nil, fmt.Errorf("capability: jq linter: close temp: %w", err)
	}

	cmd := exec.CommandContext(c, d.cfg.JqBinary, "-n", "-f", tmpFile.Name())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err = cmd.Run()
	if err == nil {
		return &LintReport{Linter: "jq", OK: true}, nil
	}

	msg := firstLine(stderr.String())
	if msg == "" {
		msg = "jq parse failed"
	}
	return &LintReport{
		Linter: "jq",
		Findings: []LintFinding{{
			Line:     1,
			Code:     "JQ_PARSE",
			Severity: "error",
			Message:  msg,
		}},
		OK: false,
	}, nil
}

// hasErrorSeverity returns true if any finding is severity="error".
// Used by every linter to set the OK flag.
func hasErrorSeverity(findings []LintFinding) bool {
	for _, f := range findings {
		if f.Severity == "error" {
			return true
		}
	}
	return false
}

// firstLine trims and returns the first non-empty line of s.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// =============================================================================
// FakeLinter — test-only Linter that returns a pre-canned report.
//
// Tests use this to avoid requiring shellcheck/ruff/jq on the
// build machine. The Forge itself never imports this; it only
// accepts the Linter interface.
// =============================================================================

// FakeLinter is a programmable Linter for tests. Each call records
// the language/source and returns the next pre-set report. If
// Reports is empty, returns an empty (OK) report.
type FakeLinter struct {
	// Reports is the queue of canned responses. Lint consumes
	// from the front; if empty, returns an empty (clean) report.
	Reports []*LintReport
	// Errors is the queue of infrastructure errors. Lint consumes
	// from the front; if empty, returns nil error.
	Errors []error
	// Calls records every (language, source) tuple passed to Lint.
	// Tests assert the linter was called with the right payload.
	Calls []FakeLinterCall
}

// FakeLinterCall records one invocation.
type FakeLinterCall struct {
	Language string
	Source   string
}

// Lint returns the next pre-canned report/error and records the call.
// If the queue is empty, returns an empty clean report.
func (f *FakeLinter) Lint(_ context.Context, language, source string) (*LintReport, error) {
	f.Calls = append(f.Calls, FakeLinterCall{Language: language, Source: source})
	var report *LintReport
	if len(f.Reports) > 0 {
		report = f.Reports[0]
		f.Reports = f.Reports[1:]
	} else {
		report = &LintReport{Linter: "fake", OK: true}
	}
	var err error
	if len(f.Errors) > 0 {
		err = f.Errors[0]
		f.Errors = f.Errors[1:]
	}
	return report, err
}
