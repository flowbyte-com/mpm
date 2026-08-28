package capability

// =============================================================================
// limits.go — ResourceLimits safety clamps (spec §4.3 / EX-4)
//
// The capability lifecycle allows the operator (and the
// metadata stored at proposal time) to override every
// ResourceLimits field. Without a clamp, a typo or malicious
// metadata blob could request a 1-petabyte memory cap, a 24-
// hour runtime, or 10⁹ open FDs — every one of which would
// either crash the host, hang the executor, or be silently
// truncated by the kernel in surprising ways.
//
// ClampLimits is the single insertion point. resolveLimits in
// executor.go calls it after the three-tier precedence
// (request > metadata > defaults), so every Driver receives
// pre-clamped limits without having to do its own defensive
// math.
//
// Ceilings are documented in §4.3 of the spec:
//
//   MaxRuntimeMs   = 5 minutes (300_000 ms) — long enough
//                    for heavy data processing or test
//                    suites, short enough to prevent
//                    infinite hangs
//   MaxMemoryMB    = 4 GB — safe boundary for a standard
//                    developer workstation; prevents a
//                    runaway process from triggering the
//                    host OOM killer
//   MaxFDs         = 65_536 — standard POSIX hard-limit
//                    ceiling
//   MaxOutputBytes = 64 MiB — generous text buffer; stops
//                    an agent from using stdout as a
//                    massive IPC stream
//
// These ceilings are deliberately conservative. A capability
// that needs more than 4 GB of RAM or 5 minutes of wall-clock
// is almost certainly misconfigured; the operator can raise
// the ceilings here (one-line change) if a legitimate use
// case emerges.
//
// Spec: docs/archive/capability-lifecycle.md §4.3
// =============================================================================

// LimitsCeiling is the upper bound applied to each
// ResourceLimits field. Kept as a package-level const rather
// than a function so test code can reference the same value
// without recomputing it.
//
// The values mirror the spec §4.3 ceiling table:
//
//   MaxRuntimeMs   = 300_000   (5 minutes)
//   MaxMemoryMB    = 4_096     (4 GiB)
//   MaxFDs         = 65_536    (POSIX hard limit ceiling)
//   MaxOutputBytes = 64 MiB    (64 * 1024 * 1024 bytes)
func LimitsCeiling() ResourceLimits {
	return ResourceLimits{
		MaxRuntimeMs:   300_000,
		MaxMemoryMB:    4_096,
		MaxFDs:         65_536,
		MaxOutputBytes: 64 * 1024 * 1024,
	}
}

// ClampLimits reduces each field of `in` to at most the
// documented ceiling. Fields already under the ceiling are
// untouched (this is not a rebase — it is a safety ceiling).
//
// Zero values pass through unchanged. The clamp assumes the
// caller has already resolved defaults (via resolveLimits); a
// zero value here means "default wasn't applied," which is the
// Executor's bug, not a clamping concern.
//
// Negative values are also passed through unchanged — they
// indicate a malformed payload and are caught by the Driver's
// own input validation, not by ClampLimits. ClampLimits only
// defends against absurd-but-parseable values, not against
// outright garbage.
//
// ClampLimits is idempotent: clamping twice yields the same
// result as clamping once (because the ceiling itself is at
// or below the ceiling for every field).
func ClampLimits(in ResourceLimits) ResourceLimits {
	ceil := LimitsCeiling()
	out := in
	if out.MaxRuntimeMs > ceil.MaxRuntimeMs {
		out.MaxRuntimeMs = ceil.MaxRuntimeMs
	}
	if out.MaxMemoryMB > ceil.MaxMemoryMB {
		out.MaxMemoryMB = ceil.MaxMemoryMB
	}
	if out.MaxFDs > ceil.MaxFDs {
		out.MaxFDs = ceil.MaxFDs
	}
	if out.MaxOutputBytes > ceil.MaxOutputBytes {
		out.MaxOutputBytes = ceil.MaxOutputBytes
	}
	return out
}

// MemoryBytes converts MaxMemoryMB to the byte count expected
// by bwrap's `--rlimit-as` flag. Returns 0 for non-positive
// inputs (which the Driver treats as "do not set the limit").
//
// Kept here (rather than inline in driver_bwrap.go) because
// EX-6's DirectDriver will reuse the same conversion — prlimit
// and cgroup v2 memory.max both take bytes, not megabytes.
//
// We multiply as int64 explicitly to avoid silent truncation
// when MaxMemoryMB is near 2^31; 4 GB ceiling × 1 MiB easily
// fits in int64.
func MemoryBytes(maxMemoryMB int64) int64 {
	if maxMemoryMB <= 0 {
		return 0
	}
	return maxMemoryMB * 1024 * 1024
}
