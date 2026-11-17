/**
 * sandbox-exec.ts — the HOST-CONTROLLED sandboxed execution tool.
 *
 * WHY THIS EXISTS
 * ---------------
 * The reviewer's isolation used to rest on the PERMISSION layer: the
 * production MPM tools were denied by name and the MPM binaries were
 * refused wherever they were named.  A permission rule is a pattern
 * match on a command string, so a route the rules do not name is still
 * a route — and the shell-matcher research that tried to close those
 * routes is closed and is not the boundary.
 *
 * The boundary is an OPERATING-SYSTEM one: `reviewer_bwrap.py` builds a
 * bubblewrap MOUNT NAMESPACE, in which a path that is not bound does
 * not exist.  The kernel refuses the access; there is no pattern to
 * get wrong.
 *
 * WHY IT IS HOST-CONTROLLED
 * -------------------------
 * The alternative — rewriting the command in the shell hook and
 * identifying the reviewer's command by a session identifier — was
 * measured and REJECTED: the hook's event carries no session
 * identifier, and the environment's `OPENCODE_SESSION_ID` is
 * agent-influenceable.  A security authority the agent can influence
 * is not an authority.
 *
 * So there is no identification to get wrong: this tool is ALWAYS
 * sandboxed, and the reviewer has no other way to execute.  Everything
 * the model controls is the COMMAND, and the command is data to the
 * sandbox, never configuration for it.
 *
 *   - the sandbox base is resolved by the HOST, from the operator's
 *     configuration or a per-user location — never from the model;
 *   - the repository that is copied in is the HOST's project directory
 *     — never from the model;
 *   - the command arrives as a JSON ARRAY on stdin, so it never
 *     reaches a command-line parser, and the namespace's own `--`
 *     separator stops it reaching bubblewrap's parser either;
 *   - the working directory is one of the sandbox's own trees;
 *   - if the sandbox cannot be established, the command does NOT run.
 *     There is no unsandboxed fallback, and no degraded mode.
 */

import { execFileSync, spawn } from "node:child_process";
import { createHash } from "node:crypto";
import { existsSync, mkdirSync, statSync } from "node:fs";
import { homedir, tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

/** The canonical path to the sandbox script, relative to this module. */
export const SANDBOX_SCRIPT = fileURLToPath(
  new URL("../scripts/reviewer_bwrap.py", import.meta.url),
);

/** The tool's name.  The reviewer's agent definition allows exactly this. */
export const SANDBOX_EXEC_TOOL = "sandbox_exec";

/** The trees a command may run in.  Anything else is refused. */
export const SANDBOX_TREES = ["repo", "home", "tmp", "workspace", "findings"];

/** The command exit code that means "the sandbox refused". */
const SANDBOX_FAILURE = 125;

export interface SandboxEnvelope {
  ok: boolean;
  stage?: string;
  error?: string;
  exit_code?: number | null;
  stdout?: string;
  stderr?: string;
  base?: string;
  sandbox?: string;
}

export interface SandboxConfig {
  /** The sandbox base directory — HOST-resolved. */
  base: string;
  /** The repository to copy in — HOST-resolved. */
  source?: string;
  /** Directories the base must never be inside — HOST-resolved. */
  protect: string[];
  /** The python interpreter. */
  python?: string;
  /** The sandbox script. */
  script?: string;
}

export interface SandboxRunOptions {
  cwd?: string;
  timeoutMs?: number;
}

/**
 * The sandbox base, resolved by the HOST.
 *
 * The operator may relocate it.  Otherwise it lives under the
 * operator's own cache directory, which is not world-writable — a
 * shared temporary directory would let another user pre-create the
 * base.  The script refuses a base it does not own, so the fallback
 * below is safe too.
 */
export function resolveSandboxBase(): string {
  const configured = process.env.MPM_REVIEW_SANDBOX_BASE;
  if (configured && configured.trim().length > 0) {
    return resolve(configured.trim());
  }
  const cache = join(homedir(), ".cache", "mpm-opencode");
  try {
    mkdirSync(cache, { recursive: true });
    return join(cache, "review-sandbox");
  } catch {
    return join(tmpdir(), `mpm-review-sandbox-${process.getuid?.() ?? 0}`);
  }
}

/**
 * The stamp of the source tree.
 *
 * The disposable copy must be the tree the operator is looking at, so
 * the copy is refreshed when the source's HEAD or its working-tree
 * state changes.  The stamp is HOST-side state: it never comes from
 * the model.
 */
export function sourceStamp(source: string): string {
  const hash = createHash("sha256");
  hash.update(source);
  for (const args of [
    ["rev-parse", "HEAD"],
    ["status", "--porcelain=v1", "--untracked-files=normal"],
  ]) {
    try {
      const result = spawnSyncText("git", ["-C", source, ...args]);
      hash.update(result);
    } catch {
      hash.update("unavailable");
    }
  }
  try {
    hash.update(String(statSync(source).mtimeMs));
  } catch {
    hash.update("no-stat");
  }
  return hash.digest("hex");
}

function spawnSyncText(command: string, args: string[]): string {
  // A tiny synchronous helper: the stamp must be available before the
  // command runs, and the sandbox itself is asynchronous.
  try {
    return execFileSync(command, args, {
      encoding: "utf-8",
      timeout: 30_000,
      stdio: ["ignore", "pipe", "ignore"],
    });
  } catch {
    return "";
  }
}

async function runScript(
  config: SandboxConfig,
  args: string[],
  stdinPayload?: string,
): Promise<{ code: number; stdout: string; stderr: string; envelope?: SandboxEnvelope }> {
  const script = config.script ?? SANDBOX_SCRIPT;
  const python = config.python ?? "python3";
  const full = [
    script,
    "--base",
    config.base,
    ...config.protect.flatMap((p) => ["--protect", p]),
    ...args,
  ];
  return new Promise((resolvePromise) => {
    const child = spawn(python, full, {
      stdio: ["pipe", "pipe", "pipe", "pipe"],
    });
    let stdout = "";
    let stderr = "";
    let envelopeText = "";
    child.stdout.on("data", (chunk) => {
      stdout += String(chunk);
    });
    child.stderr.on("data", (chunk) => {
      stderr += String(chunk);
    });
    const envelopeStream = child.stdio[3];
    if (envelopeStream) {
      envelopeStream.on("data", (chunk) => {
        envelopeText += String(chunk);
      });
    }
    child.on("error", (err) => {
      resolvePromise({
        code: SANDBOX_FAILURE,
        stdout,
        stderr: `${stderr}${stderr ? "\n" : ""}the sandbox could not be started: ${err.message}`,
      });
    });
    child.on("close", (code) => {
      let envelope: SandboxEnvelope | undefined;
      const line = envelopeText.split("\n").filter((l) => l.trim().length > 0).pop();
      if (line) {
        try {
          envelope = JSON.parse(line) as SandboxEnvelope;
        } catch {
          envelope = undefined;
        }
      }
      resolvePromise({
        code: code ?? SANDBOX_FAILURE,
        stdout,
        stderr,
        envelope,
      });
    });
    if (stdinPayload !== undefined) {
      child.stdin.write(stdinPayload);
    }
    child.stdin.end();
  });
}

/**
 * Build the disposable tree, if it is not already current.
 *
 * The repository copy is the expensive part, so it is refreshed only
 * when the source's stamp changes.
 */
export class SandboxManager {
  private readonly config: SandboxConfig;
  private stamp: string | undefined;

  constructor(config: SandboxConfig) {
    this.config = config;
  }

  get base(): string {
    return this.config.base;
  }

  async ensureTree(): Promise<{ ok: boolean; error?: string }> {
    const script = this.config.script ?? SANDBOX_SCRIPT;
    if (!existsSync(script)) {
      return {
        ok: false,
        error:
          `the sandbox script is missing at ${script}; the sandbox cannot be ` +
          "established and no command was run",
      };
    }
    if (!this.config.source) {
      return { ok: true };
    }
    const current = sourceStamp(this.config.source);
    if (this.stamp === current && existsSync(join(this.config.base, "source", "repo"))) {
      return { ok: true };
    }
    const args = ["init"];
    if (this.config.source) args.push("--source", this.config.source);
    const result = await runScript(this.config, args);
    if (result.code !== 0) {
      return {
        ok: false,
        error:
          `the sandbox tree could not be built (exit ${result.code}): ` +
          `${result.stderr.trim() || "no diagnostic"}`,
      };
    }
    this.stamp = current;
    return { ok: true };
  }

  /**
   * Run a command inside the sandbox.
   *
   * There is NO fallback.  If the sandbox cannot be established the
   * command does not run: the result is a refusal, and the refusal is
   * what the model sees.
   */
  async run(argv: string[], options: SandboxRunOptions = {}): Promise<SandboxEnvelope> {
    if (!Array.isArray(argv) || argv.length === 0) {
      return {
        ok: false,
        stage: "refused",
        error: "the command must be a non-empty array of arguments",
      };
    }
    for (const element of argv) {
      if (typeof element !== "string" || element.includes("\0")) {
        return {
          ok: false,
          stage: "refused",
          error: "the command must be an array of strings without NUL",
        };
      }
    }
    if (options.cwd !== undefined && !SANDBOX_TREES.includes(options.cwd)) {
      return {
        ok: false,
        stage: "refused",
        error: `the working directory must be one of ${SANDBOX_TREES.join(", ")}`,
      };
    }

    const tree = await this.ensureTree();
    if (!tree.ok) {
      return { ok: false, stage: "refused", error: tree.error };
    }

    const payload = JSON.stringify({
      argv,
      cwd: options.cwd ?? "repo",
      timeout_ms: options.timeoutMs ?? 600_000,
    });
    const result = await runScript(this.config, ["exec"], payload);
    if (!result.envelope) {
      return {
        ok: false,
        stage: "refused",
        error:
          `the sandbox did not report a result (exit ${result.code}); the ` +
          `command was NOT run: ${result.stderr.trim() || "no diagnostic"}`,
      };
    }
    return result.envelope;
  }
}

/** Render an envelope as the text the model sees. */
export function renderEnvelope(envelope: SandboxEnvelope): string {
  if (!envelope.ok) {
    return [
      "The sandbox REFUSED the command; it did not run.",
      `reason: ${envelope.error ?? "unknown"}`,
      "",
      "There is no unsandboxed fallback. Report the refusal.",
    ].join("\n");
  }
  const lines: string[] = [];
  lines.push(`exit code: ${envelope.exit_code ?? 0}`);
  const stdout = envelope.stdout ?? "";
  const stderr = envelope.stderr ?? "";
  if (stdout.length > 0) {
    lines.push("--- stdout ---", stdout.replace(/\n$/, ""));
  }
  if (stderr.length > 0) {
    lines.push("--- stderr ---", stderr.replace(/\n$/, ""));
  }
  if (stdout.length === 0 && stderr.length === 0) {
    lines.push("(no output)");
  }
  return lines.join("\n");
}

/** The tool's input schema. */
export const SANDBOX_EXEC_INPUT = {
  type: "object",
  properties: {
    command: {
      type: "array",
      items: { type: "string" },
      description:
        "The command to run, as an argument ARRAY. It is executed directly, " +
        "without a shell. To use shell syntax, run `sh` with `-c` as two " +
        "elements of this array.",
    },
    cwd: {
      type: "string",
      enum: SANDBOX_TREES,
      description:
        "The directory the command starts in, inside the sandbox. " +
        "`repo` is the disposable copy of the repository under review.",
    },
    timeout_ms: {
      type: "integer",
      description: "How long the command may run, in milliseconds.",
    },
  },
  required: ["command"],
} as const;

export const SANDBOX_EXEC_DESCRIPTION = [
  "Run a command inside the reviewer's OS-CONFINED sandbox.",
  "",
  "The sandbox is a bubblewrap mount namespace: only the runtime, a",
  "disposable copy of the repository under review and the reviewer's own",
  "scratch directories exist inside it. The operator's files, the live MPM",
  "substrate, the credentials and the network do NOT.",
  "",
  "The command is an argument ARRAY and is executed without a shell.",
  "",
  "There is no unsandboxed fallback: if the sandbox cannot be established,",
  "the command does not run and the refusal is returned instead.",
].join("\n");
