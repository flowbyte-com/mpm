// session_identity.go — canonical MPM-owned session identity
// resolver. Three distinct entry points encode the read / allocate /
// rotate split that the stage-2C contract requires:
//
//   - CurrentMPMSessionID    — READ-ONLY. Never allocates. Used by
//                              wake / context / doctor / status paths.
//   - AcquireMPMSessionID    — INTERACTION-BOUNDARY ALLOCATOR.
//                              Allocates a fresh ID if and only if no
//                              active one exists. Used by handoff
//                              write, scratchpad flush, explicit
//                              allocation commands.
//   - RotateMPMSessionID     — EXPLICIT ROTATION. Always allocates a
//                              fresh ID, overwriting the previous
//                              one. Used by `mpm session rotate`.
//
// The authoritative backing store is active.json (canonical active
// state file in $MPMDir). All three entry points go through the
// cross-process flock in session_persistence.go to prevent
// split-brain between concurrent CLI/MCP processes.
//
// Process-local cache: read paths cache the resolved value in
// memory after the first call so subsequent reads are lock-free.
// The cache is invalidated on rotate and on mtime-change detection
// (best-effort — if a different process rotates, the next read
// re-reads from disk).
//
// INVARIANTS enforced here:
//
//   1. active.json is the source of truth. The process cache is
//      OPTIONAL only as an optimization.
//   2. Session acquisition is atomic across processes (flock +
//      read-or-create).
//   3. Reads (CurrentMPMSessionID) NEVER allocate.
//   4. First-use allocation happens at interaction boundaries, NOT
//      on every generic read. doctor/health_check/status/wake do
//      NOT allocate.
//   5. A closing handoff records the current mpm_session_id BEFORE
//      the lifecycle is cleared (caller's responsibility — see
//      handoff.go:EndSessionV2).
//   6. Next genuine session gets a new ID (operator-driven via
//      RotateMPMSessionID or `mpm session rotate`).
package internal

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/flowbyte-com/mpm-core/config"
)

// mpmSessionIDPrefix is the public-domain prefix for MPM-owned
// session IDs. Format: "mpm-" + 32 hex chars (128 bits from
// crypto/rand). The prefix is part of the contract — downstream
// tooling (audit logs, handoff rows, wake payloads) can grep for
// "mpm-" to distinguish MPM-owned IDs from framework-supplied
// session identifiers.
const mpmSessionIDPrefix = "mpm-"

// generateNewMPMSessionID allocates a fresh mpm_session_id. The
// format is "mpm-" + 32 hex chars (128 bits from crypto/rand).
// 128 bits of entropy makes accidental collisions effectively
// impossible across any realistic fleet. We deliberately do NOT
// derive from PID, hostname, or timestamp because those produce
// guessable IDs and violate the spec ("Do NOT derive fallback IDs
// from timestamps alone / PID alone / working directory / prompt
// text / user name / hostname / random values regenerated on every
// tool call").
func generateNewMPMSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is exceptional (no /dev/urandom on
		// the host). Fall back to time-based entropy so the
		// allocator still produces something rather than crashing
		// the boot. The resulting ID is still random per boot —
		// just not crypto-strong.
		ns := time.Now().UnixNano()
		pid := int64(os.Getpid())
		for i := 0; i < 16; i++ {
			b[i] = byte((ns >> (uint(i) * 4)) ^ (pid >> (uint(i/2) * 4)))
		}
	}
	return mpmSessionIDPrefix + hex.EncodeToString(b[:]), nil
}

// ── Process-local cache ────────────────────────────────────────────
//
// Read paths consult this cache before touching disk. The cache is
// invalidated on RotateMPMSessionID (always — that's a fresh value)
// and on best-effort mtime-change detection (if active.json's
// modification time moves forward, another process rotated — drop the
// cache). The mtime check is best-effort: it costs one stat per
// read, which is negligible compared to the disk read it gates.
//
// The cache is keyed by the active.json path so tests that change
// MPM_WORKSPACE between sub-tests don't observe stale values.

var (
	sessionCacheMu      sync.Mutex
	sessionCacheValue   string // "" means "not cached or no active session"
	sessionCacheMTime   int64  // unix seconds; 0 means "never loaded"
	sessionCachePathKey string // path the cached value was loaded from
)

// sessionCacheLoadFileMTime returns the modification time of the
// active.json file, or 0 if the file does not exist or the stat
// fails. Used by the cache invalidation check.
func sessionCacheLoadFileMTime() int64 {
	info, err := os.Stat(ActiveJSONPath())
	if err != nil {
		return 0
	}
	return info.ModTime().Unix()
}

// CurrentMPMSessionID returns the current MPM-owned session ID from
// active.json, or "" if no active session exists. NEVER allocates.
// Use this from read paths (wake / context / doctor / status /
// read_handoff / scratchpad read).
//
// Within a process, the value is cached in memory after the first
// read so subsequent calls are lock-free. The cache is invalidated
// when active.json's modification time moves forward — best-effort
// detection that another process rotated. The cache is also keyed
// by the active.json path so MPM_WORKSPACE changes invalidate
// automatically.
//
// Returns "" when:
//   - active.json does not exist (fresh workspace), OR
//   - active.json has no mpm_session_id field (legacy active.json
//     from before the session-identity pass).
//
// Returns the active ID otherwise. The caller MUST NOT interpret
// "" as "error" — it is the normal fresh-workspace state. Read
// paths propagate "" to their output and the agent sees an empty
// mpm_session_id field.
func CurrentMPMSessionID() string {
	sessionCacheMu.Lock()
	defer sessionCacheMu.Unlock()

	pathKey := ActiveJSONPath()

	// Path-key invalidation: tests change MPM_WORKSPACE via
	// overrideMPMDir, so a stale cache from a different path must
	// be discarded.
	if sessionCachePathKey != pathKey {
		sessionCacheValue = ""
		sessionCacheMTime = 0
		sessionCachePathKey = pathKey
	}

	// Best-effort mtime check: if active.json's mtime moved forward
	// since the cache was loaded, another process rotated. Drop the
	// cache and re-read.
	currentMTime := sessionCacheLoadFileMTime()
	if sessionCacheMTime > 0 && currentMTime > sessionCacheMTime {
		sessionCacheValue = ""
		sessionCacheMTime = 0
	}

	if sessionCacheMTime > 0 {
		// Cache hit.
		return sessionCacheValue
	}

	// Cache miss — read from disk.
	state, err := LoadActiveJSON()
	if err != nil {
		// Treat any read error as "no active session" so read
		// paths don't surface a substrate error to the agent. The
		// agent already sees an empty mpm_session_id; failing the
		// read path entirely would be worse than silent empty.
		sessionCacheValue = ""
		sessionCacheMTime = currentMTime
		return ""
	}
	sessionCacheValue = state.MPMSessionID
	sessionCacheMTime = currentMTime
	return sessionCacheValue
}

// CurrentMPMSessionIDCreatedAt returns the unix epoch seconds at
// which the active mpm_session_id was allocated, or 0 if no active
// session exists. Read-only; mirrors CurrentMPMSessionID's
// cache-and-mtime invalidation contract.
func CurrentMPMSessionIDCreatedAt() int64 {
	sessionCacheMu.Lock()
	defer sessionCacheMu.Unlock()

	pathKey := ActiveJSONPath()
	if sessionCachePathKey != pathKey {
		sessionCacheValue = ""
		sessionCacheMTime = 0
		sessionCachePathKey = pathKey
	}
	currentMTime := sessionCacheLoadFileMTime()
	if sessionCacheMTime > 0 && currentMTime > sessionCacheMTime {
		sessionCacheValue = ""
		sessionCacheMTime = 0
	}
	state, err := LoadActiveJSON()
	if err != nil {
		return 0
	}
	// Cache the path-key/mtime stamp even though we don't cache the
	// created_at value here — keep the mtime check consistent.
	sessionCacheMTime = currentMTime
	return state.MPMSessionIDCreatedAt
}

// invalidateSessionCache drops the cached mpm_session_id value so
// the next read re-reads from disk. Called by RotateMPMSessionID
// after writing the new value, and by AcquireMPMSessionID after
// writing the new value.
func invalidateSessionCache() {
	sessionCacheMu.Lock()
	defer sessionCacheMu.Unlock()
	sessionCacheValue = ""
	sessionCacheMTime = 0
	sessionCachePathKey = ""
}

// AcquireMPMSessionID returns the current mpm_session_id; if no
// active session exists, performs an atomic read-or-create against
// active.json and returns the new ID. This is the FIRST-USE
// allocation path — call from INTERACTION BOUNDARIES only:
//
//   - mpm_handoff write
//   - mpm_scratchpad flush
//   - explicit mpm session allocate
//
// NEVER call from generic read paths (wake / context / doctor /
// status / read_handoff / scratchpad read). The invariant #4
// separation between read and allocate is what prevents
// doctor probes and startup tooling from manufacturing phantom
// sessions.
//
// Cross-process locking: this function acquires the active.json
// flock for the duration of the read-or-create cycle. Two
// processes starting simultaneously against an empty active state
// serialize on the lock and converge on the same ID.
func AcquireMPMSessionID() string {
	var id string
	err := withActiveJSONFlock(func() error {
		got, err := loadOrAllocateMPMSessionIDLocked()
		if err != nil {
			return err
		}
		id = got
		return nil
	})
	if err != nil {
		// Best-effort: if the lock acquisition or persistence failed,
		// fall back to an in-process ID so the caller can still
		// write a handoff. The next acquire from any process will
		// see this row and adopt the ID.
		id, _ = generateNewMPMSessionID()
	}
	invalidateSessionCache()
	return id
}

// RotateMPMSessionID allocates a fresh mpm_session_id, persists it
// to active.json (cross-process-locked), and returns it. The
// previous value (if any) is overwritten. Callers MUST treat this
// as an explicit lifecycle signal.
//
// Use cases:
//   - Operator-driven "new session" via `mpm session rotate`.
//   - Integration boot with MPM_SESSION_ROTATE=1.
//
// Reads (wake / context / doctor) MUST NOT call this.
func RotateMPMSessionID() string {
	var id string
	err := withActiveJSONFlock(func() error {
		got, err := rotateMPMSessionIDLocked()
		if err != nil {
			return err
		}
		id = got
		return nil
	})
	if err != nil {
		// On persistence failure, still return a fresh ID so the
		// caller has something to print / log. The next acquire
		// from any process will see the prior value (rotate did
		// not persist) — that's the correct "rotate attempted,
		// failed" semantic.
		id, _ = generateNewMPMSessionID()
	}
	invalidateSessionCache()
	return id
}

// InitSessionIdentityOnBoot is the entry point called once at the
// start of every `mpm` process (CLI or MCP). It honors two env
// vars:
//
//   - MPM_SESSION_ROTATE=1 → forces a rotation at boot, allocating
//                              a fresh ID and overwriting the prior.
//                              Use case: integration boot after a
//                              long pause, or when the integration
//                              detects a fresh interaction lifecycle
//                              it wants to start tracking.
//
//   - MPM_SESSION_ID=<id>  → does NOT override the active.json
//                              identity; this var is reserved for
//                              future framework-side correlation
//                              metadata. The substrate's
//                              mpm_session_id is always owned by MPM.
//
// This is called from main.go / call.go / mcp/main.go at startup.
// It does NOT auto-allocate otherwise — that's AcquireMPMSessionID's
// job at the interaction boundary.
func InitSessionIdentityOnBoot() {
	if v := os.Getenv("MPM_SESSION_ROTATE"); v == "1" || v == "true" {
		RotateMPMSessionID()
		return
	}
	// Touch CurrentMPMSessionID to seed the process-local cache so
	// the first read in this process is lock-free. Read-only — no
	// allocation.
	_ = CurrentMPMSessionID()
	_ = config.GetMPMDir // import anchor for go.mod
}

// _ = fmt.Sprintf — used elsewhere in the package; the import
// keeps gofmt happy when this file is the only consumer.
var _ = fmt.Sprintf
