package telemetry

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServe_AcceptsFrameAndPersistsRow(t *testing.T) {
	tmp := t.TempDir()
	socketPath := filepath.Join(tmp, "telemetry.sock")
	dbPath := filepath.Join(tmp, "telemetry.db")

	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = Serve(ctx, store, socketPath)
	}()
	defer func() {
		cancel()
		time.Sleep(50 * time.Millisecond) // let serve exit
	}()

	// Wait for socket to be ready.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	const goodFrame = `{"schema_version":"v1","event_type":"invocation_completed","invocation_id":"inv_sock","framework":"claude-code","provider":"anthropic","model":"claude-fable-5","started_at":1756000000,"completed_at":1756000012,"status":"completed","input_tokens":100,"output_tokens":50,"provider_metadata":{}}`
	if _, err := conn.Write([]byte(goodFrame + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatalf("read response: %v", scanner.Err())
	}
	resp := scanner.Text()
	if !strings.Contains(resp, `"status":"ACCEPTED"`) {
		t.Errorf("response = %q, want ACCEPTED", resp)
	}

	// Verify row landed.
	var n int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM telemetry_invocation WHERE invocation_id=?`, "inv_sock").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("row count = %d, want 1", n)
	}
}
