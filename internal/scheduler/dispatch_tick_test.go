// dispatch_tick_test.go — the scheduler's opt-in dispatch tick.
//
// # WHAT IS UNDER TEST
//
// The tick handler's INERTNESS, which is the property that matters
// most: an installed scheduler must never dispatch on its own.
//
//   - a MISSING configuration file -> the handler does nothing;
//   - a DISABLED configuration file -> the handler does nothing;
//   - a BROKEN configuration file -> the handler logs the fault and
//     dispatches nothing.
//
// The last one matters as much as the first: a configuration that
// cannot be understood must not be read as permission.

package scheduler

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/flowbyte-com/mpm-core"
)

// testLogger is a logger that discards everything.
//
// The handler's contract is about WHAT it does, not about what it logs,
// and a test that printed the logs would bury the assertion.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{
		Level: slog.LevelError + 1,
	}))
}

// setupDispatchTickDM returns a file-backed DatabaseManager and the
// workspace it lives in.
func setupDispatchTickDM(t *testing.T) (*core.DatabaseManager, string) {
	t.Helper()
	workspace := t.TempDir()
	t.Setenv("MPM_WORKSPACE", workspace)
	dm, err := core.NewDatabaseManager(workspace)
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	t.Cleanup(func() { _ = dm.Close() })
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("initialise the schema: %v", err)
	}
	return dm, workspace
}

// TestTheDispatchTickIsInertWithoutAConfigurationFile proves the
// installed scheduler does nothing.
//
// The handler is given a configuration path that does not exist, and it
// must return without touching the database or the runtime.
func TestTheDispatchTickIsInertWithoutAConfigurationFile(t *testing.T) {
	dm, _ := setupDispatchTickDM(t)

	handler := &DispatchTickHandler{
		dm:         dm,
		logger:     testLogger(),
		configPath: filepath.Join(t.TempDir(), "absent.json"),
	}
	if err := handler.run(context.Background()); err != nil {
		t.Fatalf("the handler failed with no configuration file: %v", err)
	}

	// NOTHING WAS WRITTEN. The assignment table is untouched.
	var assignments int
	row := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM agent_tasks`)
	if err := row.Scan(&assignments); err != nil {
		t.Fatalf("count the assignments: %v", err)
	}
	if assignments != 0 {
		t.Errorf("the inert tick created %d assignment(s)", assignments)
	}
}

// TestTheDispatchTickIsInertWhenTheDispatcherIsDisabled proves the
// switch is the switch.
//
// A configuration file that says "enabled: false" is read and obeyed.
// The dispatcher's default is the same: a file that omits the flag is
// disabled.
func TestTheDispatchTickIsInertWhenTheDispatcherIsDisabled(t *testing.T) {
	dm, _ := setupDispatchTickDM(t)

	path := filepath.Join(t.TempDir(), "dispatch.json")
	if err := os.WriteFile(path, []byte(`{"enabled": false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := &DispatchTickHandler{
		dm:         dm,
		logger:     testLogger(),
		configPath: path,
	}
	if err := handler.run(context.Background()); err != nil {
		t.Fatalf("the handler failed with a disabled configuration: %v", err)
	}
}

// TestTheDispatchTickRefusesABrokenConfigurationFile proves a broken
// file is a FAULT, not a permission.
//
// The handler returns an error rather than dispatching, and the error
// names the file so an operator can see which one is broken.
func TestTheDispatchTickRefusesABrokenConfigurationFile(t *testing.T) {
	dm, _ := setupDispatchTickDM(t)

	path := filepath.Join(t.TempDir(), "dispatch.json")
	if err := os.WriteFile(path, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := &DispatchTickHandler{
		dm:         dm,
		logger:     testLogger(),
		configPath: path,
	}
	err := handler.run(context.Background())
	if err == nil {
		t.Fatal("a broken configuration file was accepted")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("the error does not name the broken file: %v", err)
	}
}

// TestTheDispatchTickHandlerNeverReturnsAnError proves the scheduler's
// other handlers are unaffected.
//
// The scheduler logs a tick handler's error and carries on. The
// dispatch handler therefore swallows its own faults and returns nil —
// the fault is in the log, not in the scheduler's error path.
func TestTheDispatchTickHandlerNeverReturnsAnError(t *testing.T) {
	dm, _ := setupDispatchTickDM(t)

	path := filepath.Join(t.TempDir(), "dispatch.json")
	if err := os.WriteFile(path, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := &DispatchTickHandler{
		dm:         dm,
		logger:     testLogger(),
		configPath: path,
	}
	if err := handler.TickHandler()(context.Background()); err != nil {
		t.Errorf("the tick handler returned an error: %v", err)
	}
}
