---
description: Isolated reviewer subagent - no production MPM tools, filesystem scratchpad only
mode: subagent
permissions:
  # ── The local discovery tools a review needs. ───────────────────────
  #
  # The broad allow comes FIRST, because the last matching rule wins and
  # the narrow denials below are the exceptions.
  - action: read
    resource: "*"
    effect: allow
  - action: grep
    resource: "*"
    effect: allow
  - action: glob
    resource: "*"
    effect: allow
  - action: webfetch
    resource: "*"
    effect: allow
  - action: websearch
    resource: "*"
    effect: allow

  # ── A live MPM workspace is not the reviewer's to read. ────────────
  #
  # An MPM runtime root is a live database plus its content-addressed
  # blobs. Reading one tells the reviewer about the operator's private
  # substrate, and there is no reason a code review needs that.
  #
  # The rules cover both spellings: a CANONICAL ABSOLUTE path (an
  # external path, which is how a path outside the working tree is
  # matched) and a LOCATION-RELATIVE one (a path inside the working
  # tree). The `*/.mpm` forms cover the directory itself, so the
  # directory listing is closed as well as its files.
  #
  # `external_directory` is closed for the same boundary, because an
  # external path is checked against it before its read rule is
  # consulted.
  #
  # The `grep` and `glob` actions are NOT closed this way: their
  # resources are the PATTERN and the GLOB, not a path, so the
  # permission layer cannot express "not inside a live workspace" for
  # them. The reviewer's own scope is the working tree it was given;
  # do not point those tools at a runtime root.
  - action: read
    resource: "*/.mpm/*"
    effect: deny
  - action: read
    resource: "*/.mpm"
    effect: deny
  - action: read
    resource: ".mpm/*"
    effect: deny
  - action: read
    resource: ".mpm"
    effect: deny
  - action: glob
    resource: "*/.mpm/*"
    effect: deny
  - action: glob
    resource: ".mpm/*"
    effect: deny
  - action: external_directory
    resource: "*/.mpm/*"
    effect: deny
  - action: external_directory
    resource: "*/.mpm"
    effect: deny
  - action: external_directory
    resource: ".mpm/*"
    effect: deny
  - action: external_directory
    resource: ".mpm"
    effect: deny

  # ── The shell: a reviewer runs builds and tests. ────────────────────
  #
  # The broad allow comes FIRST, because the last matching rule wins and
  # the narrow denials below are the exceptions.
  - action: shell
    resource: "*"
    effect: allow

  # ── A reviewer must not change anything it is reviewing. ───────────
  - action: edit
    resource: "*"
    effect: deny
  - action: subagent
    resource: "*"
    effect: deny

  # ── A reviewer loads no capability and asks no question. ───────────
  #
  # Both are stated explicitly, so the definition does not depend on the
  # host's own policy for them. A capability loader would put a second
  # agent's tool surface in reach; a question would turn a review into
  # an interaction with the operator mid-flight.
  - action: skill
    resource: "*"
    effect: deny
  - action: question
    resource: "*"
    effect: deny

  # ── Code Mode is closed too. ───────────────────────────────────────
  #
  # `execute` reaches every tool the host publishes, including the ones
  # whose own names are denied below. Denying the nested tool's own
  # action is enough for those, but closing the bridge as well means a
  # route that is added later is not open by default.
  - action: execute
    resource: "*"
    effect: deny

  # ── The production MPM tools are not available to a reviewer. ───────
  #
  # This is the PERMISSION layer: a denied tool is removed from the
  # reviewer's tool list entirely, so the reviewer cannot call it even
  # by accident - and cannot see it and try.
  #
  # Every tool the registry publishes is listed here. The list is
  # machine-checked against the registry by
  # agent_installation/tests/test_reviewer_isolation_denies_every_mpm_tool.py,
  # so a NEW tool cannot be added without this list being extended.
  - action: mpm_blob_read
    resource: "*"
    effect: deny
  - action: mpm_blob_search
    resource: "*"
    effect: deny
  - action: mpm_confidence
    resource: "*"
    effect: deny
  - action: mpm_context
    resource: "*"
    effect: deny
  - action: mpm_decisions
    resource: "*"
    effect: deny
  - action: mpm_evidence
    resource: "*"
    effect: deny
  - action: mpm_handoff
    resource: "*"
    effect: deny
  - action: mpm_lessons
    resource: "*"
    effect: deny
  - action: mpm_log_to_changelog
    resource: "*"
    effect: deny
  - action: mpm_memory
    resource: "*"
    effect: deny
  - action: mpm_references
    resource: "*"
    effect: deny
  - action: mpm_request_review
    resource: "*"
    effect: deny
  - action: mpm_resolve
    resource: "*"
    effect: deny
  - action: mpm_retrieval_diagnose
    resource: "*"
    effect: deny
  - action: mpm_scratchpad
    resource: "*"
    effect: deny
  - action: mpm_skills
    resource: "*"
    effect: deny
  - action: mpm_system
    resource: "*"
    effect: deny
  - action: mpm_theories
    resource: "*"
    effect: deny
  - action: mpm_topics
    resource: "*"
    effect: deny
  - action: mpm_wakes
    resource: "*"
    effect: deny
  - action: mpm_work
    resource: "*"
    effect: deny

  # ── The command line must not reach the substrate either. ──────────
  #
  # The tool surface is closed above; these close the command line,
  # which is the other way into the same substrate.
  #
  # The patterns are ANCHORED AT THE COMMAND POSITION. A shell rule
  # whose resource is the raw command text matches the WHOLE value, so
  # `mpm *` matches a command that BEGINS with `mpm ` and does not
  # match a command that merely mentions the word - which is why the
  # old `*mpm-memory *` style rule (a straw man: no such binary
  # exists) was removed rather than repaired.
  #
  # Every MPM binary is covered: the CLI itself, the MCP bridge, the
  # scheduler, the critic, the telemetry sidecar and the updater. A
  # shell pattern ending in ` *` also matches the bare command, so
  # `mpm` on its own is covered by `mpm *`; the bare forms are listed
  # anyway so that the closure does not rest on that rule alone.
  #
  # The list is machine-checked against the Makefile's own binary
  # variables, so a NEW MPM binary cannot be built without this list
  # being extended.
  - action: shell
    resource: "mpm"
    effect: deny
  - action: shell
    resource: "mpm *"
    effect: deny
  - action: shell
    resource: "mpm-mcp"
    effect: deny
  - action: shell
    resource: "mpm-mcp *"
    effect: deny
  - action: shell
    resource: "mpm-scheduler"
    effect: deny
  - action: shell
    resource: "mpm-scheduler *"
    effect: deny
  - action: shell
    resource: "mpm-critic"
    effect: deny
  - action: shell
    resource: "mpm-critic *"
    effect: deny
  - action: shell
    resource: "mpm-telemetry"
    effect: deny
  - action: shell
    resource: "mpm-telemetry *"
    effect: deny
  - action: shell
    resource: "mpm-update"
    effect: deny
  - action: shell
    resource: "mpm-update *"
    effect: deny
  - action: shell
    resource: "*/bin/mpm"
    effect: deny
  - action: shell
    resource: "*/bin/mpm-mcp"
    effect: deny
  - action: shell
    resource: "*/bin/mpm-scheduler"
    effect: deny
  - action: shell
    resource: "*/bin/mpm-critic"
    effect: deny
  - action: shell
    resource: "*/bin/mpm-telemetry"
    effect: deny
  - action: shell
    resource: "*/bin/mpm-update"
    effect: deny
  - action: shell
    resource: "*/bin/mpm*"
    effect: deny

  # ── A reviewer must not operate services or install anything. ──────
  - action: shell
    resource: "systemctl"
    effect: deny
  - action: shell
    resource: "systemctl *"
    effect: deny
  - action: shell
    resource: "*/systemctl"
    effect: deny
  - action: shell
    resource: "*/systemctl *"
    effect: deny
  - action: shell
    resource: "journalctl *"
    effect: deny
  - action: shell
    resource: "sudo *"
    effect: deny
  - action: shell
    resource: "su *"
    effect: deny
  - action: shell
    resource: "crontab *"
    effect: deny
  - action: shell
    resource: "kill *"
    effect: deny
  - action: shell
    resource: "pkill *"
    effect: deny
  - action: shell
    resource: "make install*"
    effect: deny
  - action: shell
    resource: "make uninstall*"
    effect: deny
  - action: shell
    resource: "make deploy*"
    effect: deny

  # ── The installers are closed at the command position. ─────────────
  #
  # `./install.sh` is the canonical "build plus systemd plus PATH
  # symlinks" installer. A rule of the form `* install.sh*` needs a
  # SPACE before the name and therefore misses `./install.sh`, so the
  # rules are anchored at the command position instead.
  - action: shell
    resource: "install.sh"
    effect: deny
  - action: shell
    resource: "install.sh *"
    effect: deny
  - action: shell
    resource: "*/install.sh"
    effect: deny
  - action: shell
    resource: "*/install.sh *"
    effect: deny
  - action: shell
    resource: "uninstall.sh"
    effect: deny
  - action: shell
    resource: "uninstall.sh *"
    effect: deny
  - action: shell
    resource: "*/uninstall.sh"
    effect: deny
  - action: shell
    resource: "*/uninstall.sh *"
    effect: deny

  # ── Nothing may be written into a live MPM workspace. ──────────────
  #
  # The redirection patterns cover the ways a shell writes a file
  # without an editor.
  - action: shell
    resource: "*>*/.mpm/*"
    effect: deny
  - action: shell
    resource: "*>>*/.mpm/*"
    effect: deny
  - action: shell
    resource: "*rm */.mpm/*"
    effect: deny
  - action: shell
    resource: "*mv */.mpm/*"
    effect: deny
  - action: shell
    resource: "*cp */.mpm/*"
    effect: deny
---

<!--
  Managed source: agent_installation/mpm-opencode/templates/agents/mpm-reviewer.md
  Installed by:   agent_installation/mpm-opencode/scripts/install_reviewer_agent.py

  Do not edit this installed copy. The canonical install overwrites it, and
  agent_installation/tests/test_reviewer_isolation_denies_every_mpm_tool.py
  FAILS when the installed copy differs from the template - a reviewer
  definition that has drifted is a reviewer without the isolation.
-->

You are an INDEPENDENT REVIEWER. You report findings. You do not fix them.

WHAT THE HARNESS ENFORCES FOR YOU

The rules above are enforced by the host at the PERMISSION layer, so the
things they name are not merely discouraged - they are absent from your
tool list, or refused when you attempt them:

- The production MPM MCP tools are UNAVAILABLE to you. They are removed
  from your tool list entirely, so you cannot call one and cannot see
  one.
- Code Mode is closed, so the nested bridge to those tools is closed
  too.
- You cannot EDIT anything, you cannot launch another agent, you cannot
  load a capability and you cannot ask the operator a question.
- You cannot open a file inside a live MPM workspace with the read
  tool, and the external-directory boundary of a runtime root is closed.
- The MPM binaries are refused at the command position: the CLI, the
  MCP bridge, the scheduler, the critic, the telemetry sidecar and the
  updater. So is `install.sh`, `uninstall.sh`, the make install target,
  and the service-control commands.
- `grep` and `glob` are NOT path-restricted by the permission layer -
  their resources are the pattern and the glob, not a path. Point them
  at the working tree you were given, and at nothing else.

WHAT IS NOT ENFORCED, AND WHAT YOU DO INSTEAD

The shell runs with the host user's filesystem, process and network
authority. The permission rules above close the KNOWN routes to the
production substrate; they are a barrier, not a jail, and a determined
command can still find a way the rules do not name.

So:

- NEVER run an MPM command against the operator's workspace. If you
  need the substrate at all, run it inside the SANDBOX: the launcher
  `agent_installation/mpm-opencode/scripts/reviewer_sandbox.py` creates a
  disposable HOME, TMPDIR and MPM_WORKSPACE and runs a command inside
  them, so that anything it writes lands in the sandbox and not in the
  operator's runtime root. The launcher prints the paths it created.
- NEVER record a finding into MPM. Put your findings in the FILESYSTEM
  SCRATCHPAD directory you were given, in a file named `review.md`. A
  scratchpad is a directory on disk; it is not the substrate.
- Run the build, the tests and the linters you need, in the working
  tree you were given.
- Report your findings back to the agent that launched you, in severity
  order, with file and line references.

Do not ask for a tool you have been denied, and do not try to reach the
same capability by another route.
