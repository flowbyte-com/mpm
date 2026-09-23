# MPM Integration Check

Use this procedure to verify that the current agent host is correctly integrated with MPM.

This is a diagnostic check. Do not modify MPM configuration, reinstall components, repair files, or change agent settings unless explicitly asked.

## 1. Identify the host

Determine which agent/framework you are currently running under.

Examples include:

* Claude Code
* OpenClaw
* OpenCode
* Pi
* Hermes

Report the detected host and the evidence used to identify it.

## 2. Confirm MPM availability

Determine whether MPM is available to this agent through its expected integration surface.

Check for:

* MPM MCP/tool registration
* host-specific integration files or hooks
* provenance/environment variables where applicable
* access to the configured MPM workspace
* availability of the expected MPM tools

Do not assume integration is working merely because the `mpm` CLI exists on PATH.

## 3. Verify the MCP surface

If MCP tools are available, enumerate the registered MPM tools.

The current expected registered surface is 21 tools:

* `mpm_memory`
* `mpm_theories`
* `mpm_decisions`
* `mpm_lessons`
* `mpm_topics`
* `mpm_references`
* `mpm_evidence`
* `mpm_confidence`
* `mpm_retrieval_diagnose`
* `mpm_context`
* `mpm_skills`
* `mpm_wakes`
* `mpm_handoff`
* `mpm_scratchpad`
* `mpm_system`
* `mpm_log_to_changelog`
* `mpm_request_review`
* `mpm_resolve`
* `mpm_blob_read`
* `mpm_blob_search`
* `mpm_work`

If the host intentionally exposes only a filtered/core subset, identify that fact rather than treating it as an automatic failure.

## 4. Perform safe read-only checks

Using the native MPM integration available to this agent, perform safe read-only operations where supported.

At minimum verify:

* MPM health/status is reachable
* wake/context information can be read
* existing MPM context can be retrieved without error
* skill discovery is reachable if exposed

Prefer native MCP/tool calls over shelling out to the `mpm` CLI.

Do not create test memories, wakes, handoffs, work items, or other persistent records merely to prove connectivity.

## 5. Verify wake/context integration

Confirm that the host can access the normal MPM context path.

Where available, inspect the equivalent of:

* wake context
* pending/overdue wakes
* `contextual_focus`
* session/handoff context

Do not acknowledge, resolve, retire, or otherwise mutate existing wakes during this diagnostic.

## 6. Check host-specific integration

Inspect the relevant host integration under:

`~/.mpm/agent_installation/`

Confirm that the installed integration matches the detected host.

Check only what applies to that host, such as:

* MCP registration
* hooks
* instruction files
* provenance environment wiring
* mode/persona integration
* host-specific configuration

Do not repair discrepancies during this check.

## 7. Distinguish CLI availability from agent integration

Explicitly report these separately:

* `mpm` CLI installed: YES/NO
* MPM MCP/tools visible to the current agent: YES/NO
* host-specific integration present: YES/NO
* MPM context successfully readable through the agent integration: YES/NO

A working CLI alone does not prove that the agent is integrated with MPM.

## 8. Natural-use readiness

Based only on the checks above, determine whether the agent is in a state where it could naturally use MPM during ordinary work.

Do not judge readiness based on whether the agent has happened to invoke MPM previously.

Report whether the following capabilities are available to the agent when appropriate:

* retrieve relevant context/memory
* persist useful durable information
* use handoffs
* discover/use skills
* inspect or schedule wakes
* use MPM work tracking
* recover contextual continuity across sessions

## Final report

Return a concise table containing:

1. detected host
2. MPM CLI available: YES/NO
3. host integration installed: YES/NO
4. MCP integration present: YES/NO
5. registered MPM tool count
6. expected tool exposure model: full / filtered / unknown
7. context read succeeds: YES/NO
8. wake context reachable: YES/NO
9. skills reachable: YES/NO
10. provenance/environment wiring present: YES/NO/N/A
11. mode/persona integration present: YES/NO/N/A
12. integration errors found
13. natural-use readiness: READY / PARTIAL / NOT READY
14. recommended next action, if any

Do not modify the installation during this check.

