// Package update implements the MPM update manager.
//
// An MPM installation has two halves with two different lifetimes: the
// RUNTIME (binaries, host adapters, plugin payloads — replaced wholesale
// on every upgrade) and the STATE (the database, backups, configuration,
// definitions — which must survive every upgrade unchanged). The versioned
// layout that enforces the split already exists:
//
//	<data root>/
//	  releases/<rev>/   a versioned, Git-free runtime bundle
//	  current -> releases/<rev>   THE ACTIVE POINTER (one atomic symlink)
//	  staging/          where a candidate bundle is assembled
//	  mpm-update        the stable updater copy (a real file, NOT a symlink
//	                    into any release — see paths.go)
//	  src/db/ backups/ mpm_config.json mode/ persona/   STATE
//
// This package orchestrates the move from one release to another. It
// REUSES the existing promotion primitives rather than redefining them:
//
//   - bundle validation mirrors scripts/lib/runtime_bundle.sh
//     rb_validate_bundle (same five checks, same forbidden list — see
//     verify.go for the reuse contract and the parity test);
//   - the pointer swap is the same atomic rename(2) the shell library
//     performs (os.Rename over a same-directory temp symlink);
//   - backups use the SQLite online backup path (VACUUM INTO), never a
//     raw file copy, for the WAL reason the rehearsal documented;
//   - schema preflight compares live table shapes against the
//     substrate's own DDL, read-only, against a snapshot.
//
// What this package does NOT do:
//
//   - it never touches install.sh's flows and never modifies the shared
//     shell libraries (coordination boundary with Session A);
//   - it never performs a database rollback silently: runtime rollback
//     and database restore are separate operations, and a rollback that
//     would strand user data on an incompatible schema is refused;
//   - it never terminates agent sessions and never restarts a managed
//     service unless the operator explicitly authorised it.
//
// The standalone recovery story: cmd/mpm-update links only this package
// (plus internal/buildinfo). It never imports the substrate packages, so
// recovering MPM never requires starting the broken MPM runtime.
package update
