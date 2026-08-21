package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os/exec"
	"time"

	"github.com/flowbyte-com/mpm/internal/telemetry"
)

func runObserve(args []string) error {
	fs := flag.NewFlagSet("observe", flag.ContinueOnError)
	since := fs.Int64("since", 0, "Unix epoch seconds; default = now-7d")
	threshold := fs.Int64("threshold", 100000, "high-token threshold (input + output)")
	minInv := fs.Int("min-invocations", 1, "minimum invocations per session")
	mpmPath := fs.String("mpm", "mpm", "path to mpm binary for cross-DB lookups")
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

	countFn := defaultArtifactCountFn(*mpmPath)
	lessonFn := defaultLessonSaveFn(*mpmPath)

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

func defaultArtifactCountFn(mpmPath string) telemetry.ArtifactCountFn {
	return func(ctx context.Context, sessionID string) (int, error) {
		payload, _ := json.Marshal(map[string]any{
			"action":     "count_by_session",
			"session_id": sessionID,
		})
		out, err := runMpmCall(ctx, mpmPath, "mpm_provenance", payload)
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

func defaultLessonSaveFn(mpmPath string) func(context.Context, map[string]any) error {
	return func(ctx context.Context, payload map[string]any) error {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		_, err = runMpmCall(ctx, mpmPath, "mpm_lessons", b)
		return err
	}
}

func runMpmCall(ctx context.Context, mpmPath, tool string, payload []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, mpmPath, "call", tool, "--payload", string(payload))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("mpm call %s: %v (output: %s)", tool, err, out)
	}
	return out, nil
}
