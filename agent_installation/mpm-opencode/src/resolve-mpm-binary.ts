/**
 * resolve-mpm-binary.ts — Canonical MPM binary discovery.
 *
 * OpenCode runs the plugin under a process whose PATH may not include
 * the operator's interactive shell PATH — particularly when OpenCode
 * is launched by a service manager (launchd, systemd --user) without
 * sourcing ~/.profile or ~/.bashrc. We have observed MPM correctly
 * installed at ~/.mpm/bin/mpm while `mpm` is not on PATH, breaking
 * boot. The previous behaviour (`bin = MPM_BINARY ?? "mpm"`) treated
 * PATH as the sole source of truth and produced a misleading
 * "Executable not found in $PATH: mpm" boot warning even though MPM
 * is installed and healthy.
 *
 * Canonical discovery order (each candidate must both exist and be
 * executable; we normalize to an absolute path where possible):
 *
 *   1. process.env.MPM_BINARY
 *        Explicit per-process override (operator-controlled).
 *
 *   2. $HOME/.mpm/bin/mpm
 *        The canonical install root written by install.sh.
 *
 *   3. $HOME/.local/bin/mpm
 *        The canonical user-symlink created by install.sh.
 *        If a symlink, resolved through readlink so the spawn uses
 *        the real target (deterministic behaviour).
 *
 *   4. command -v mpm
 *        Last-resort PATH lookup. May legitimately miss if OpenCode
 *        was launched without sourcing the user's login shell.
 *
 * The resolved path is cached per plugin process. Every MPM subprocess
 * — boot health check, tool calls, wake-context fetch — uses the same
 * resolved binary, so the operator sees consistent spawn behaviour.
 */

import { execFileSync } from "node:child_process";
import * as fs from "node:fs";
import * as path from "node:path";

export interface DiscoveryAttempt {
	candidate: string;
	reason: "explicit_override" | "canonical_primary" | "canonical_symlink" | "path_lookup" | "fallback_literal";
	exists: boolean;
	executable: boolean;
	resolved: string | null;
}

/**
 * Resolve the canonical MPM binary path. Returns the first candidate
 * that exists and is executable, normalized to an absolute path.
 * Returns the literal "mpm" as a last-resort fallback (callers will
 * then get a clear spawn failure + the boot warning will name the
 * exact discovery paths attempted).
 *
 * `attemptLog` (if provided) is populated with a per-candidate
 * audit trail — useful for diagnostics and for the boot-warning
 * message.
 */
export function resolveMpmBinary(
	env: NodeJS.ProcessEnv = process.env,
	execFile: typeof execFileSync = execFileSync,
	attemptLog?: DiscoveryAttempt[],
): string {
	const log = (a: DiscoveryAttempt) => attemptLog?.push(a);

	// 1. Explicit per-process override.
	const envOverride = env.MPM_BINARY;
	if (envOverride && envOverride.length > 0) {
		const resolved = resolveExistingAbsolute(envOverride);
		log?.({
			candidate: envOverride,
			reason: "explicit_override",
			exists: resolved !== null,
			executable: resolved !== null,
			resolved,
		});
		if (resolved) return resolved;
		// Explicit override that doesn't exist or isn't executable
		// is a hard error — the operator meant something specific.
		// Don't fall through; return the literal so the spawn fails
		// loudly with the operator's chosen value.
		return envOverride;
	}

	const home = env.HOME;
	if (home && home.length > 0) {
		// 2. Canonical install root.
		const primary = path.join(home, ".mpm", "bin", "mpm");
		const resolvedPrimary = resolveExistingAbsolute(primary);
		log?.({
			candidate: primary,
			reason: "canonical_primary",
			exists: resolvedPrimary !== null,
			executable: resolvedPrimary !== null,
			resolved: resolvedPrimary,
		});
		if (resolvedPrimary) return resolvedPrimary;

		// 3. Canonical user-symlink.
		const symlink = path.join(home, ".local", "bin", "mpm");
		const resolvedSymlink = resolveExistingAbsolute(symlink);
		log?.({
			candidate: symlink,
			reason: "canonical_symlink",
			exists: resolvedSymlink !== null,
			executable: resolvedSymlink !== null,
			resolved: resolvedSymlink,
		});
		if (resolvedSymlink) return resolvedSymlink;
	}

	// 4. Last-resort PATH lookup.
	try {
		const which = execFile("command", ["-v", "mpm"], {
			stdio: ["ignore", "pipe", "ignore"],
			encoding: "utf8",
		}).trim();
		if (which.length > 0) {
			log?.({
				candidate: which,
				reason: "path_lookup",
				exists: true,
				executable: true,
				resolved: which,
			});
			return which;
		}
	} catch {
		// command -v failed — mpm genuinely not on PATH.
		log?.({
			candidate: "PATH (command -v mpm)",
			reason: "path_lookup",
			exists: false,
			executable: false,
			resolved: null,
		});
	}

	// Final fallback: literal "mpm". Caller will get a clear spawn
	// failure if it really is missing; the boot warning will name
	// the exact discovery paths we attempted.
	log?.({
		candidate: "mpm",
		reason: "fallback_literal",
		exists: false,
		executable: false,
		resolved: null,
	});
	return "mpm";
}

function resolveExistingAbsolute(candidate: string): string | null {
	try {
		const abs = path.isAbsolute(candidate)
			? candidate
			: path.resolve(candidate);
		const stat = fs.statSync(abs);
		if (!stat.isFile()) return null;
		// Must be executable. fs.access X_OK throws if not.
		fs.accessSync(abs, fs.constants.X_OK);
		// If it's a symlink, follow it once so spawn uses the real
		// target. realpath is best-effort; if it fails we keep the
		// symlink path (deterministic enough).
		try {
			const real = fs.realpathSync(abs);
			return real;
		} catch {
			return abs;
		}
	} catch {
		return null;
	}
}
