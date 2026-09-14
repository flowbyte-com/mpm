package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"
)

func runPing(args []string) error {
	// 2026-09-14 release-pass: help flags exit 0 BEFORE
	// workspace/socket validation. Pre-fix this branch
	// short-circuited to "MPM_WORKSPACE is required" on
	// `mpm-telemetry ping --help`, which was a help-safety
	// defect (help requested an env var the operator was
	// trying to discover).
	if hasHelpFlag(args) {
		writeHelpPing(os.Stdout, buildVersion)
		return nil
	}
	workspace := os.Getenv("MPM_WORKSPACE")
	if workspace == "" {
		return fmt.Errorf("MPM_WORKSPACE is required")
	}
	socketPath := defaultTelemetrySocketPath()
	if socketPath == "" {
		return fmt.Errorf("could not derive telemetry socket path (set MPM_TELEMETRY_SOCKET or MPM_WORKSPACE)")
	}

	dialer := net.Dialer{Timeout: 2 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return fmt.Errorf("dial socket %s: %w", socketPath, err)
	}
	defer conn.Close()

	// Send a ping frame: a single NDJSON line with event_type=ping.
	// The collector will respond with a handshake describing its version
	// and current state.
	if _, err := conn.Write([]byte(`{"event_type":"ping"}` + "\n")); err != nil {
		return fmt.Errorf("write ping: %w", err)
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	if !scanner.Scan() {
		return fmt.Errorf("read handshake: %v", scanner.Err())
	}
	var resp map[string]any
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		return fmt.Errorf("parse handshake: %w", err)
	}
	out, err := json.MarshalIndent(resp, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}
