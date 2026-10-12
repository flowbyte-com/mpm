// src/resolve-mpm-binary.ts — canonical MPM binary discovery for the
// mpm-pi extension.
//
// This mirrors mpm-opencode/src/resolve-mpm-binary.ts, which was written
// after an OpenCode install was observed to break at boot: MPM was
// correctly installed at ~/.mpm/bin/mpm while `mpm` was not on PATH,
// because OpenCode had been launched by a service manager (launchd,
// systemd --user) that never sourced ~/.profile or ~/.bashrc. The
// adapter spawned the literal "mpm", the spawn failed, and the boot
// warning blamed PATH — sending the operator to look at a PATH that was
// never the problem.
//
// The Pi adapter had the same latent defect in a stronger form: every
// call site resolved `opts.mpmBin ?? "mpm"`, so there was no fallback
// whatsoever and no place to record why resolution failed. Pi is
// frequently launched from a desktop entry or a wrapper script, so the
// same missing-PATH condition is at least as likely there.
//
// Canonical discovery order (each candidate must exist, be a regular
// file, and be executable; symlinks are resolved so the spawn uses a
// stable real path):
//
//   1. process.env.MPM_BINARY
//        Explicit per-process override. An override that does not
//        resolve is returned verbatim rather than replaced by a
//        fallback: the operator asked for something specific, and
//        silently using a different binary would be worse than failing.
//
//   2. $MPM_WORKSPACE/bin/mpm
//        The canonical install root. This honours a relocated workspace
//        rather than assuming $HOME/.mpm, matching resolveWorkspace()
//        in ./workspace.ts — resolution of the binary and of the
//        workspace it operates on must not disagree.
//
//   3. $HOME/.mpm/bin/mpm
//        The default install root when MPM_WORKSPACE is unset.
//
//   4. $HOME/.local/bin/mpm
//        The user symlink created by install.sh.
//
//   5. command -v mpm
//        Last-resort PATH lookup, preserving the historical behaviour for
//        operators who install mpm by some other means.
//
// If none resolve, the literal "mpm" is returned so the spawn fails with
// the operator's normal PATH semantics, and the attempt log explains
// exactly which locations were checked.

import { execFileSync } from "node:child_process";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";

export interface DiscoveryAttempt {
	candidate: string;
	reason:
		| "explicit_override"
		| "workspace_root"
		| "canonical_primary"
		| "canonical_symlink"
		| "path_lookup"
		| "fallback_literal";
	exists: boolean;
	executable: boolean;
	resolved: string | null;
}

export interface ResolvedMpmBinary {
	/** Absolute path when discovered, or the literal "mpm" as a fallback. */
	binary: string;
	/** True when no candidate resolved and the literal fallback was used. */
	unresolved: boolean;
	/** Per-candidate audit trail, for diagnostics. */
	attempts: DiscoveryAttempt[];
}

function resolveExistingAbsolute(candidate: string): string | null {
	try {
		const abs = path.isAbsolute(candidate) ? candidate : path.resolve(candidate);
		const stat = fs.statSync(abs);
		if (!stat.isFile()) return null;
		fs.accessSync(abs, fs.constants.X_OK);
		// Follow a symlink once so repeated spawns target the same real
		// path. realpathSync is best-effort: on failure keep the
		// symlink path rather than discarding a working candidate.
		try {
			return fs.realpathSync(abs);
		} catch {
			return abs;
		}
	} catch {
		return null;
	}
}

/**
 * Resolve the canonical MPM binary path.
 *
 * Returns the resolved path plus an audit trail. `attempts` is populated
 * unconditionally (not optional) so the boot-time warning can name every
 * location checked instead of blaming PATH generically.
 */
export function resolveMpmBinary(
	env: NodeJS.ProcessEnv = process.env,
	execFile: typeof execFileSync = execFileSync,
): ResolvedMpmBinary {
	const attempts: DiscoveryAttempt[] = [];

	const record = (
		candidate: string,
		reason: DiscoveryAttempt["reason"],
		resolved: string | null,
	): string | null => {
		attempts.push({
			candidate,
			reason,
			exists: resolved !== null,
			executable: resolved !== null,
			resolved,
		});
		return resolved;
	};

	// 1. Explicit per-process override. Fails closed: an unresolvable
	// override is the operator's stated intent, so it is returned as-is
	// and the spawn fails loudly instead of silently using another binary.
	const override = env.MPM_BINARY;
	if (override && override.length > 0) {
		const resolved = record(override, "explicit_override", resolveExistingAbsolute(override));
		return { binary: resolved ?? override, unresolved: resolved === null, attempts };
	}

	const home = env.HOME && env.HOME.length > 0 ? env.HOME : os.homedir();

	// 2. Relocated workspace, when the operator set one. Checked before
	// $HOME/.mpm so an explicit workspace always wins over the default.
	const workspace = env.MPM_WORKSPACE;
	if (workspace && workspace.trim() !== "") {
		const candidate = path.join(workspace, "bin", "mpm");
		const resolved = record(candidate, "workspace_root", resolveExistingAbsolute(candidate));
		if (resolved) return { binary: resolved, unresolved: false, attempts };
	}

	// 3. Default install root.
	const primary = path.join(home, ".mpm", "bin", "mpm");
	const resolvedPrimary = record(primary, "canonical_primary", resolveExistingAbsolute(primary));
	if (resolvedPrimary) return { binary: resolvedPrimary, unresolved: false, attempts };

	// 4. User symlink.
	const symlink = path.join(home, ".local", "bin", "mpm");
	const resolvedSymlink = record(symlink, "canonical_symlink", resolveExistingAbsolute(symlink));
	if (resolvedSymlink) return { binary: resolvedSymlink, unresolved: false, attempts };

	// 5. PATH lookup — the historical behaviour, kept as a last resort.
	try {
		const which = execFile("command", ["-v", "mpm"], {
			stdio: ["ignore", "pipe", "ignore"],
			encoding: "utf8",
		}).trim();
		if (which.length > 0) {
			record(which, "path_lookup", which);
			return { binary: which, unresolved: false, attempts };
		}
	} catch {
		record("PATH (command -v mpm)", "path_lookup", null);
	}

	record("mpm", "fallback_literal", null);
	return { binary: "mpm", unresolved: true, attempts };
}

/** Render the discovery trail for a diagnostic message. */
export function describeDiscovery(result: ResolvedMpmBinary): string {
	return result.attempts
		.map((a) => `  ${a.resolved ? "found   " : "missing "} ${a.candidate}`)
		.join("\n");
}