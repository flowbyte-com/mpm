/**
 * Ambient types for the OpenCode plugin SDK contract this plugin consumes.
 *
 * Deliberately loose. Nothing is installed for these declarations to hand
 * type information to `tsc`:
 *
 *   - `@opencode/plugin` — the OpenCode 2 loader ALIASES this specifier
 *     for directory plugins (docs examples import it with no
 *     node_modules in sight). Signatures verified against the installed
 *     SDK types of opencode v2.0.30 (`Tool.Context`, `Plugin.define`,
 *     setup-cleanup contract) and against a live plugin load on
 *     2026-10-16.
 *
 *   - `@opencode-ai/plugin` / `@opencode-ai/sdk` — the OpenCode V1 SDK,
 *     used only by the historical implementation src/index.ts. The npm
 *     package and its transitive @opencode-ai/sdk dependency are no
 *     longer resolvable from the registry (removed in the OpenCode 1→2
 *     rework), which is WHY the ambient stub exists: the checkout must
 *     still typecheck without depending on dead packages. It is NOT part
 *     of the staged payload.
 *
 * The runtime contract is pinned by the isolated harness test
 * (`opencode run --standalone` invoking an mpm_* tool), not by
 * type-checking a vendored SDK copy.
 */

declare module "@opencode/plugin" {
  /** V2 tool execution context (verified: no `model` surfaced). */
  export interface ToolContext {
    sessionID?: string;
    agent?: string;
    messageID?: string;
    id?: string;
    signal?: AbortSignal;
  }

  /** Shape accepted by ToolEditor.add(). */
  export interface ToolAddSpec {
    name: string;
    description: string;
    /** JSON schema; the host validates, we treat it as opaque. */
    input: unknown;
    execute(
      input: unknown,
      context?: ToolContext,
    ):
      | Promise<{ content: string; [key: string]: unknown }>
      | { content: string; [key: string]: unknown }
      | undefined
      | void;
  }

  export interface ToolEditor {
    add(spec: ToolAddSpec): void;
  }

  /** A part pushed onto the system prompt for the current context build. */
  export interface SystemPromptPart {
    type: string;
    text: string;
    [key: string]: unknown;
  }

  /** `ctx.session.hook("context", ...)` event payload. */
  export interface SessionContextHookEvent {
    sessionID?: string;
    system: SystemPromptPart[];
    [key: string]: unknown;
  }

  /** `ctx.shell.hook("create.before", ...)` event payload. */
  export interface ShellCreateBeforeEvent {
    command: string;
    cwd?: string;
    timeout?: number;
    shell?: string;
    env: Record<string, string | undefined>;
    [key: string]: unknown;
  }

  export interface PluginContext {
    tool: {
      transform(fn: (editor: ToolEditor) => void | Promise<void>): Promise<void>;
    };
    session: {
      hook(
        name: "context",
        fn: (event: SessionContextHookEvent) => void | Promise<void>,
      ): Promise<void>;
    };
    shell: {
      hook(
        name: "create.before",
        fn: (event: ShellCreateBeforeEvent) => void | Promise<void>,
      ): Promise<void>;
    };
    [key: string]: unknown;
  }

  export interface PluginDefinition {
    id: string;
    setup(ctx: PluginContext): Promise<(() => void) | void> | (() => void) | void;
  }

  export const Plugin: {
    define(definition: PluginDefinition): PluginDefinition;
  };
}

declare module "@opencode-ai/plugin" {
  export interface ToolContext {
    sessionID?: string;
    model?: { id?: string; [key: string]: unknown };
    messageID?: string;
    [key: string]: unknown;
  }

  export interface ToolSpec {
    name?: string;
    description: string;
    input?: unknown;
    execute?(input: unknown, context?: ToolContext): unknown;
    [key: string]: unknown;
  }

  /** The V1 SDK's zod-like schema builder; loose stub for the V1 source only. */
  export interface SchemaBuilder {
    (t: unknown): SchemaLeaf;
    string(): SchemaLeaf;
    number(): SchemaLeaf;
    boolean(): SchemaLeaf;
    object(shape: unknown): SchemaLeaf;
    array(shape: unknown): SchemaLeaf;
    enum(values: unknown[]): SchemaLeaf;
    union(values: unknown[]): SchemaLeaf;
    literal(value: unknown): SchemaLeaf;
  }

  export interface SchemaLeaf {
    optional(): SchemaLeaf;
    describe(text: string): SchemaLeaf;
  }

  export const tool: {
    (spec: ToolSpec): ToolSpec;
    schema: SchemaBuilder;
  };

  export interface PluginModule {
    server: unknown;
    [key: string]: unknown;
  }

  export type PluginInput = {
    [key: string]: unknown;
  };

  export type Plugin = (
    ctx: PluginInput,
  ) =>
    | Promise<{ hooks?: Record<string, unknown>; [key: string]: unknown }>
    | { hooks?: Record<string, unknown>; [key: string]: unknown };
}

declare module "@opencode-ai/sdk" {
  /** Ambient stub for the removed V1 SDK package (see above). */
  export interface Model {
    id?: string;
    [key: string]: unknown;
  }
}
