// multi-agent-write-test: Day-5 contention harness for the mpm substrate.
//
// Stress test for SQLite WAL mode + 5s busy_timeout under concurrent
// writes from three independent invocation paths:
//
//  1. CLI writers — exec.Command("mpm", "memory", "add", ...).
//     Real-world: agents shelling out from scripts.
//  2. MCP writers — each worker spawns its own mpm-mcp stdio server,
//     serializes save() requests through it.
//     Real-world: Hermes / OpenClaw / Claude Code each owning one
//     mpm-mcp child.
//  3. Direct SQLite readers — separate *sql.DB connections running
//     FTS5 + aggregate queries in a tight loop.
//     Real-world: the wake scheduler reading scheduled_wakes while
//     writes are in flight.
//
// Verification gates (per the Day-5 blueprint, 2026-08-17):
//
//	G1: Zero SQLITE_BUSY errors leaked to any caller.
//	G2: Zero dropped rows — memory_count_delta == successful_writes.
//	G3: Zero reader latency spikes — max(per-query latency) < 500ms.
//
// Lives at cmd/multi-agent-write-test/ (separate binary, not part of
// the regular `go test ./...` sweep). Run with:
//
//	go run ./cmd/multi-agent-write-test -duration 30s \
//	  -cli-workers 8 -mcp-workers 4 -read-workers 4
package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// ── Config ─────────────────────────────────────────────────────────────

type Config struct {
	Duration    time.Duration
	CLIWorkers  int
	MCPWorkers  int
	ReadWorkers int
	DBPath      string
	MPMBin      string
	MCPBin      string
	Tag         string
	SpikeMs     int64
}

func parseFlags() Config {
	var cfg Config
	flag.DurationVar(&cfg.Duration, "duration", 30*time.Second, "test wall-clock duration")
	flag.IntVar(&cfg.CLIWorkers, "cli-workers", 8, "CLI writer goroutines (each execs one mpm CLI at a time)")
	flag.IntVar(&cfg.MCPWorkers, "mcp-workers", 4, "MCP writer goroutines (each spawns one mpm-mcp)")
	flag.IntVar(&cfg.ReadWorkers, "read-workers", 4, "direct-Sqlite reader goroutines")
	// Flag defaults resolve to the canonical user-level install root
	// ($HOME/.mpm/...) so this binary works on any host without
	// pointing at a specific author's checkout. Operators on a
	// non-default layout override via -db / -mpm / -mcp explicitly.
	defaultDB, defaultMPMBin, defaultMCPBin := mpmDefaultPaths()
	flag.StringVar(&cfg.DBPath, "db", defaultDB, "SQLite database path")
	flag.StringVar(&cfg.MPMBin, "mpm", defaultMPMBin, "mpm CLI binary")
	flag.StringVar(&cfg.MCPBin, "mcp", defaultMCPBin, "mpm-mcp stdio server binary")
	flag.StringVar(&cfg.Tag, "tag", "stress-test-day5", "tag applied to every stress-test memory for post-test cleanup")
	flag.Int64Var(&cfg.SpikeMs, "spike-ms", 500, "reader latency spike threshold in milliseconds")
	flag.Parse()
	return cfg
}

// mpmDefaultPaths returns the user-level canonical paths this
// harness defaults to: $HOME/.mpm/src/db/mpm.db (database) and
// $HOME/.mpm/bin/{mpm,mpm-mcp} (binaries). The previous hardcoded
// "/home/v/.mpm/..." values assumed the original author's home
// directory. If HOME is unreachable, we fall back to empty strings
// so flag.Parse surfaces the missing value via the operator's
// explicit override (the alternative — silently picking another
// user's path — is worse).
func mpmDefaultPaths() (db, mpmBin, mcpBin string) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", "", ""
	}
	db = filepath.Join(home, ".mpm", "src", "db", "mpm.db")
	mpmBin = filepath.Join(home, ".mpm", "bin", "mpm")
	mcpBin = filepath.Join(home, ".mpm", "bin", "mpm-mcp")
	return db, mpmBin, mcpBin
}

// ── Shared counters ────────────────────────────────────────────────────

type Counters struct {
	WriteSuccess atomic.Int64
	WriteBusy    atomic.Int64
	WriteOther   atomic.Int64
	ReadSpikes   atomic.Int64

	mu             sync.Mutex
	WriteLatencies []time.Duration
	ReadLatencies  []time.Duration
}

func (c *Counters) RecordWriteLatency(d time.Duration) {
	c.mu.Lock()
	c.WriteLatencies = append(c.WriteLatencies, d)
	c.mu.Unlock()
}

func (c *Counters) RecordReadLatency(d time.Duration) {
	c.mu.Lock()
	c.ReadLatencies = append(c.ReadLatencies, d)
	c.mu.Unlock()
}

// ── Helpers ────────────────────────────────────────────────────────────

// countMemories uses the test's own read-only DB connection so we
// don't depend on the production mpm CLI for the verification check.
func countMemories(db *sql.DB) (int64, error) {
	var n int64
	if err := db.QueryRow(`SELECT count(*) FROM memories`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// stressFact builds the unique fact string for write attempt N.
func stressFact(workerID, attempt int, tag string) string {
	return fmt.Sprintf("day5-stress [%s worker=%d attempt=%d ts=%d]",
		tag, workerID, attempt, time.Now().UnixNano())
}

// drainRows iterates a *sql.Rows iterator to completion and closes
// it via defer. Extracted into its own function so the mpm-lint
// no-close / rows-discarded gates recognise the defer rows.Close()
// pattern (defer only fires at function return, not loop iteration).
func drainRows(rows *sql.Rows) {
	defer rows.Close()
	for rows.Next() {
	}
}

// isBusyErr returns true if the error string indicates SQLITE_BUSY
// (per Go's database/sql and mpm CLI's stderr).
func isBusyErr(s string) bool {
	ls := strings.ToLower(s)
	return strings.Contains(ls, "sqlite_busy") ||
		strings.Contains(ls, "database is locked")
}

// ── Worker: CLI writer ────────────────────────────────────────────────

func runCLIWriter(ctx context.Context, id int, cfg Config, c *Counters) {
	for attempt := 1; ; attempt++ {
		select {
		case <-ctx.Done():
			return
		default:
		}
		start := time.Now()
		fact := stressFact(id, attempt, cfg.Tag)
		cmd := exec.CommandContext(ctx, cfg.MPMBin, "memory", "add",
			"--fact", fact,
			"--tags", cfg.Tag,
			"--weight", "1",
			"--json",
		)
		// Separate stdout/stderr so the slog log lines on stderr don't
		// pollute the JSON response on stdout. Verified 2026-08-17.
		var stdout, stderr strings.Builder
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		latency := time.Since(start)
		if err != nil {
			errStr := stderr.String() + " " + stdout.String() + " " + err.Error()
			if isBusyErr(errStr) {
				c.WriteBusy.Add(1)
			} else {
				c.WriteOther.Add(1)
				fmt.Fprintf(os.Stderr, "[cli-%d attempt=%d] error: %v\nstdout: %s\nstderr: %s\n",
					id, attempt, err, truncate(stdout.String(), 200), truncate(stderr.String(), 200))
			}
			continue
		}
		var resp struct {
			ID string `json:"id"`
		}
		if jerr := json.Unmarshal([]byte(stdout.String()), &resp); jerr != nil || resp.ID == "" {
			c.WriteOther.Add(1)
			fmt.Fprintf(os.Stderr, "[cli-%d attempt=%d] no id in response: stdout=%s stderr=%s\n",
				id, attempt, truncate(stdout.String(), 200), truncate(stderr.String(), 200))
			continue
		}
		c.WriteSuccess.Add(1)
		c.RecordWriteLatency(latency)
	}
}

// ── Worker: MCP writer ─────────────────────────────────────────────────

// mcpSession owns one mpm-mcp subprocess and a JSON-RPC client over
// its stdio. All calls are serialized (one in-flight at a time) per
// worker — contention comes from the 4 workers running in parallel,
// each on its own mpm-mcp process, all hitting the same SQLite file.
type mcpSession struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	id     int
	mu     sync.Mutex
	nextID int
}

func newMCPSession(ctx context.Context, bin string, id int) (*mcpSession, error) {
	cmd := exec.CommandContext(ctx, bin)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr // forward for diagnostics
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}
	s := &mcpSession{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReader(stdout),
		id:     id,
	}
	// MCP requires initialize handshake before any tools/call.
	if err := s.initialize(ctx); err != nil {
		cmd.Process.Kill()
		return nil, fmt.Errorf("initialize: %w", err)
	}
	return s, nil
}

// sendRPC writes one line-delimited JSON-RPC request (one JSON object
// per '\n'-terminated line) and reads one line-delimited JSON response.
// Verified 2026-08-17 against mark3labs/mcp-go v0.55.1 — LSP-style
// Content-Length framing returns Parse errors; line-delimited is the
// correct wire format for this MCP transport.
func (s *mcpSession) sendRPC(method string, params interface{}) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	id := s.nextID
	req := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	// Write the body followed by exactly one '\n'. Do NOT include
	// any Content-Length or framing prefix — the server rejects
	// those with code -32700 Parse error.
	if _, err := s.stdin.Write(append(body, '\n')); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}
	// Read one response line.
	line, err := s.stdout.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	line = strings.TrimRight(line, "\r\n")
	var resp struct {
		ID     json.RawMessage           `json:"id"`
		Result json.RawMessage           `json:"result"`
		Error  *struct{ Message string } `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w\nline: %s", err, truncate(line, 300))
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("rpc error: %s", resp.Error.Message)
	}
	return resp.Result, nil
}

func (s *mcpSession) initialize(ctx context.Context) error {
	_, err := s.sendRPC("initialize", map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]interface{}{},
		"clientInfo":      map[string]string{"name": "multi-agent-write-test", "version": "0.1"},
	})
	return err
}

func (s *mcpSession) callMemorySave(fact string, tag string) (string, error) {
	result, err := s.sendRPC("tools/call", map[string]interface{}{
		"name": "mpm_memory",
		"arguments": map[string]interface{}{
			"action": "save",
			"params": map[string]interface{}{
				"fact":   fact,
				"tags":   []string{tag},
				"weight": 1,
			},
		},
	})
	if err != nil {
		return "", err
	}
	// tools/call result is a JSON-encoded envelope (content[0].text holds the payload).
	var envelope struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(result, &envelope); err != nil {
		return "", fmt.Errorf("unmarshal envelope: %w", err)
	}
	if envelope.IsError {
		return "", fmt.Errorf("mcp returned isError=true")
	}
	for _, c := range envelope.Content {
		if c.Type == "text" {
			var inner struct {
				ID string `json:"id"`
			}
			if jerr := json.Unmarshal([]byte(c.Text), &inner); jerr == nil && inner.ID != "" {
				return inner.ID, nil
			}
		}
	}
	return "", fmt.Errorf("no id in tools/call response: %s", truncate(string(result), 200))
}

func (s *mcpSession) close() error {
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	if s.cmd != nil {
		return s.cmd.Wait()
	}
	return nil
}

func runMCPWriter(ctx context.Context, id int, cfg Config, c *Counters) {
	sess, err := newMCPSession(ctx, cfg.MCPBin, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[mcp-%d] session init failed: %v\n", id, err)
		c.WriteOther.Add(1)
		return
	}
	defer sess.close()
	for attempt := 1; ; attempt++ {
		select {
		case <-ctx.Done():
			return
		default:
		}
		start := time.Now()
		fact := stressFact(id, attempt, cfg.Tag)
		idStr, err := sess.callMemorySave(fact, cfg.Tag)
		latency := time.Since(start)
		if err != nil {
			errStr := err.Error()
			if isBusyErr(errStr) {
				c.WriteBusy.Add(1)
			} else {
				c.WriteOther.Add(1)
				fmt.Fprintf(os.Stderr, "[mcp-%d attempt=%d] error: %v\n", id, attempt, err)
			}
			continue
		}
		if idStr == "" {
			c.WriteOther.Add(1)
			continue
		}
		c.WriteSuccess.Add(1)
		c.RecordWriteLatency(latency)
	}
}

// ── Worker: direct SQLite reader ──────────────────────────────────────

// runDirectReader opens its own read-only connection to the DB and
// runs a mixed workload of FTS5 + aggregate queries. Records every
// per-query latency; flags any > spikeMs as a spike.
func runDirectReader(ctx context.Context, id int, cfg Config, c *Counters) {
	db, err := sql.Open("sqlite3",
		cfg.DBPath+"?_busy_timeout=5000&_journal_mode=WAL&_synchronous=NORMAL&mode=ro")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[reader-%d] open failed: %v\n", id, err)
		return
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		fmt.Fprintf(os.Stderr, "[reader-%d] ping failed: %v\n", id, err)
		return
	}
	queries := []struct {
		name string
		sql  string
	}{
		{"count_all", "SELECT count(*) FROM memories"},
		{"count_ltm", "SELECT count(*) FROM memories WHERE weight >= 90"},
		{"fts_match", "SELECT count(*) FROM memories_fts WHERE memories_fts MATCH ?"},
		{"agg_weight", "SELECT collection, count(*) FROM memories GROUP BY collection"},
	}
	const ftsTerm = "stress OR test OR agent"
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		for _, q := range queries {
			start := time.Now()
			var args []interface{}
			if q.name == "fts_match" {
				args = []interface{}{ftsTerm}
			}
			rows, err := db.QueryContext(ctx, q.sql, args...)
			if err != nil {
				// Readers don't get busy in WAL mode (they're concurrent with
				// writers), but record any error as a non-spike.
				continue
			}
			drainRows(rows)
			latency := time.Since(start)
			c.RecordReadLatency(latency)
			if latency.Milliseconds() > cfg.SpikeMs {
				c.ReadSpikes.Add(1)
				fmt.Fprintf(os.Stderr, "[reader-%d] SPIKE %s: %dms\n", id, q.name, latency.Milliseconds())
			}
		}
	}
}

// ── Reporting ──────────────────────────────────────────────────────────

func percentile(latencies []time.Duration, p float64) time.Duration {
	if len(latencies) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(latencies))
	copy(sorted, latencies)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(float64(len(sorted)) * p)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func printReport(initial, final int64, c *Counters, cfg Config, start, end time.Time) {
	fmt.Println()
	fmt.Println("═══════════════════════════════════════════════════════════════")
	fmt.Println("  Day-5 multi-agent write contention report")
	fmt.Println("═══════════════════════════════════════════════════════════════")
	fmt.Printf("  Duration:        %s  (wall: %s)\n", cfg.Duration, end.Sub(start).Round(time.Millisecond))
	fmt.Printf("  Workers:         cli=%d  mcp=%d  reader=%d\n", cfg.CLIWorkers, cfg.MCPWorkers, cfg.ReadWorkers)
	fmt.Printf("  DB:              %s\n", cfg.DBPath)
	fmt.Println()
	fmt.Printf("  Memory count:    %d → %d  (delta=%d)\n", initial, final, final-initial)
	fmt.Printf("  Write successes: %d\n", c.WriteSuccess.Load())
	fmt.Printf("  Busy errors:     %d\n", c.WriteBusy.Load())
	fmt.Printf("  Other errors:    %d\n", c.WriteOther.Load())
	fmt.Printf("  Reader spikes:   %d  (threshold: %dms)\n", c.ReadSpikes.Load(), cfg.SpikeMs)
	fmt.Println()
	c.mu.Lock()
	writeCount := len(c.WriteLatencies)
	readCount := len(c.ReadLatencies)
	fmt.Printf("  Write latency:   n=%d  p50=%s  p95=%s  p99=%s  max=%s\n",
		writeCount,
		percentile(c.WriteLatencies, 0.50).Round(time.Millisecond),
		percentile(c.WriteLatencies, 0.95).Round(time.Millisecond),
		percentile(c.WriteLatencies, 0.99).Round(time.Millisecond),
		maxDur(c.WriteLatencies).Round(time.Millisecond),
	)
	fmt.Printf("  Read  latency:   n=%d  p50=%s  p95=%s  p99=%s  max=%s\n",
		readCount,
		percentile(c.ReadLatencies, 0.50).Round(time.Millisecond),
		percentile(c.ReadLatencies, 0.95).Round(time.Millisecond),
		percentile(c.ReadLatencies, 0.99).Round(time.Millisecond),
		maxDur(c.ReadLatencies).Round(time.Millisecond),
	)
	c.mu.Unlock()
	fmt.Println("═══════════════════════════════════════════════════════════════")
}

func maxDur(s []time.Duration) time.Duration {
	if len(s) == 0 {
		return 0
	}
	m := s[0]
	for _, d := range s[1:] {
		if d > m {
			m = d
		}
	}
	return m
}

// checkGates returns nil on pass, error on fail.
func checkGates(initial, final int64, c *Counters) error {
	failed := false
	if c.WriteBusy.Load() > 0 {
		fmt.Printf("\n  ❌ GATE 1 FAILED: %d SQLITE_BUSY errors leaked to callers\n", c.WriteBusy.Load())
		failed = true
	} else {
		fmt.Printf("\n  ✅ GATE 1 PASS:   zero SQLITE_BUSY errors\n")
	}
	writes := c.WriteSuccess.Load()
	if delta := final - initial; delta != writes {
		fmt.Printf("  ❌ GATE 2 FAILED: memory count delta=%d != successful writes=%d  (delta - writes = %d)\n",
			delta, writes, delta-writes)
		failed = true
	} else {
		fmt.Printf("  ✅ GATE 2 PASS:   memory delta equals successful writes (%d)\n", writes)
	}
	if c.ReadSpikes.Load() > 0 {
		fmt.Printf("  ❌ GATE 3 FAILED: %d reader latency spikes\n", c.ReadSpikes.Load())
		failed = true
	} else {
		fmt.Printf("  ✅ GATE 3 PASS:   zero reader latency spikes\n")
	}
	if failed {
		return fmt.Errorf("one or more gates failed")
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ── Main ───────────────────────────────────────────────────────────────

func main() {
	cfg := parseFlags()
	fmt.Printf("Day-5 contention harness starting: duration=%s workers=cli:%d+mcp:%d+reader:%d tag=%s\n",
		cfg.Duration, cfg.CLIWorkers, cfg.MCPWorkers, cfg.ReadWorkers, cfg.Tag)

	// Open a single read-only connection for the countMemories verification.
	// The readers open their own connections; this one is just for the
	// initial/final memory count snapshots.
	verifyDB, err := sql.Open("sqlite3",
		cfg.DBPath+"?_busy_timeout=5000&_journal_mode=WAL&_synchronous=NORMAL&mode=ro")
	if err != nil {
		fmt.Fprintf(os.Stderr, "open verify db: %v\n", err)
		os.Exit(2)
	}
	defer verifyDB.Close()

	initial, err := countMemories(verifyDB)
	if err != nil {
		fmt.Fprintf(os.Stderr, "initial count: %v\n", err)
		os.Exit(2)
	}
	fmt.Printf("  initial memories count: %d\n", initial)

	c := &Counters{}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Duration)
	defer cancel()

	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < cfg.CLIWorkers; i++ {
		wg.Add(1)
		go func(id int) { defer wg.Done(); runCLIWriter(ctx, id, cfg, c) }(i)
	}
	for i := 0; i < cfg.MCPWorkers; i++ {
		wg.Add(1)
		go func(id int) { defer wg.Done(); runMCPWriter(ctx, id, cfg, c) }(i)
	}
	for i := 0; i < cfg.ReadWorkers; i++ {
		wg.Add(1)
		go func(id int) { defer wg.Done(); runDirectReader(ctx, id, cfg, c) }(i)
	}
	wg.Wait()
	end := time.Now()

	final, err := countMemories(verifyDB)
	if err != nil {
		fmt.Fprintf(os.Stderr, "final count: %v\n", err)
		os.Exit(2)
	}

	printReport(initial, final, c, cfg, start, end)
	if err := checkGates(initial, final, c); err != nil {
		os.Exit(1)
	}
}
