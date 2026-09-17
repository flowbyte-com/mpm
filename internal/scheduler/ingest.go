// internal/scheduler/ingest.go
//
// OpenClaw memory-flush ingest handler. Watches the canonical path
// the mpm-memory-openclaw plugin's flushPlanResolver writes to, ingests
// the file as a wake row in scheduled_wakes, and deletes the source.
//
// Architecture (2026-08-13):
//
//   OpenClaw compaction fires (token threshold hit)
//        │
//        ▼
//   mpm-memory-openclaw plugin (Task 2 — flushPlanResolver writes to
//   relativePath: ".mpm/run/ingest.md" relative to workspaceDir, which
//   for the main session resolves to /home/v/.mpm/run/ingest.md)
//        │
//        ▼
//   mpm-scheduler tick (this file)
//        │
//        ▼
//   scheduled_wakes row with metadata.source="openclaw_ingest"
//   (visible on next read_wake_context call via overdue_wakes surface)
//
// Safety rails (added 2026-08-13, see decision ad518f4ac00943ee
// sibling context — filesystems-to-agent-context are a known prompt-
// injection vector, see also lesson 9b349f3869c7eb07):
//
//  1. Path allowlist. Only the exact canonical path is acceptable.
//     No glob, no parent walk, no configuration override.
//  2. Symlink rejection. Lstat — never follow symlinks. A symlink
//     to /etc/passwd or /home/v/.mpm/db/mpm.db must not be
//     ingested as content.
//  3. Size cap. 64KB hard cap; oversize files are quarantined to
//     *.rejected (renamed, not deleted) for forensic inspection.
//  4. Atomic rename. File is renamed to *.processing before read
//     so a partial write mid-tick can't corrupt the read.
//  5. Source attribution. Every wake row carries metadata.source =
//     "openclaw_ingest" so audit trail distinguishes filesystem-to-
//     substrate bridging from explicit user / mpm_call actions.
//  6. Permission re-check. /home/v/.mpm/run/ is 0700 user-only
//     (per AGENTS.md "External vs Internal" — this stays internal
//     but the path is sensitive). Lstat mode bits are inspected;
//     a world-writable file or parent directory is rejected.
//  7. Non-fatal throughout. Every failure path logs and returns
//     nil so the scheduler tick cannot die from this handler.
//
// References:
//   - Decision ad518f4ac00943ee (the wake-context bridge sibling)
//   - Lesson 1d333bf9e0d34d1c (architectural-gap lesson about
//     silent failure at bootstrap surfaces)
//   - Lesson 9b349f3869c7eb07 (AgentBaiting lesson about Skill/MCP
//     server surface-area expansion)

package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"time"

	core "github.com/flowbyte-com/mpm-core"
)

// openclawIngestDefaultPath is the absolute canonical target the
// mpm-memory-openclaw plugin writes to. Kept as a const so the path
// allowlist is structurally enforced, not configurable per-instance
// in production. Changing this requires editing both this file and
// the plugin's flushPlanResolver together.
const openclawIngestDefaultPath = "/home/v/.mpm/run/ingest.md"

// openclawIngestMaxBytes is the hard cap on a single ingest file.
// 64KB is enough for ~16K words of structured markdown — far more
// than a single compaction cycle produces. Oversize is treated as
// attack surface (or worse: a runaway plugin loop) and quarantined,
// not truncated silently.
const openclawIngestMaxBytes = 64 * 1024

// openclawIngestProcessingSuffix is the rename target during the read
// window. Atomic rename ensures no concurrent writer can corrupt the
// snapshot mid-read.
const openclawIngestProcessingSuffix = ".processing"

// openclawIngestRejectedSuffix is the rename target for oversize files.
// Renamed, not deleted, so v can inspect what the plugin tried to send.
const openclawIngestRejectedSuffix = ".rejected"

// IngestHandler is the per-tick handler that drains the OpenClaw
// memory-flush inbox into scheduled_wakes. State-free — every tick is
// idempotent given the same filesystem state.
type IngestHandler struct {
	dm     *core.DatabaseManager
	logger *slog.Logger
	// ingestPath is the canonical target the plugin writes to.
	// Constructor-injected so tests can use a temp file and not
	// collide with prod. Production passes openclawIngestDefaultPath.
	ingestPath string
}

// NewIngestHandler returns a handler with the production-default
// canonical ingest path. dm is the canonical mpm-core DatabaseManager
// opened in mpm-scheduler main(); logger is the per-daemon slog
// logger. The handler holds no internal state and is safe to call
// sequentially from the scheduler tick.
func NewIngestHandler(dm *core.DatabaseManager, logger *slog.Logger) *IngestHandler {
	return newIngestHandlerWithPath(dm, logger, openclawIngestDefaultPath)
}

// newIngestHandlerWithPath is the test seam: lets the ingest target
// be redirected to a temp file. Production should use NewIngestHandler.
func newIngestHandlerWithPath(dm *core.DatabaseManager, logger *slog.Logger, ingestPath string) *IngestHandler {
	return &IngestHandler{dm: dm, logger: logger, ingestPath: ingestPath}
}

// TickHandler satisfies Scheduler.RegisterTickHandler. Returns the
// inner tickHandler as a closure so the scheduler can invoke it under
// the per-tick budget without exposing internal state.
func (h *IngestHandler) TickHandler() func(ctx context.Context) error {
	return h.tickHandler
}

func (h *IngestHandler) tickHandler(ctx context.Context) error {
	path := h.ingestPath

	// Step 1: lstat with symlink refusal. ErrNotExist is the steady
	// state on most ticks (no file to ingest) — silent no-op.
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		// Transient permission or FS error — log and skip this tick.
		// The next tick will retry; this is non-fatal by design.
		h.logger.Warn("ingest: lstat failed",
			"path", path, "err", err)
		return nil
	}

	// Step 2: refuse symlinks. Symlink to /etc/passwd or any other
	// system file would be a textbook prompt-injection vector through
	// the filesystem-to-context bridge. Reject and log loud.
	if info.Mode()&os.ModeSymlink != 0 {
		h.logger.Warn("ingest: refusing symlink target",
			"path", path)
		return nil
	}

	// Step 3: refuse non-regular files (directories, devices, pipes).
	if !info.Mode().IsRegular() {
		return nil
	}

	// Step 4: perms re-check at tick time. /home/v/.mpm/run/ should
	// be 0700 (user-only) by the AGENTS.md "External vs Internal"
	// directory discipline. A world-writable target would let any
	// local user inject content; reject loud.
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		h.logger.Warn("ingest: world-readable file, refusing",
			"path", path, "perm", perm.String())
		return nil
	}

	// Step 5: size cap. Oversize → quarantine (rename), do not delete,
	// do not truncate silently. The wake row insert needs bounded
	// metadata (the columns are TEXT but wake_context is a glance
	// surface — 64KB JSON-in-metadata would bloat every wake).
	if info.Size() > openclawIngestMaxBytes {
		quarantine := path + openclawIngestRejectedSuffix
		if rerr := os.Rename(path, quarantine); rerr != nil {
			h.logger.Warn("ingest: oversize quarantine rename failed",
				"from", path, "to", quarantine, "err", rerr)
			return nil
		}
		h.logger.Warn("ingest: oversize file quarantined",
			"size", info.Size(), "max", openclawIngestMaxBytes,
			"quarantined_to", quarantine)
		return nil
	}

	// Step 6: atomic read. Rename to .processing, then read. If a
	// concurrent writer races us, they create a separate *.md file
	// (or worse: get a rename error which we log and skip); we never
	// read a partially-written file.
	processing := path + openclawIngestProcessingSuffix
	if rerr := os.Rename(path, processing); rerr != nil {
		h.logger.Warn("ingest: rename to processing failed",
			"from", path, "to", processing, "err", rerr)
		return nil
	}
	content, rerr := os.ReadFile(processing)
	if rerr != nil {
		// Don't delete .processing — leave it for v to inspect
		// (per the AGENTS.md "loud failure beats silent omission").
		h.logger.Warn("ingest: read .processing failed",
			"path", processing, "err", rerr)
		return nil
	}

	// Step 7: ingest. metadata.source is the audit-trail discriminator
	// — wake rows from this path have a different provenance from
	// explicit mpm_call ScheduleWake invocations.
	//
	// targetTime is the absolute epoch seconds at NOW. ScheduleWake's
	// resolveTargetTime rejects "0s" (parseDuration bans zero-duration);
	// passing the resolved absolute epoch keeps the wake firing
	// immediately on the next CheckPendingWakes sweep.
	nowUnix := time.Now().Unix()
	metadata := map[string]interface{}{
		"source":        "openclaw_ingest",
		"original_path": path,
		"byte_count":    len(content),
		"ingested_at":   time.Unix(nowUnix, 0).UTC().Format(time.RFC3339),
		"content":       string(content),
	}
	if _, serr := h.dm.ScheduleWake(
		"ephemeral_compaction_ready",
		fmt.Sprintf("%d", nowUnix), // absolute epoch
		"",                          // theory_id
		"",                          // recurring_rule
		"mpm-memory-openclaw-ingest",
		metadata,
	); serr != nil {
		h.logger.Warn("ingest: ScheduleWake failed",
			"err", serr, "byte_count", len(content))
		// Leave .processing for inspection — wake row was not
		// inserted, file should not disappear to keep evidence.
		return nil
	}

	// Step 8: cleanup. Successful insert + delete the .processing file.
	// If delete fails, log — the next tick will skip the (now-empty)
	// target, and a future plugin write will create a new ingest.md.
	if rerr := os.Remove(processing); rerr != nil {
		h.logger.Warn("ingest: cleanup failed",
			"path", processing, "err", rerr)
		return nil
	}
	h.logger.Info("ingest: wake inserted",
		"byte_count", len(content), "path", path)
	return nil
}
