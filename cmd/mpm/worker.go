package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"mpm/internal/config"
	mpminternal "mpm/internal"
)

// WatchEventType categorizes background events from the watcher or pollers.
type WatchEventType int

const (
	EventMarkdownFile         WatchEventType = iota // .md file created/written
	EventOrphanSweep                                // new session created → sweep for orphan .jsonl
	EventSystemConfig                               // workspace.json/config.json changed
	EventExternalDBPoll                             // external SQLite DB has new rows
	EventStartupSweep                               // initial startup sweep
	EventTopicClusterCheck                          // manual topic clustering trigger
	EventReconciliationSweep                        // periodic sweep for files missed by fsnotify
)

// WatchEvent is a unit of work pushed into the job queue by the watcher goroutine
// or external DB pollers. Workers process these events using the shared DB.
type WatchEvent struct {
	Type     WatchEventType
	Path     string // file path for file-based events
	Label    string // DB label for external DB events
	DryRun   bool
	Verbose  bool
}

// WorkerPool manages a fixed set of goroutines that process WatchEvents
// from the job queue, sharing a single DatabaseManager.
type WorkerPool struct {
	dm        *mpminternal.DatabaseManager
	jobQueue  chan WatchEvent
	wg        sync.WaitGroup
	quit      chan struct{}
	active    atomic.Int64
	processed atomic.Int64
	maxWorkers int
}

// NewWorkerPool creates a pool with the given number of workers.
// The jobQueue buffer size equals maxWorkers to allow reasonable backlog.
func NewWorkerPool(dm *mpminternal.DatabaseManager, maxWorkers int) *WorkerPool {
	bufSize := maxWorkers * 2
	if bufSize < 16 {
		bufSize = 16
	}
	return &WorkerPool{
		dm:         dm,
		jobQueue:   make(chan WatchEvent, bufSize),
		quit:       make(chan struct{}),
		maxWorkers: maxWorkers,
	}
}

// Start launches worker goroutines.
func (wp *WorkerPool) Start(ctx context.Context) {
	for i := 0; i < wp.maxWorkers; i++ {
		wp.wg.Add(1)
		go wp.worker(ctx, i)
	}
}

// Submit enqueues an event. Returns false if the queue is full.
func (wp *WorkerPool) Submit(ev WatchEvent) bool {
	select {
	case wp.jobQueue <- ev:
		return true
	default:
		return false
	}
}

// Stop signals all workers to stop after draining the queue.
func (wp *WorkerPool) Stop() {
	close(wp.quit)
	wp.wg.Wait()
}

// QueueSize returns the current length of the job queue.
func (wp *WorkerPool) QueueSize() int {
	return len(wp.jobQueue)
}

// DM returns the worker's DatabaseManager. Used to initialize sibling
// workers that share the same DB connection (e.g., the synthesis worker).
func (wp *WorkerPool) DM() *mpminternal.DatabaseManager {
	return wp.dm
}

// ProcessedCount returns the total number of events processed.
func (wp *WorkerPool) ProcessedCount() int64 {
	return wp.processed.Load()
}

// ActiveWorkers returns the number of currently busy workers.
func (wp *WorkerPool) ActiveWorkers() int64 {
	return wp.active.Load()
}

func (wp *WorkerPool) worker(ctx context.Context, id int) {
	defer wp.wg.Done()
	for {
		select {
		case <-ctx.Done():
			// Drain remaining events before exiting to avoid data loss.
			for {
				select {
				case ev, ok := <-wp.jobQueue:
					if !ok {
						return
					}
					wp.active.Add(1)
					wp.processEvent(ev)
					wp.active.Add(-1)
					wp.processed.Add(1)
				default:
					return
				}
			}
		case <-wp.quit:
			// Drain remaining events before exiting to avoid data loss.
			for {
				select {
				case ev, ok := <-wp.jobQueue:
					if !ok {
						return
					}
					wp.active.Add(1)
					wp.processEvent(ev)
					wp.active.Add(-1)
					wp.processed.Add(1)
				default:
					return
				}
			}
		case ev, ok := <-wp.jobQueue:
			if !ok {
				return
			}
			wp.active.Add(1)
			wp.processEvent(ev)
			wp.active.Add(-1)
			wp.processed.Add(1)
		}
	}
}

func (wp *WorkerPool) processEvent(ev WatchEvent) {
	switch ev.Type {
	case EventMarkdownFile:
		processFileEvent(wp.dm, ev)
	case EventSystemConfig:
		processConfigEvent(wp.dm, ev)
	case EventExternalDBPoll:
		processExternalDBPoll(wp.dm, ev)
	case EventStartupSweep:
		processStartupSweep(wp.dm, ev)
	case EventTopicClusterCheck:
		processTopicClusterCheck(wp.dm, ev)
	case EventOrphanSweep:
		processOrphanSweep(wp.dm, ev)
	case EventReconciliationSweep:
		processReconciliationSweep(wp.dm, ev)
	}
}

// ============================================================================
// Event Processors (shared between worker pool and legacy watch daemon)
// ============================================================================

func processFileEvent(dm *mpminternal.DatabaseManager, ev WatchEvent) {
	memory := mpminternal.NewMemoryStore("")
	wd := &watcherDaemon{
		db:           dm,
		memory:       memory,
		dryRun:       ev.DryRun,
		verbose:      ev.Verbose,
		synthWorker:  watchSynthWorker, // isolated synthesis pool
	}
	wd.processMarkdownFile(ev.Path, false)
}

func processConfigEvent(dm *mpminternal.DatabaseManager, ev WatchEvent) {
	memory := mpminternal.NewMemoryStore("")
	wd := &watcherDaemon{
		db:      dm,
		memory:  memory,
		dryRun:  ev.DryRun,
		verbose: ev.Verbose,
	}
	wd.processSessionsConfig(ev.Path)
}

func processExternalDBPoll(dm *mpminternal.DatabaseManager, ev WatchEvent) {
	// Find the external DB config by label
	cfg, err := config.LoadConfig()
	if err != nil {
		if ev.Verbose {
			fmt.Fprintf(os.Stderr, "⚠️  External DB poll %s: cannot load config: %v\n", ev.Label, err)
		}
		return
	}
	for _, dbc := range cfg.GetExternalDbs() {
		if dbc.Label == ev.Label {
			pollOnce(&dbc, dm, ev.DryRun, ev.Verbose)
			return
		}
	}
	if ev.Verbose {
		fmt.Fprintf(os.Stderr, "⚠️  External DB poll %s: config not found\n", ev.Label)
	}
}

func processStartupSweep(dm *mpminternal.DatabaseManager, ev WatchEvent) {
	memory := mpminternal.NewMemoryStore("")
	// Resolve directories to sweep
	dirs := resolveWatchDirs("", "")
	wd := &watcherDaemon{
		db:      dm,
		memory:  memory,
		dryRun:  ev.DryRun,
		verbose: ev.Verbose,
	}
	for _, dir := range dirs {
		wd.sweepDirectory(dir)
	}
}

func processTopicClusterCheck(dm *mpminternal.DatabaseManager, ev WatchEvent) {
	memory := mpminternal.NewMemoryStore("")
	wd := &watcherDaemon{
		db:      dm,
		memory:  memory,
		dryRun:  ev.DryRun,
		verbose: ev.Verbose,
	}
	wd.checkTopicClustering()
}

// processOrphanSweep scans a session directory for orphan .jsonl files (no
// matching .lock) and processes each one: fact extraction → save to DB →
// LLM synthesis → archive the .jsonl to archive/.
func processOrphanSweep(dm *mpminternal.DatabaseManager, ev WatchEvent) {
	dir := ev.Path
	if dir == "" {
		return
	}
	memory := mpminternal.NewMemoryStore("")
	wd := &watcherDaemon{
		db:      dm,
		memory:  memory,
		dryRun:  ev.DryRun,
		verbose: ev.Verbose,
	}
	wd.sweepOrphanSessions(dir)
}

// processReconciliationSweep performs a periodic background sweep of watch
// directories to find files that fsnotify may have missed (OS buffer overflow,
// transient errors, system sleep/wake cycles). It compares files on disk
// against the source_path stored in memories metadata.
//
// Orphan .md files are processed inline via processMarkdownFile. Orphan
// .jsonl session files trigger an orphanSweep per directory. The sweep is
// intentionally low-priority and single-threaded to avoid overwhelming the
// worker pool or the filesystem.
func processReconciliationSweep(dm *mpminternal.DatabaseManager, ev WatchEvent) {
	dirs := resolveWatchDirs("", "")
	if len(dirs) == 0 {
		return
	}

	memory := mpminternal.NewMemoryStore("")
	wd := &watcherDaemon{
		db:      dm,
		memory:  memory,
		dryRun:  ev.DryRun,
		verbose: ev.Verbose,
	}
	// Throttle: process at most 25 files per sweep to avoid CPU spikes
	processed := 0
	const maxPerSweep = 25

	for _, dir := range dirs {
		if processed >= maxPerSweep {
			break
		}
		n := reconcileDirectory(wd, dm, dir, maxPerSweep-processed)
		processed += n
	}

	if ev.Verbose && processed > 0 {
		fmt.Printf("   🔄 Reconciliation sweep: %d orphan files processed\n", processed)
	}
}

// reconcileDirectory lists files in dir and processes any that have not been
// ingested (checked by source_path in memories metadata). Returns count.
func reconcileDirectory(wd *watcherDaemon, dm *mpminternal.DatabaseManager, dir string, max int) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if wd.verbose {
			fmt.Fprintf(os.Stderr, "   ⚠️  Reconciliation sweep: cannot read %s: %v\n", dir, err)
		}
		return 0
	}

	count := 0
	for _, entry := range entries {
		if count >= max {
			break
		}
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		path := filepath.Join(dir, name)

		// Skip conflicted files and system files (same filters as sweepDirectory)
		nameLower := strings.ToLower(name)
		if strings.Contains(nameLower, "conflicted") {
			continue
		}
		switch nameLower {
		case "sessions.json", "session.json", "workspace.json", "config.json":
			continue
		}

		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".md" && ext != ".jsonl" {
			continue
		}

		// Check if this file has already been ingested
		alreadyIngested, err := isFileIngested(dm, path)
		if err != nil || alreadyIngested {
			continue
		}

		switch ext {
		case ".md":
			wd.processMarkdownFile(path, false)
			count++
		case ".jsonl":
			// For .jsonl, trigger an orphan sweep on the directory
			wd.sweepOrphanSessions(dir)
			count++
		}
	}
	return count
}

// isFileIngested checks whether a file at the given path has already been
// stored in the memories table (matched by source_path in metadata JSON).
func isFileIngested(dm *mpminternal.DatabaseManager, filePath string) (bool, error) {
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		absPath = filePath
	}
	var count int
	err = dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE json_extract(metadata, '$.source_path') = ?`,
		absPath,
	).Scan(&count)
	if err != nil {
		// json_extract may not be available on all builds; fall back to LIKE
		if strings.Contains(err.Error(), "no such function") {
			escaped := strings.ReplaceAll(absPath, `'`, `''`)
			escaped = strings.ReplaceAll(escaped, `%`, `\%`)
			escaped = strings.ReplaceAll(escaped, `_`, `\_`)
			err = dm.SQLDB().QueryRow(
				`SELECT COUNT(*) FROM memories WHERE metadata LIKE '%"source_path":"`+escaped+`"%'`,
			).Scan(&count)
		}
		if err != nil {
			return false, err
		}
	}
	return count > 0, nil
}
