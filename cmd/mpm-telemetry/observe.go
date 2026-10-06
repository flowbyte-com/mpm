package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/flowbyte-com/mpm/internal/runtimebin"
	"github.com/flowbyte-com/mpm/internal/telemetry"
)

func runObserve(args []string) error {
	// 2026-09-14 release-pass: help flags exit 0 BEFORE flag
	// parsing or workspace resolution. Pre-fix the help path
	// raised an unrelated error instead of showing help.
	if hasHelpFlag(args) {
		writeHelpObserve(os.Stdout, buildVersion)
		return nil
	}
	fs := flag.NewFlagSet("observe", flag.ContinueOnError)
	since := fs.Int64("since", 0, "Unix epoch seconds; default = now-7d")
	threshold := fs.Int64("threshold", 100000, "high-token threshold (input + output)")
	minInv := fs.Int("min-invocations", 1, "minimum invocations per session")
	// --mpm is an operator override for the binary used for cross-DB
	// lookups. It defaults to EMPTY, not to "mpm": resolution happens
	// through runtimebin (MPM_BIN, then the sibling of this executable)
	// and only when a subprocess is actually required.
	//
	// It used to default to the bare name "mpm", so an unattended observe
	// run cross-joined whichever mpm PATH offered. Under a unit's PATH
	// that is not necessarily the installed one, and the resulting counts
	// look plausible either way — which is exactly why it needs to be
	// pinned rather than inherited.
	mpmPath := fs.String("mpm", "", "absolute path to mpm binary for cross-DB lookups; "+
		"default resolves MPM_BIN then the sibling of this executable")
	dryRun := fs.Bool("dry-run", false, "print findings instead of calling mpm call")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *since == 0 {
		*since = time.Now().Add(-7 * 24 * time.Hour).Unix()
	}

	store, err := openDefaultStore()
	if err != nil {
		return err
	}
	defer store.Close()

	lazyBin := &lazyMPMBin{explicit: *mpmPath}
	countFn := defaultArtifactCountFn(lazyBin)
	lessonFn := defaultLessonSaveFn(lazyBin)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	findings, err := telemetry.Hunt(ctx, store, telemetry.HuntConfig{
		Since: *since, HighTokenThreshold: *threshold, MinInvocations: *minInv,
	}, countFn)
	if err != nil {
		return err
	}

	cycleTag := fmt.Sprintf("cycle_%d", time.Now().Unix())
	for _, f := range findings {
		payload := telemetry.FindingLessonPayload(f, cycleTag)
		if *dryRun {
			out, _ := json.MarshalIndent(payload, "", "  ")
			fmt.Println(string(out))
			continue
		}
		if err := lessonFn(ctx, payload); err != nil {
			return fmt.Errorf("save lesson: %w", err)
		}
	}
	return nil
}

func defaultArtifactCountFn(l *lazyMPMBin) telemetry.ArtifactCountFn {
	return func(ctx context.Context, sessionID string) (int, error) {
		payload, _ := json.Marshal(map[string]any{
			"action":     "count_by_session",
			"session_id": sessionID,
		})
		out, err := runMpmCall(ctx, l, "mpm_provenance", payload)
		if err != nil {
			return 0, err
		}
		var resp struct {
			Count int `json:"count"`
		}
		if err := json.Unmarshal(out, &resp); err != nil {
			return 0, fmt.Errorf("parse count_by_session response: %w (raw: %s)", err, out)
		}
		return resp.Count, nil
	}
}

func defaultLessonSaveFn(l *lazyMPMBin) func(context.Context, map[string]any) error {
	return func(ctx context.Context, payload map[string]any) error {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		_, err = runMpmCall(ctx, l, "mpm_lessons", b)
		return err
	}
}

// lazyMPMBin resolves the mpm binary on first use and caches it.
//
// Resolution is deliberately deferred rather than performed at startup.
// `observe --dry-run` prints findings without saving a lesson, and on a
// quiet telemetry store the hunt may never call mpm at all — neither
// should fail because a binary could not be resolved for a subprocess
// that was never going to run. The error still surfaces the moment a
// call is genuinely required.
type lazyMPMBin struct {
	explicit string
	resolved string
	err      error
}

func (l *lazyMPMBin) path() (string, error) {
	if l.resolved != "" || l.err != nil {
		return l.resolved, l.err
	}
	l.resolved, l.err = (&runtimebin.Resolver{Explicit: l.explicit}).Resolve()
	return l.resolved, l.err
}

func runMpmCall(ctx context.Context, l *lazyMPMBin, tool string, payload []byte) ([]byte, error) {
	mpmPath, err := l.path()
	if err != nil {
		return nil, fmt.Errorf("resolve mpm binary for %s: %w", tool, err)
	}
	cmd := exec.CommandContext(ctx, mpmPath, "call", tool, "--payload", string(payload))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("mpm call %s: %v (output: %s)", tool, err, out)
	}
	return out, nil
}
