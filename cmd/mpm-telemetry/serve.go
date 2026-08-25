package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/flowbyte-com/mpm/internal/telemetry"
)

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	quiet := fs.Bool("quiet", false, "suppress startup banner")
	if err := fs.Parse(args); err != nil {
		return err
	}

	workspace := os.Getenv("MPM_WORKSPACE")
	if workspace == "" {
		return fmt.Errorf("MPM_WORKSPACE is required")
	}
	socketPath := os.Getenv("MPM_TELEMETRY_SOCKET")
	if socketPath == "" {
		socketPath = filepath.Join(workspace, "run", "mpm-telemetry.sock")
	} else {
		socketPath = filepath.Clean(socketPath)
	}
	dbPath := os.Getenv("MPM_TELEMETRY_DB")
	if dbPath == "" {
		dbPath = filepath.Join(workspace, "src", "db", "telemetry.db")
	} else {
		dbPath = filepath.Clean(dbPath)
	}

	store, err := telemetry.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open telemetry.db: %w", err)
	}
	defer store.Close()

	if !*quiet {
		fmt.Fprintf(os.Stderr, "mpm-telemetry serve: socket=%s db=%s\n", socketPath, dbPath)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	return telemetry.Serve(ctx, store, socketPath)
}
