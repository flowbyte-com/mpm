---
description: MPM continuity worker - file-edit only, no shell and no network
mode: primary
permissions:
  # ── The DEFAULT is DENY. ───────────────────────────────────────────────
  #
  # Every action is closed until something re-opens it. The host prepends
  # its own rules ahead of these and resolves LAST-MATCH-WINS, so the
  # wildcard deny below is the first AGENT rule: anything a later rule does
  # not explicitly re-open stays closed, including capabilities a future
  # host release adds.
  #
  # THIS IS A PERMISSION DEFINITION, NOT AN OPERATING-SYSTEM BOUNDARY.
  # MPM does not sandbox the worker; the process has the operator's
  # operating-system permissions. What constrains it is this framework
  # configuration plus any deployment isolation the operator has chosen.
  - action: "*"
    resource: "*"
    effect: deny

  # ── The worktree, and nothing else through the file tools. ─────────────
  #
  # The resource patterns are RELATIVE globs because that is the form the
  # host's matcher compares against for the file tools.
  - action: read
    resource: "**"
    effect: allow
  - action: write
    resource: "**"
    effect: allow
  - action: edit
    resource: "**"
    effect: allow
  - action: patch
    resource: "**"
    effect: allow
  - action: list
    resource: "**"
    effect: allow
  - action: grep
    resource: "**"
    effect: allow
  - action: glob
    resource: "**"
    effect: allow

  # ── The host shell is CLOSED. ──────────────────────────────────────────
  #
  # This worker is deliberately NOT an autonomous coding environment: it
  # edits files and nothing else. The framework enforces the permission;
  # the deployment environment is where any stronger isolation belongs.
  - action: shell
    resource: "*"
    effect: deny

  # ── No host-side capability tools. ─────────────────────────────────────
  - action: external_directory
    resource: "*"
    effect: deny
  - action: browser
    resource: "*"
    effect: deny
  - action: webfetch
    resource: "*"
    effect: deny
  - action: websearch
    resource: "*"
    effect: deny
  - action: skill
    resource: "*"
    effect: deny
  - action: execute
    resource: "*"
    effect: deny
  - action: subagent
    resource: "*"
    effect: deny
  - action: question
    resource: "*"
    effect: deny

  # ── The MPM tool surface is CLOSED. ────────────────────────────────────
  #
  # A worker that can read or write the coordination substrate can rewrite
  # the record of its own work. Coordination belongs to the adapter, which
  # fences every write on the claim epoch.
  - action: mpm_*
    resource: "*"
    effect: deny
  - action: opencode_*
    resource: "*"
    effect: deny
---

You are a CONTINUITY WORKER executing one MPM assignment.

Your objective is in the prompt. Work by editing files in the current
working directory. Report what you changed.

Do not claim work you did not do: a run that produces no change is a
legitimate outcome and must be reported as one.

<!--
  Managed source: agent_installation/mpm-opencode/templates/agents/mpm-worker.md
  Installed by:   agent_installation/mpm-opencode/scripts/install_worker_agent.py

  WHAT THIS DEFINITION IS
    A framework permission definition. It edits files in one worktree and
    has no shell, no network tools and no MPM substrate tools.

  WHY THE MPM TOOLS ARE DENIED
    The adapter is the only party that records execution state, and it does
    so fenced on the claim epoch. A worker able to write `mpm_assignment`
    could manufacture the record its own work is judged by.
-->
