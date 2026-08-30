// version_stamping_test.go — regressions for audit finding D-003.
//
// D-003: `mpm-telemetry -h` reported `Build: dev` regardless of the
// canonical `-ldflags "-X main.buildVersion=..."` stamp from `make build`.
// Root cause: `buildVersion` was declared `const`, so the linker flag
// silently no-op'd. The fix changes it to `var` so the linker can
// rewrite it at link time.
//
// This is a build-artifact regression test: it builds the binary with
// a known ldflags stamp and asserts the binary reports that stamp on
// `-h`. Skip if `go` is not available.
package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestD003_BuildVersionStamped pins the headline regression: a binary
// built with `-ldflags "-X main.buildVersion=vTEST"` must report
// `Build: vTEST` (not `Build: dev`) in the `-h` output.
func TestD003_BuildVersionStamped(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	// Build a fresh telemetry binary with a known stamp.
	const stamp = "vTEST-d003-regression-2026-08-30"
	binPath := t.TempDir() + "/mpm-telemetry-stamped"
	cmd := exec.Command("go", "build", "-tags", "fts5",
		"-ldflags", "-X main.buildVersion="+stamp,
		"-o", binPath, ".")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build: %v", err)
	}

	out, err := exec.Command(binPath, "-h").CombinedOutput()
	if err != nil {
		// `-h` exits 0 normally; if it errors here something is wrong.
		t.Fatalf("mpm-telemetry -h: %v\n%s", err, out)
	}

	got := extractBuildLine(string(out))
	if got != "Build: "+stamp {
		t.Errorf("D-003 REGRESSION: telemetry reported %q, want %q (ldflags stamping broken)", got, "Build: "+stamp)
	}
}

// TestD003_DefaultVersionIsDev pins the fallback contract: when no
// ldflags stamp is provided, the binary still reports the literal
// `dev` placeholder. This makes operator intent unambiguous — a build
// without an explicit stamp must be visible as such.
func TestD003_DefaultVersionIsDev(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	binPath := t.TempDir() + "/mpm-telemetry-dev"
	cmd := exec.Command("go", "build", "-tags", "fts5", "-o", binPath, ".")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build: %v", err)
	}

	out, err := exec.Command(binPath, "-h").CombinedOutput()
	if err != nil {
		t.Fatalf("mpm-telemetry -h: %v\n%s", err, out)
	}

	got := extractBuildLine(string(out))
	if got != "Build: dev" {
		t.Errorf("default build stamp should be 'dev', got %q", got)
	}
}

// TestD003_NoConstBuildVersion pins the structural regression: the
// `buildVersion` symbol MUST be declared `var` (not `const`). A const
// would bake "dev" into the binary at compile time and the linker flag
// would silently no-op. We test this with a static check on the source
// rather than a runtime probe — the runtime probe above already
// confirms the consequence, this one confirms the structural cause.
func TestD003_NoConstBuildVersion(t *testing.T) {
	// Read our own source — the test runs in the same package as main.go.
	const src = "main.go"
	body, err := os.ReadFile(src)
	if err != nil {
		t.Skipf("could not read %s: %v", src, err)
	}

	text := string(body)
	if !strings.Contains(text, "var buildVersion") {
		t.Errorf("D-003 REGRESSION: %s does not declare `var buildVersion`; "+
			"the linker ldflags stamp will silently no-op", src)
	}
	if strings.Contains(text, "const buildVersion") {
		t.Errorf("D-003 REGRESSION: %s declares `const buildVersion`; "+
			"the linker ldflags stamp will silently no-op", src)
	}
}

// extractBuildLine pulls the `Build: <value>` line out of the
// `-h` output, normalising trailing whitespace.
func extractBuildLine(helpOutput string) string {
	for _, line := range strings.Split(helpOutput, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Build:") {
			return line
		}
	}
	return ""
}
