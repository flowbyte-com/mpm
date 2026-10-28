// internal/telemetry/paths.go — the canonical telemetry store path.
//
// WHY THIS FILE EXISTS. The telemetry store's location was decided
// TWICE, in two different places, and the two answers disagreed:
//
//   - docs/SPEC.md §6.6 says "$MPM_WORKSPACE/telemetry.db".
//   - The collector (cmd/mpm-telemetry/serve.go) has written to
//     "$MPM_WORKSPACE/src/db/telemetry.db" since commit 2a082bc
//     ("feat(work): provenance env vars, wake-context format, and
//     Claude Code integration"), which moved the path as an incidental
//     side effect of an unrelated feature and never touched the spec.
//
// The collector's path is the authoritative one: it is where the frames
// actually land, and it is where `mpm-telemetry query` has read them
// from since the same commit. The spec was simply never updated.
//
// The cost of the disagreement was paid by the critic. The telemetry
// hunt merged in #138 ("the telemetry hunt is a read-only diagnostic")
// adopted the SPEC's path — a path nothing writes to. `telemetry.Open`
// applies DDL, so it CREATED an empty telemetry.db there on every cycle
// and the hunt never fired. The correction in #138 was therefore inert,
// and its test was self-consistent but unrepresentative: it seeded the
// same wrong path the code used.
//
// ONE resolver, used by the collector, by every reader, and by the
// critic. A second path definition is how the two drifted apart.
package telemetry

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// StorePath returns the canonical telemetry store path for a workspace.
//
// The canonical location is <workspace>/src/db/telemetry.db — the same
// "src/db/" layout the substrate uses for mpm.db, so the two stores sit
// side by side under the runtime root.
func StorePath(workspace string) string {
	return filepath.Join(workspace, "src", "db", "telemetry.db")
}

// EnvStorePathOverride returns the MPM_TELEMETRY_DB override, or "" when
// it is unset or blank.
//
// MPM_TELEMETRY_DB is the operator's escape hatch: it points at the
// collector's actual store when the collector runs with a non-default
// path. Both the collector and every reader honour it, so a store
// written by one is found by the others.
func EnvStorePathOverride() string {
	return strings.TrimSpace(os.Getenv("MPM_TELEMETRY_DB"))
}

// ResolveStorePath resolves the telemetry store path for a workspace.
//
// Precedence:
//
//  1. MPM_TELEMETRY_DB — an explicit operator override.
//  2. <workspace>/src/db/telemetry.db — the canonical location.
//
// The workspace may be empty ONLY when the override is set; otherwise
// the result would be a path relative to the current working directory,
// which is the "ghost store" shape this codebase removed after the
// 2026-07-21 incident (lesson 59fe3f8ff3e1549e): a relative path
// resolves differently depending on where the process was started, so
// the writer and the reader silently disagree.
func ResolveStorePath(workspace string) (string, error) {
	if override := EnvStorePathOverride(); override != "" {
		return filepath.Clean(override), nil
	}
	if strings.TrimSpace(workspace) == "" {
		return "", errors.New(
			"cannot resolve the telemetry store path: no workspace was given — " +
				"set MPM_WORKSPACE, or set MPM_TELEMETRY_DB to the store's path")
	}
	return StorePath(workspace), nil
}

// DescribeStorePath renders a store path for an operator-facing message,
// naming the override that produced it when one is in effect.
func DescribeStorePath(path string) string {
	if EnvStorePathOverride() != "" {
		return fmt.Sprintf("%s (via MPM_TELEMETRY_DB)", path)
	}
	return path
}
