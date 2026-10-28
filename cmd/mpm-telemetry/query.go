package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/flowbyte-com/mpm/internal/telemetry"
)

func runQuery(args []string) error {
	// 2026-09-14 release-pass: help flags exit 0 BEFORE
	// workspace/DB resolution. Pre-fix the help path silently
	// fell through to `MPM_WORKSPACE is required` instead of
	// showing the help page.
	if hasHelpFlag(args) {
		writeHelpQuery(os.Stdout, buildVersion)
		return nil
	}
	if len(args) < 1 {
		return fmt.Errorf("usage: query invocation <id> | query session <id> | query since <unix-seconds>")
	}
	kind := args[0]
	rest := args[1:]

	store, err := openDefaultStore()
	if err != nil {
		return err
	}
	defer store.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var result any
	switch kind {
	case "invocation":
		if len(rest) < 1 {
			return fmt.Errorf("usage: query invocation <id>")
		}
		f, err := store.QueryInvocation(ctx, rest[0])
		if err != nil {
			return fmt.Errorf("query invocation: %w", err)
		}
		result = f
	case "session":
		if len(rest) < 1 {
			return fmt.Errorf("usage: query session <id>")
		}
		rows, err := store.QuerySession(ctx, rest[0])
		if err != nil {
			return fmt.Errorf("query session: %w", err)
		}
		result = rows
	case "since":
		if len(rest) < 1 {
			return fmt.Errorf("usage: query since <unix-seconds>")
		}
		cutoff, err := strconv.ParseInt(rest[0], 10, 64)
		if err != nil {
			return fmt.Errorf("parse cutoff: %w", err)
		}
		rows, err := store.QuerySince(ctx, cutoff)
		if err != nil {
			return fmt.Errorf("query since: %w", err)
		}
		result = rows
	default:
		return fmt.Errorf("unknown query kind %q", kind)
	}

	out, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}

// openDefaultStore opens the telemetry store for READING.
//
// A read surface must not CREATE the store it reads. `telemetry.Open`
// applies the DDL, so the old path materialised an empty telemetry.db
// on every machine where the collector had never run — and it could
// not distinguish "the collector has not run" from "the collector ran
// and recorded nothing".
//
// A missing store is a CLEAR ERROR that names the path and says what to
// do. It is not a fabricated empty result, and it is not a file on disk.
func openDefaultStore() (*telemetry.Store, error) {
	workspace := os.Getenv("MPM_WORKSPACE")
	if strings.TrimSpace(workspace) == "" && telemetry.EnvStorePathOverride() == "" {
		return nil, fmt.Errorf("MPM_WORKSPACE is required " +
			"(or set MPM_TELEMETRY_DB to the store's path)")
	}
	// The canonical path, resolved by the ONE resolver the collector,
	// the readers and the critic all use. The two used to disagree
	// with docs/SPEC.md §6.6, and the disagreement is what made the
	// critic's telemetry hunt inert.
	dbPath, err := telemetry.ResolveStorePath(workspace)
	if err != nil {
		return nil, err
	}
	store, err := telemetry.OpenReadOnly(dbPath)
	if err != nil {
		if errors.Is(err, telemetry.ErrStoreNotFound) {
			return nil, fmt.Errorf("no telemetry store at %s: the collector has not created it yet "+
				"(run `mpm-telemetry serve`)", telemetry.DescribeStorePath(dbPath))
		}
		return nil, fmt.Errorf("open telemetry store: %w", err)
	}
	return store, nil
}
