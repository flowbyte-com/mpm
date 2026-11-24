// The reviewer's provider credential belongs to the INFERENCE process, not
// to the model-controlled `sandbox_exec` path. This unit test pins the
// scrubber the plugin uses before it spawns its helper.
import { test } from "node:test";
import assert from "node:assert/strict";
import { sandboxProcessEnv } from "../dist/sandbox-exec.js";

test("sandboxProcessEnv strips provider and worker credentials", () => {
  const env = sandboxProcessEnv({
    PATH: "/usr/bin",
    HOME: "/tmp/home",
    MINIMAX_API_KEY: "marker-provider",
    DEEPSEEK_API_KEY: "marker-worker",
    MPM_OPENCODE_REVIEWER_KEY: "marker-reviewer",
    MPM_OPENCODE_SIDECAR_KEY: "marker-sidecar",
    OPENAI_API_KEY: "marker-openai",
    ANTHROPIC_API_KEY: "marker-anthropic",
  });
  assert.equal(env.PATH, "/usr/bin");
  assert.equal(env.HOME, "/tmp/home");
  for (const name of [
    "MINIMAX_API_KEY",
    "DEEPSEEK_API_KEY",
    "MPM_OPENCODE_REVIEWER_KEY",
    "MPM_OPENCODE_SIDECAR_KEY",
    "OPENAI_API_KEY",
    "ANTHROPIC_API_KEY",
  ]) {
    assert.equal(env[name], undefined, `${name} reached the sandbox helper`);
  }
});
