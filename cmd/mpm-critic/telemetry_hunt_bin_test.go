package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flowbyte-com/mpm/internal/telemetry"
)

// The critic's telemetry hunt shells out twice per cycle: `mpm call
// mpm_provenance` for the cross-DB artifact count, and `mpm call mpm_lessons`
// to save the finding. Both used to pass a bare "mpm" and let PATH decide,
// which meant a single cycle could read its artifact count from one substrate
// and write its lesson into another.
//
// These tests seed a telemetry session that actually qualifies for a finding
// (high token burn, zero artifacts) so that BOTH subprocesses are reached.
// An empty store would produce no finding, the count function would never be
// called, and the assertions below would pass vacuously.

const huntSessionID = "session-hunt-1"

func i64(v int64) *int64   { return &v }
func str(v string) *string { return &v }

// seedQualifyingSession writes one invocation whose token total exceeds the
// hunt's 100k threshold, so the hunt calls countFn and, on a zero count,
// emits a finding and calls lessonFn.
func seedQualifyingSession(t *testing.T, projectRoot string) {
	t.Helper()
	store, err := telemetry.Open(filepath.Join(projectRoot, "telemetry.db"))
	if err != nil {
		t.Fatalf("open telemetry store: %v", err)
	}
	now := time.Now().Unix()
	res, err := store.InsertFrame(context.Background(), telemetry.Frame{
		SchemaVersion:    "1",
		EventType:        "invocation",
		InvocationID:     "inv-hunt-1",
		SessionID:        str(huntSessionID),
		Framework:        "claude-code",
		Provider:         "anthropic",
		Model:            "test-model",
		StartedAt:        now - 60,
		CompletedAt:      now - 30,
		Status:           "completed",
		InputTokens:      i64(120000),
		OutputTokens:     i64(10000),
		ProviderMetadata: json.RawMessage(`{}`),
	})
	if err != nil {
		store.Close()
		t.Fatalf("InsertFrame: %v", err)
	}
	if !res.Inserted {
		t.Error("InsertFrame did not insert; the hunt would find nothing to do")
	}
	store.Close()
}

// huntFakeMPM writes a fake mpm that answers the provenance cross-DB lookup
// with zero artifacts (so the hunt yields a finding) and succeeds silently for
// the lesson save. Every invocation is appended to logPath with its argv0.
func huntFakeMPM(t *testing.T, dir, name, logPath string) string {
	t.Helper()
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	body := "#!/bin/sh\n" +
		"printf '%s\\n' \"$0 $*\" >> " + q(logPath) + "\n" +
		"if [ \"$2\" = \"mpm_provenance\" ]; then printf '%s' '{\"count\":0}'; fi\n" +
		"exit 0\n"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake mpm: %v", err)
	}
	return path
}

// runHuntForTest executes one telemetry hunt with a hostile `mpm` first on
// PATH and the real binary supplied via MPM_BIN. It returns the fake binary's
// call log and the hostile binary's call log.
func runHuntForTest(t *testing.T) (goodLog, hostileLog string) {
	t.Helper()

	projectRoot := t.TempDir()
	seedQualifyingSession(t, projectRoot)

	binDir := t.TempDir()
	goodLog = filepath.Join(binDir, "good.log")
	good := huntFakeMPM(t, binDir, "mpm", goodLog)
	t.Setenv("MPM_BIN", good)

	hostileDir := t.TempDir()
	hostileLog = filepath.Join(hostileDir, "hostile.log")
	hp := filepath.Join(hostileDir, "mpm")
	if err := os.WriteFile(hp, []byte("#!/bin/sh\necho HOSTILE >> "+shellQuotePath(hostileLog)+"\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write hostile mpm: %v", err)
	}
	t.Setenv("PATH", hostileDir)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := runTelemetryHunt(context.Background(), logger, projectRoot, 7); err != nil {
		t.Fatalf("runTelemetryHunt: %v", err)
	}
	return goodLog, hostileLog
}

func shellQuotePath(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func readLogFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read log: %v", err)
	}
	return string(b)
}

// TestTelemetryHunt_ProvenanceCallUsesExactResolvedPath covers requirement I.
func TestTelemetryHunt_ProvenanceCallUsesExactResolvedPath(t *testing.T) {
	goodLog, hostileLog := runHuntForTest(t)

	got := readLogFile(t, goodLog)
	provenanceLines := 0
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "call mpm_provenance") {
			provenanceLines++
			// argv0 must be an absolute path naming the resolved binary,
			// never a bare name that PATH would have to interpret.
			fields := strings.Fields(line)
			if len(fields) == 0 || !filepath.IsAbs(fields[0]) {
				t.Errorf("provenance call argv0 is not absolute: %q", line)
			}
			if filepath.Base(fields[0]) != "mpm" {
				t.Errorf("provenance call ran %q, want the resolved mpm binary", fields[0])
			}
		}
	}
	if provenanceLines != 1 {
		t.Fatalf("provenance calls = %d, want exactly 1 (test is vacuous if the hunt made none):\n%s", provenanceLines, got)
	}
	if h := readLogFile(t, hostileLog); h != "" {
		t.Errorf("hostile PATH mpm was used for the provenance lookup: %q", h)
	}
}

// TestTelemetryHunt_LessonCallUsesExactResolvedPath covers requirement J.
//
// It asserts the lesson call runs the SAME binary identity as the provenance
// call, which is the property the old per-call PATH lookup could not guarantee.
func TestTelemetryHunt_LessonCallUsesExactResolvedPath(t *testing.T) {
	goodLog, hostileLog := runHuntForTest(t)

	got := readLogFile(t, goodLog)
	provBin, lessonBin := "", ""
	for _, line := range strings.Split(got, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch {
		case strings.Contains(line, "call mpm_provenance"):
			provBin = fields[0]
		case strings.Contains(line, "call mpm_lessons"):
			lessonBin = fields[0]
		}
	}
	if provBin == "" {
		t.Fatalf("no provenance call recorded; the test is vacuous:\n%s", got)
	}
	if lessonBin == "" {
		t.Fatalf("no lesson call recorded; the hunt produced no finding:\n%s", got)
	}
	if lessonBin != provBin {
		t.Errorf("lesson call ran %q but provenance call ran %q; one cycle must use one binary identity", lessonBin, provBin)
	}
	if h := readLogFile(t, hostileLog); h != "" {
		t.Errorf("hostile PATH mpm was used: %q", h)
	}
}

// TestTelemetryHunt_FailsClearlyWhenNoBinaryResolvable pins the fail-closed
// behaviour: with no MPM_BIN, no sibling, and a hostile PATH, the hunt must
// error rather than silently run the wrong MPM.
func TestTelemetryHunt_FailsClearlyWhenNoBinaryResolvable(t *testing.T) {
	projectRoot := t.TempDir()
	seedQualifyingSession(t, projectRoot)

	hostileDir := t.TempDir()
	hostileLog := filepath.Join(hostileDir, "hostile.log")
	hp := filepath.Join(hostileDir, "mpm")
	if err := os.WriteFile(hp, []byte("#!/bin/sh\necho HOSTILE >> "+shellQuotePath(hostileLog)+"\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write hostile mpm: %v", err)
	}
	t.Setenv("PATH", hostileDir)
	t.Setenv("MPM_BIN", "")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := runTelemetryHunt(context.Background(), logger, projectRoot, 7)
	if err == nil {
		t.Fatal("runTelemetryHunt succeeded with no resolvable mpm binary")
	}
	if !strings.Contains(err.Error(), "MPM_BIN") {
		t.Errorf("error = %v, want it to name MPM_BIN", err)
	}
	if h := readLogFile(t, hostileLog); h != "" {
		t.Errorf("hostile PATH mpm was used as a fallback: %q", h)
	}
}
