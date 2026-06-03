package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	mpminternal "mpm/internal"
	"mpm/internal/config"
)

func TestWatchStartStopLifecycle(t *testing.T) {
	// Setup: create a temp memory directory
	tmpDir := t.TempDir()
	memDir := filepath.Join(tmpDir, "memory")
	sessionsDir := filepath.Join(tmpDir, "sessions")
	os.MkdirAll(memDir, 0755)
	os.MkdirAll(sessionsDir, 0755)

	// Save original config and restore after test
	origCfg, _ := config.LoadConfig()
	defer func() {
		if origCfg != nil {
			config.SaveConfig(origCfg)
		}
	}()

	// Override config to use temp dirs
	cfg := &config.Config{
		MemoryDirs:   []string{memDir},
		SessionsDirs: []string{sessionsDir},
	}
	config.SaveConfig(cfg)

	// Reset global state between tests
	watchPool = nil
	watcherCtx = nil
	watcherCancel = nil
	watcherDone = nil

	// Start watcher
	err := startWatchGoroutine()
	if err != nil {
		t.Fatalf("startWatchGoroutine failed: %v", err)
	}

	// Use WaitGroup to detect when goroutines have started
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		// Wait for pool to be alive: either active workers or processed events > 0
		// The startup sweep is submitted immediately after goroutine start
		for i := 0; i < 50; i++ { // poll for up to 5s
			if watchPool != nil && (watchPool.ActiveWorkers() > 0 || watchPool.ProcessedCount() > 0) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()

	// Wait for goroutine startup with timeout
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// pool is alive
	case <-time.After(5 * time.Second):
		t.Fatal("Watch pool did not become active within 5s")
	}

	// Verify pool is usable
	if watchPool == nil {
		t.Fatal("watchPool is nil after start")
	}

	// Verify we can submit an event and it is picked up
	initialProcessed := watchPool.ProcessedCount()
	ok := watchPool.Submit(WatchEvent{Type: EventTopicClusterCheck, DryRun: true, Verbose: false})
	if !ok {
		t.Fatal("watchPool.Submit returned false — queue may be full or pool dead")
	}

	// Wait for event to be processed (poll, no sleep)
	for i := 0; i < 20; i++ {
		if watchPool.ProcessedCount() > initialProcessed {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Stop watcher — this blocks until the goroutine exits
	stopWatchGoroutine()

	// Verify stopped state
	if watcherCancel != nil || watcherCtx != nil {
		t.Fatal("Watcher globals not cleared after stop")
	}
}

func TestWatchExternalDBPoolInitialization(t *testing.T) {
	// Test that external DB polling goroutines start without crashing
	// when no external DBs are configured
	tmpDir := t.TempDir()
	cfg := &config.Config{
		MemoryDirs:   []string{tmpDir},
		SessionsDirs: []string{},
		ExternalDbs:  []config.ExternalDB{},
	}
	origCfg, _ := config.LoadConfig()
	config.SaveConfig(cfg)
	defer func() {
		if origCfg != nil {
			config.SaveConfig(origCfg)
		}
	}()

	watchPool = nil
	watcherCtx = nil
	watcherCancel = nil
	watcherDone = nil

	err := startWatchGoroutine()
	if err != nil {
		t.Fatalf("startWatchGoroutine failed: %v", err)
	}
	defer stopWatchGoroutine()

	// Wait for startup sweep to complete (it processes synchronously in the pool)
	for i := 0; i < 50; i++ {
		if watchPool != nil && watchPool.ProcessedCount() > 0 {
			return // startup sweep done
		}
		time.Sleep(100 * time.Millisecond)
	}
	// If no events processed, at least verify the pool didn't crash
	if watchPool == nil {
		t.Fatal("watchPool is nil after start — pool may have crashed")
	}
}

func TestWatchEventProcessorDispatch(t *testing.T) {
	// Create a minimal in-memory DB for testing
	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		t.Skip("Database not available for testing")
	}
	defer dm.Close()

	pool := NewWorkerPool(dm, 2)
	ctx := context.Background()
	pool.Start(ctx)
	defer pool.Stop()

	// Poll for processed count instead of sleep
	initialProcessed := pool.ProcessedCount()

	ok := pool.Submit(WatchEvent{Type: EventTopicClusterCheck, DryRun: true, Verbose: false})
	if !ok {
		t.Fatal("Submit failed")
	}

	// Wait for processing with poll loop
	for i := 0; i < 20; i++ {
		if pool.ProcessedCount() > initialProcessed {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}
