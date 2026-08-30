package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

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
	socketPath := defaultTelemetrySocketPath()
	if socketPath == "" {
		return fmt.Errorf("could not derive telemetry socket path (set MPM_TELEMETRY_SOCKET or MPM_WORKSPACE)")
	}
	socketPath = filepath.Clean(socketPath)
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

	// Trap SIGINT and SIGTERM so the deferred unlink in
	// internal/telemetry/serve runs on operator shutdown. Without
	// this, Go's default SIGTERM handler kills the process before
	// defers fire, leaving a stale socket inode that confuses the
	// next serve start. systemd unit stops with SIGTERM, so this
	// path is the canonical shutdown sequence.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-sigCh:
			cancel()
		}
	}()
	defer signal.Stop(sigCh)

	return telemetry.Serve(ctx, store, socketPath)
}
