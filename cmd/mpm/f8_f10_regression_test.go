// f8_f10_regression_test.go — regressions for audit findings F8 and F10.
//
// F8: `mpm debug show` printed "Weight: 1" for every memory because the
// handler asserted mem["weight"].(int64) while GetMemory stores Go `int`.
//
// F10: `mpm theories list` treated the literal subcommand "list" as a
// status filter, matched zero rows, and reported none despite 61+ pending
// theories existing.
package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// captureStdout redirects os.Stdout while fn runs and returns what was written.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	return captureStreams(t, fn)[0]
}

// captureBoth redirects os.Stdout AND os.Stderr while fn runs and returns
// their combined output.
func captureBoth(t *testing.T, fn func()) string {
	t.Helper()
	return captureStreams(t, fn)[1]
}

func captureStreams(t *testing.T, fn func()) []string {
	t.Helper()
	origOut, origErr := os.Stdout, os.Stderr
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout, os.Stderr = wOut, wErr

	done := make(chan string)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, rOut)
		done <- buf.String()
	}()
	doneErr := make(chan string)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, rErr)
		doneErr <- buf.String()
	}()

	fn()

	os.Stdout, os.Stderr = origOut, origErr
	wOut.Close()
	wErr.Close()
	out, errOut := <-done, <-doneErr
	rOut.Close()
	rErr.Close()
	return []string{out, out + errOut}
}

func TestF8_DebugShowReportsStoredWeight(t *testing.T) {
	dm := newTestDMForCmd(t)

	cases := []struct {
		name   string
		weight int
	}{
		{"default weight", 1},
		{"weight five", 5},
		{"weight seven", 7},
		{"arbitrary weight", 23},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, err := dm.SaveMemory("memories", "f8 weight display target "+tc.name, "", nil, nil, nil, false, tc.weight)
			if err != nil {
				t.Fatalf("seed: %v", err)
			}

			// Persistence first: what is ACTUALLY stored? The display must
			// reflect it; the fix must not be presentation-only.
			mem, err := dm.GetMemory(id)
			if err != nil {
				t.Fatalf("GetMemory: %v", err)
			}
			storedWeight := 0
			switch w := mem["weight"].(type) {
			case int:
				storedWeight = w
			case int64:
				storedWeight = int(w)
			case float64:
				storedWeight = int(w)
			}
			if storedWeight != tc.weight {
				t.Fatalf("persistence layer returned weight %d, want %d", storedWeight, tc.weight)
			}

			out := captureStdout(t, func() {
				runShow(dm, id)
			})
			idx := strings.Index(out, "Weight:")
			if idx < 0 {
				t.Fatalf("output missing Weight line:\n%s", out)
			}
			line := out[idx:]
			if nl := strings.Index(line, "\n"); nl >= 0 {
				line = line[:nl]
			}
			got := strings.TrimSpace(strings.TrimPrefix(line, "Weight:"))
			want := strings.TrimSpace(itoaString(tc.weight))
			if got != want {
				t.Errorf("F8 REGRESSION: debug show printed %q, want %q (stored=%d)", got, want, tc.weight)
			}
		})
	}
}

func itoaString(n int) string {
	return strings.TrimSpace(strings.Repeat("", 0) + fmtInt(n))
}

func fmtInt(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	if neg {
		return "-" + digits
	}
	return digits
}

func seedTheoryWithStatus(t *testing.T, dm *mpminternal.DatabaseManager, content, status string) {
	t.Helper()
	_, err := dm.SaveMemory("theories", content, "", []string{"challenge"},
		map[string]interface{}{"status": status}, nil, false, 1)
	if err != nil {
		t.Fatalf("seed theory: %v", err)
	}
}

func TestF10_TheoriesListSubcommandReturnsPending(t *testing.T) {
	dm := newTestDMForCmd(t)

	for i := 0; i < 5; i++ {
		seedTheoryWithStatus(t, dm, "F10 pending theory number "+fmtInt(i), "pending")
	}
	seedTheoryWithStatus(t, dm, "F10 proven via arbitration", "proven")
	seedTheoryWithStatus(t, dm, "F10 legacy resolved row", "resolved")

	// The audited invocation: `mpm theories list` must show ALL theories,
	// not swallow "list" as a status filter that matches zero rows.
	stdout := captureStdout(t, func() {
		runTheories(dm, []string{"list"})
	})
	for _, needle := range []string{
		"F10 pending theory number 0",
		"F10 pending theory number 4",
		"F10 proven via arbitration",
	} {
		if !strings.Contains(stdout, needle) {
			t.Errorf("F10 REGRESSION: `theories list` output missing %q\noutput:\n%s", needle, stdout)
		}
	}
	if strings.Contains(stdout, "No list theories found") {
		t.Errorf("F10 REGRESSION: 'list' leaked into the status filter")
	}

	// Default invocation shows everything too.
	stdout = captureStdout(t, func() {
		runTheories(dm, nil)
	})
	if !strings.Contains(stdout, "F10 pending theory number 2") {
		t.Errorf("bare `mpm theories` should list all theories")
	}

	// Explicit filters still work.
	stdout = captureStdout(t, func() {
		runTheories(dm, []string{"pending"})
	})
	if strings.Contains(stdout, "F10 proven via arbitration") || strings.Contains(stdout, "legacy resolved") {
		t.Errorf("explicit pending filter leaked non-pending rows:\n%s", stdout)
	}
	if !strings.Contains(stdout, "F10 pending theory number 3") {
		t.Errorf("explicit pending filter dropped pending rows:\n%s", stdout)
	}

	// "resolved" covers both vocabularies (legacy CLI wrote "resolved",
	// MCP/arbitration write "proven"/"disproven").
	stdout = captureStdout(t, func() {
		runTheories(dm, []string{"resolved"})
	})
	if !strings.Contains(stdout, "F10 proven via arbitration") {
		t.Errorf("`theories resolved` must include MCP-vocabulary 'proven' rows:\n%s", stdout)
	}
	if !strings.Contains(stdout, "F10 legacy resolved row") {
		t.Errorf("`theories resolved` must include legacy 'resolved' rows:\n%s", stdout)
	}

	// Empty database reports cleanly for every invocation shape. The hint
	// goes to stderr via respond(), so capture both streams.
	emptyDM := newTestDMForCmd(t)
	stdout = captureBoth(t, func() {
		runTheories(emptyDM, []string{"list"})
	})
	if !strings.Contains(stdout, "No theories yet") {
		t.Errorf("empty DB should print the bootstrap hint, got:\n%s", stdout)
	}
}
