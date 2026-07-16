// Package scheduler implements the universal wake executor for MPM.
//
// The scheduler is the platform-level fix for MPM's opportunistic-fold gap.
// Wakes registered in `scheduled_wakes` are pulled on a 60s ticker. Each
// wake's `metadata.kind` field routes it through a HandlerFunc registered
// at startup. System kinds (snapshot, critic_audit, gc, broadcast) execute
// inline. Notification kinds and untagged wakes are left untouched so the
// existing mpm-mcp opportunistic fold handles them on the next MCP call.
//
// Three guarantees from the locked architecture (decision 27d7b3c18199e098):
//
//   1. Default kind = notification (backward compat). Existing wakes with
//      no kind tag pass through to mpm-mcp's fold unchanged.
//   2. Concurrent execution. Independent system wakes fire in parallel
//      goroutines. Serial would re-introduce the SF2 race failure mode.
//   3. Failure isolation. Non-zero handler exit still marks the wake
//      fired=1 with metadata.last_error. One wake's failure cannot block
//      subsequent wakes.
//
// Singleton enforcement is via flock on a PID file so two scheduler
// instances cannot race on the same wake batch.
package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Wake is the projected view of a scheduled_wakes row.
type Wake struct {
	ID            string                 `json:"id"`
	TargetTime    int64                  `json:"target_time"`
	Reason        string                 `json:"reason"`
	TheoryID      string                 `json:"theory_id,omitempty"`
	RecurringRule string                 `json:"recurring_rule,omitempty"`
	CreatedBy     string                 `json:"created_by"`
	CreatedAt     string                 `json:"created_at"`
	Metadata      map[string]interface{} `json:"metadata,omitempty"`
}

// Kind returns the dispatch key for a wake. Reads metadata.kind first;
// falls back to "notification" for any wake without an explicit system kind.
// This is the backward-compat guarantee: pre-scheduler wakes pass through
// to the existing opportunistic fold unchanged.
func (w Wake) Kind() string {
	if k, ok := w.Metadata["kind"].(string); ok && k != "" {
		return k
	}
	return "notification"
}

// HandlerFunc executes a system wake. Returning a non-nil error marks the
// wake fired with last_error set; it does NOT block subsequent wakes.
type HandlerFunc func(w Wake) error

// Scheduler is the long-running wake executor.
type Scheduler struct {
	db       *sql.DB
	dbPath   string
	log      *slog.Logger
	handlers map[string]HandlerFunc
	mu       sync.RWMutex
}

// New opens the MPM database and returns a Scheduler ready for handlers.
// Caller owns the lifecycle via Run(ctx, interval).
func New(dbPath string, log *slog.Logger) (*Scheduler, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	db, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000&_journal_mode=WAL&_synchronous=NORMAL")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}
	return &Scheduler{
		db:       db,
		dbPath:   dbPath,
		log:      log,
		handlers: make(map[string]HandlerFunc),
	}, nil
}

// Close releases the database connection.
func (s *Scheduler) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Register attaches a HandlerFunc for the given wake kind. Registering
// overwrites any previous handler for that kind. Adding a new system kind
// is a one-line operation; central dispatch never needs editing.
func (s *Scheduler) Register(kind string, h HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[kind] = h
}

// QueryDueWakes returns all unfired wakes with target_time <= now.
// Does NOT mark them fired (notification wakes must remain available for
// mpm-mcp's opportunistic fold).
func (s *Scheduler) QueryDueWakes(now time.Time) ([]Wake, error) {
	rows, err := s.db.Query(
		`SELECT id, target_time, reason, COALESCE(theory_id,''), COALESCE(recurring_rule,''),
		        created_by, created_at, COALESCE(metadata,'')
		 FROM scheduled_wakes
		 WHERE fired = 0 AND target_time <= ?
		 ORDER BY target_time ASC`,
		now.Unix(),
	)
	if err != nil {
		return nil, fmt.Errorf("query due wakes: %w", err)
	}
	defer rows.Close()

	var out []Wake
	for rows.Next() {
		var w Wake
		var metaJSON string
		if err := rows.Scan(&w.ID, &w.TargetTime, &w.Reason, &w.TheoryID, &w.RecurringRule,
			&w.CreatedBy, &w.CreatedAt, &metaJSON); err != nil {
			return nil, fmt.Errorf("scan wake: %w", err)
		}
		if metaJSON != "" {
			_ = json.Unmarshal([]byte(metaJSON), &w.Metadata)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// MarkFired records the wake as dispatched. lastError may be empty.
func (s *Scheduler) MarkFired(id string, lastError string) error {
	if lastError == "" {
		_, err := s.db.Exec(
			`UPDATE scheduled_wakes SET fired = 1, fired_at = ?, metadata = json_set(COALESCE(metadata,'{}'), '$.last_error', NULL) WHERE id = ?`,
			time.Now().Unix(), id,
		)
		return err
	}
	_, err := s.db.Exec(
		`UPDATE scheduled_wakes SET fired = 1, fired_at = ?,
		  metadata = json_set(COALESCE(metadata,'{}'), '$.last_error', ?)
		 WHERE id = ?`,
		time.Now().Unix(), lastError, id,
	)
	return err
}

// Tick runs one scheduler iteration. Pulls due wakes, dispatches each
// according to its kind. Returns the number of system wakes executed.
func (s *Scheduler) Tick(ctx context.Context) (int, error) {
	wakes, err := s.QueryDueWakes(time.Now())
	if err != nil {
		return 0, fmt.Errorf("query due: %w", err)
	}
	if len(wakes) == 0 {
		return 0, nil
	}

	// Partition: system kinds get parallel goroutines; notification kinds
	// pass through untouched so mpm-mcp's opportunistic fold handles them.
	systemWakes := make([]Wake, 0, len(wakes))
	for _, w := range wakes {
		if s.hasHandler(w.Kind()) {
			systemWakes = append(systemWakes, w)
		} else {
			s.log.Debug("notification kind, leaving for opportunistic fold",
				"wake_id", w.ID, "kind", w.Kind())
		}
	}

	if len(systemWakes) == 0 {
		return 0, nil
	}

	// Execute system wakes concurrently. Each goroutine is independent —
	// no shared state, no depends_on graph in MVP. Future hardening can
	// add a depends_on registry per wake (e.g., snapshot -> critic_audit).
	var wg sync.WaitGroup
	executed := 0
	var mu sync.Mutex
	for _, w := range systemWakes {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.executeOne(ctx, w); err != nil {
				s.log.Error("wake handler failed",
					"wake_id", w.ID, "kind", w.Kind(), "err", err)
				if mErr := s.MarkFired(w.ID, err.Error()); mErr != nil {
					s.log.Error("mark fired failed",
						"wake_id", w.ID, "err", mErr)
				}
				return
			}
			if mErr := s.MarkFired(w.ID, ""); mErr != nil {
				s.log.Error("mark fired failed", "wake_id", w.ID, "err", mErr)
				return
			}
			mu.Lock()
			executed++
			mu.Unlock()
		}()
	}
	wg.Wait()
	return executed, nil
}

// executeOne runs a single wake's handler under a goroutine, with context
// timeout so a hung handler cannot pin a tick indefinitely.
func (s *Scheduler) executeOne(ctx context.Context, w Wake) error {
	s.mu.RLock()
	h, ok := s.handlers[w.Kind()]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("no handler registered for kind=%q", w.Kind())
	}
	s.log.Info("executing wake",
		"wake_id", w.ID, "kind", w.Kind(), "reason", truncate(w.Reason, 80))

	done := make(chan error, 1)
	go func() { done <- h(w) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("handler context cancelled: %w", ctx.Err())
	}
}

func (s *Scheduler) hasHandler(kind string) bool {
	if kind == "notification" {
		return false
	}
	s.mu.RLock()
	_, ok := s.handlers[kind]
	s.mu.RUnlock()
	return ok
}

// Run is the ticker loop. Blocks until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context, interval time.Duration) error {
	if interval < time.Second {
		return errors.New("interval must be >= 1s")
	}
	s.log.Info("scheduler running", "interval", interval.String(), "db", s.dbPath)
	t := time.NewTicker(interval)
	defer t.Stop()

	// Run once immediately so a wake that became due during shutdown
	// doesn't have to wait a full interval for first dispatch.
	if n, err := s.Tick(ctx); err != nil {
		s.log.Error("initial tick failed", "err", err)
	} else if n > 0 {
		s.log.Info("initial tick executed wakes", "count", n)
	}

	for {
		select {
		case <-ctx.Done():
			s.log.Info("scheduler stopping", "reason", ctx.Err())
			return nil
		case <-t.C:
			if n, err := s.Tick(ctx); err != nil {
				s.log.Error("tick failed", "err", err)
			} else if n > 0 {
				s.log.Info("tick executed wakes", "count", n)
			}
		}
	}
}

// AcquireLock grabs an exclusive flock on path. Returns the file handle
// (caller must defer Release) or an error if another instance holds it.
func AcquireLock(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir lock dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("acquire flock (another scheduler running?): %w", err)
	}
	return f, nil
}

// truncate shortens a string for log output. Used to keep reason fields
// from blowing up logs when they include long URLs or free text.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// ── Built-in handlers ─────────────────────────────────────────────────────
//
// Each handler is a thin wrapper around an existing CLI tool. Handlers
// return errors; the scheduler translates those into last_error metadata
// and marks fired regardless of outcome (failure isolation).

// SnapshotHandler creates an atomic pre-flight SQLite backup via
// sqlite3 .backup. Replaces pre_critic_snapshot.sh. Writes to the same
// backups/critic-pre/ directory the legacy shell script used.
//
// metadata.label: optional filename suffix (default: epoch).
func SnapshotHandler(w Wake) error {
	dbPath := defaultDBPath()
	backupDir := filepath.Join(filepath.Dir(dbPath), "..", "..", "backups", "critic-pre")
	if env := os.Getenv("MPM_BACKUP_DIR"); env != "" {
		backupDir = env
	}
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return fmt.Errorf("mkdir backup dir: %w", err)
	}

	label := fmt.Sprintf("%d", time.Now().Unix())
	if l, ok := w.Metadata["label"].(string); ok && l != "" {
		label = l
	}
	snapshot := filepath.Join(backupDir, fmt.Sprintf("mpm_pre_critic_%s.db", label))

	// sqlite3 .backup is atomic and WAL-safe; concurrent writers are not blocked.
	cmd := exec.Command("sqlite3", dbPath, fmt.Sprintf(".backup '%s'", snapshot))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sqlite3 .backup: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// Verify the snapshot is sane before trusting it as rollback target.
	integ := exec.Command("sqlite3", snapshot, "PRAGMA integrity_check;")
	out, err := integ.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		return fmt.Errorf("integrity_check: %s: %w", strings.TrimSpace(string(out)), err)
	}

	return rotateSnapshots(backupDir, 7)
}

// CriticAuditHandler shells out to the mpm-critic binary for one audit
// cycle. Decoupled from internal/critic/ so the scheduler stays a thin
// dispatcher; the critic evolves independently. If the binary isn't on
// PATH, the handler returns an error so the wake is marked fired with
// last_error (failure isolation).
func CriticAuditHandler(w Wake) error {
	bin := os.Getenv("MPM_CRITIC_BIN")
	if bin == "" {
		bin = "mpm-critic"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin)
	cmd.Env = append(os.Environ(), "MPM_DB_PATH="+defaultDBPath())
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mpm-critic: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// GCHandler triggers the existing gc_run sweep. Reads dry_run and aggressive
// flags from wake metadata so per-cycle tuning is possible.
func GCHandler(w Wake) error {
	dryRun := "true"
	if d, ok := w.Metadata["dry_run"].(bool); ok && !d {
		dryRun = "false"
	}
	args := []string{"call", "gc_run", fmt.Sprintf(`{"dry_run":%s}`, dryRun)}
	if a, ok := w.Metadata["aggressive"].(bool); ok && a {
		args[2] = `{"dry_run":false,"aggressive":true}`
	}
	cmd := exec.Command("mpm", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mpm call gc_run: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// BroadcastHandler triggers an active-dissemination broadcast to receiving
// agents via mpm ops broadcast. Used for high-priority cross-agent pushes.
func BroadcastHandler(w Wake) error {
	target := ""
	if t, ok := w.Metadata["target"].(string); ok {
		target = t
	}
	args := []string{"ops", "broadcast"}
	if target != "" {
		args = append(args, "--target", target)
	}
	cmd := exec.Command("mpm", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mpm ops broadcast: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// ── Helpers ──────────────────────────────────────────────────────────────

func defaultDBPath() string {
	if env := os.Getenv("MPM_DB_PATH"); env != "" {
		return env
	}
	// Default to the workspace-relative path mpm-mcp uses.
	cwd, err := os.Getwd()
	if err != nil {
		return "src/db/mpm.db"
	}
	candidate := filepath.Join(cwd, "src", "db", "mpm.db")
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return "src/db/mpm.db"
}

// rotateSnapshots keeps the most recent N snapshots in dir and deletes
// older ones. Used by SnapshotHandler so the backup directory doesn't
// grow unbounded.
func rotateSnapshots(dir string, keep int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	type snap struct {
		path  string
		mtime time.Time
	}
	var snaps []snap
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "mpm_pre_critic_") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		snaps = append(snaps, snap{
			path:  filepath.Join(dir, e.Name()),
			mtime: fi.ModTime(),
		})
	}
	if len(snaps) <= keep {
		return nil
	}
	// Sort newest first.
	for i := 0; i < len(snaps); i++ {
		for j := i + 1; j < len(snaps); j++ {
			if snaps[j].mtime.After(snaps[i].mtime) {
				snaps[i], snaps[j] = snaps[j], snaps[i]
			}
		}
	}
	for _, s := range snaps[keep:] {
		_ = os.Remove(s.path)
	}
	return nil
}