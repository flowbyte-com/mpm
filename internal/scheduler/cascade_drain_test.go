package scheduler

import (
	"log/slog"
	"testing"
	"time"

	core "github.com/flowbyte-com/mpm-core"
)

func TestNewCascadeDrainHandler_AppliesDefaults(t *testing.T) {
	dm := core.NewTestDM(t)
	h := NewCascadeDrainHandler(dm, slog.Default(), CascadeDrainOptions{})

	if got, want := h.Budget(), 30*time.Second; got != want {
		t.Errorf("Budget() = %v, want %v", got, want)
	}
	if got, want := h.BatchSize(), 10; got != want {
		t.Errorf("BatchSize() = %d, want %d", got, want)
	}
}

func TestNewCascadeDrainHandler_HonoursOptions(t *testing.T) {
	dm := core.NewTestDM(t)
	h := NewCascadeDrainHandler(dm, slog.Default(), CascadeDrainOptions{
		Budget:    5 * time.Second,
		BatchSize: 25,
	})
	if got, want := h.Budget(), 5*time.Second; got != want {
		t.Errorf("Budget() = %v, want %v", got, want)
	}
	if got, want := h.BatchSize(), 25; got != want {
		t.Errorf("BatchSize() = %d, want %d", got, want)
	}
}
