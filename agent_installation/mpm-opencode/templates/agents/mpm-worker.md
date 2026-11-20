---
description: MPM continuity worker - file-edit only, inside an OS-enforced namespace
mode: primary
permissions:
  # ── The DEFAULT is DENY. ───────────────────────────────────────────────
  #
  # Every action is closed until something re-opens it. The host prepends
  # its own rules ahead of these and resolves LAST-MATCH-WINS, so the
  # wildcard deny below is the first AGENT rule: anything a later rule does
  # not explicitly re-open stays closed, including capabilities a future
  # host release adds.
  - action: "*"
    resource: "*"
    effect: deny

  # ── The worktree, and nothing else. ────────────────────────────────────
  #
  # The resource patterns are RELATIVE globs because that is the form the
  # host's matcher compares against for the file tools. The OS boundary is
  # what makes this safe: inside the namespace there IS nothing else to
  # reach — no operator home, no production database, no unrelated
  # repository. These rules narrow the surface; the namespace is the
  # guarantee.
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
  # edits files and nothing else. The reason is measured, not stylistic.
  # The worker's network namespace is unshared, but its own loopback is up
  # (the inference relay listens there), and a shell would let the model
  # originate connections — to the relay, and to anything else the
  # namespace can reach. A confined command capability may be added later,
  # after the namespace and network boundary have been verified
  # independently; it is not in PR 3.
  - action: shell
    resource: "*"
    effect: deny

  # ── No host-side capability tools. ─────────────────────────────────────
  #
  # `read`, `write` and friends above run inside the runtime; these run in
  # the host's process (or reach outside the namespace) and are not
  # confined by it.
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
  # runs outside the namespace and fences every write on the claim epoch.
  - action: mpm_*
    resource: "*"
    effect: deny
  - action: opencode_*
    resource: "*"
    effect: deny
---

You are a CONTINUITY WORKER executing one MPM assignment.

Your objective is in the prompt. You are inside an operating-system
sandbox: only the worktree, a scratch directory and the runtime's own
files exist. There is no shell, no network and no host filesystem.

Work by editing files. Report what you changed. Do not claim work you did
not do: a run that produces no change is a legitimate outcome and must be
reported as one.

<!--
  Managed source: agent_installation/mpm-opencode/templates/agents/mpm-worker.md
  Installed by:   agent_installation/mpm-opencode/scripts/install_worker_agent.py

  WHAT THIS DEFINITION IS NOT
    It is not an autonomous agent framework, and it is not a coding
    environment. It edits files in one worktree. Decision F4 of Continuity
    001 PR 3 chose whole-process OS containment over permission
    declarations, precisely because a permission declaration is not a
    boundary — the host honours a SESSION-LEVEL permission grant over an
    agent-level deny, so the namespace is what constrains this worker.

  WHY `shell` IS DENIED RATHER THAN CONFINED
    A confined command capability is a worthwhile future addition, but it
    needs the namespace and network boundary verified independently first.
    PR 3 keeps the worker file-edit-only so the boundary it ships is one it
    can prove.

  WHY THE MPM TOOLS ARE DENIED
    The adapter is the only party that records execution state, and it does
    so outside this namespace, fenced on the claim epoch. A worker able to
    write `mpm_assignment` could manufacture the record its own work is
    judged by.
-->
