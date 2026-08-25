package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/flowbyte-com/mpm/internal/telemetry"
)

func runQuery(args []string) error {
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

func openDefaultStore() (*telemetry.Store, error) {
	workspace := os.Getenv("MPM_WORKSPACE")
	if workspace == "" {
		return nil, fmt.Errorf("MPM_WORKSPACE is required")
	}
	dbPath := os.Getenv("MPM_TELEMETRY_DB")
	if dbPath == "" {
		dbPath = filepath.Join(workspace, "src", "db", "telemetry.db")
	}
	return telemetry.Open(dbPath)
}
