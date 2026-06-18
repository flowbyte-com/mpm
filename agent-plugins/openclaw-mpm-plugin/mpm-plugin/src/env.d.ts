declare module "openclaw/plugin-sdk/plugin-entry.js" {
  import type { OpenClawPluginApi } from "openclaw/plugin-sdk/index";

  type OpenClawPluginConfigSchema = {
    type: "object";
    properties?: Record<string, unknown>;
    additionalProperties?: boolean;
  };

  type DefinePluginEntryOptions = {
    id: string;
    name: string;
    description: string;
    configSchema?: OpenClawPluginConfigSchema | (() => OpenClawPluginConfigSchema);
    register: (api: OpenClawPluginApi) => void;
  };

  type DefinedPluginEntry = {
    id: string;
    name: string;
    description: string;
    configSchema: OpenClawPluginConfigSchema;
    register: (api: OpenClawPluginApi) => void;
  };

  export function definePluginEntry(options: DefinePluginEntryOptions): DefinedPluginEntry;
}

declare module "openclaw/plugin-sdk/index.js" {
  export interface OpenClawPluginApi {
    registerTool(
      factory: (ctx: OpenClawPluginToolContext) => AnyAgentTool,
      options?: { names?: string[]; optional?: boolean }
    ): void;
  }

  export interface OpenClawPluginToolContext {
    [key: string]: unknown;
  }

  export interface AnyAgentTool {
    name: string;
    description: string;
    parameters: Record<string, unknown>;
    emoji_name?: string;
    execute: (toolCallId: string, params: Record<string, unknown>) => Promise<{
      toolCallId: string;
      result: {
        type: string;
        results: Array<{
          content: Array<{ type: string; text: string }>;
        }>;
      };
    }>;
  }
}
