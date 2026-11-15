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

  # ── A reviewer asks no question. ───────────────────────────────────
  #
  # A question would turn a review into an interaction with the
  # operator mid-flight, so the action is closed explicitly and the
  # definition does not depend on the host's own policy for it.
  #
  # THE CAPABILITY LOADER IS NOT CLOSED HERE, and there is no rule for
  # it: the host does not gate the `skill` tool by any permission
  # action. Measured on this host with eighteen candidate action names
  # (`skill`, `capability`, `skills`, `skill.load`, `skill.use`,
  # `tool.use`, and others) and with the V1-style `tools:` frontmatter:
  # the loader stayed in the reviewer's catalogue and a `skill` call
  # ran. A rule that does not work is not a closure, so there is no
  # rule; the prose below says what a reviewer must therefore do.
  - action: question
    resource: "*"
    effect: deny

  # ── The MCP RESOURCE surface is closed. ────────────────────────────
  #
  # The MCP TOOLS are denied by name above. The resource surface is a
  # different door into the same server, so it is closed as well: a
  # resource can carry the substrate's bytes just as a tool can.
  #
  # The session and worktree MUTATIONS go with it: a reviewer reports
  # findings; it does not move the session it is running in, rename it,
  # or create a worktree of its own.
  - action: "opencode_*"
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
  # Every MPM binary is covered: the CLI itself, the MCP bridge, the
  # scheduler, the critic, the telemetry sidecar and the updater. The
  # list is machine-checked against the Makefile's own binary
  # variables, so a NEW MPM binary cannot be built without this list
  # being extended.
  #
  # A shell rule's resource is the raw command text and the match is
  # over the WHOLE value, so a rule that does not begin with a
  # wildcard is anchored AT THE COMMAND POSITION: `mpm *` matches a
  # command that BEGINS with `mpm ` and does not match a command that
  # merely mentions the word.
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
    resource: "mpm-lint"
    effect: deny
  - action: shell
    resource: "mpm-lint *"
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
    resource: "*/bin/mpm-lint"
    effect: deny
  - action: shell
    resource: "*/bin/mpm*"
    effect: deny

  # ── The CLI is closed wherever it is NAMED, not only at the
  #    command position. ─────────────────────────────────────────────
  #
  # A command-position rule is defeated by a ONE-WORD PREFIX, and the
  # prefixes are ordinary ones: `env`, `sh -c`, `timeout`, `xargs`,
  # `nohup`, `nice`, `command`, `exec`. Measured on this host with
  # only the command-position rules in force: `env mpm --version`
  # RAN, `sh -c 'mpm --version'` RAN, and `env mpm remember "..."`
  # wrote a memory into the substrate's live database. That is the
  # incident this whole mechanism exists to prevent, so a rule that a
  # prefix defeats is not a closure.
  #
  # The shell's matcher is a TEXT matcher, so a rule's literal has to
  # appear in the command text. What matters is WHERE it appears.
  #
  # The binaries are closed by their NAME standing as its OWN WORD:
  #
  #   - the name preceded by a space and followed by a space — `env
  #     mpm ...`, `xargs mpm ...`, `grep -rn mpm .`, and the bare
  #     `mpm` at the end of a command;
  #   - the name preceded by a space and followed by a closing
  #     parenthesis — `$(echo mpm)`, where the name is the argument
  #     of a substitution;
  #   - the name preceded by a quote — `"mpm" --version` and
  #     `'mpm' --version`, where the name is a quoted word.
  #
  # The SPACE BEFORE the name is deliberate. A rule of the form
  # `*mpm *` also matches a PATH whose last segment is the name, so
  # it refuses `ls /path/to/mpm` and `git -C /path/to/mpm log`
  # — the operator's own checkout, read-only, for no reason a review
  # cares about. Requiring a space before the name keeps the closure
  # and drops the false denial: the name is then a WORD, and a path
  # is not.
  #
  # The last form is shared by every binary because every one of them
  # starts with `mpm`.
  #
  # The cost that REMAINS is a quoted name: `grep -rn "mpm_memory" .`
  # is refused, while the unquoted `grep -rn mpm_memory .` is not.
  # That is the host's own quoted-segment rule, not one of these, and
  # it is accepted: quoting is the natural way to write a search, so
  # a reviewer that meets the refusal should retry unquoted.
  #
  # The list is machine-checked against the Makefile's own binary
  # variables, so a NEW MPM binary cannot be built without this list
  # being extended.
  - action: shell
    resource: "* mpm *"
    effect: deny
  - action: shell
    resource: "* mpm-mcp *"
    effect: deny
  - action: shell
    resource: "* mpm-scheduler *"
    effect: deny
  - action: shell
    resource: "* mpm-critic *"
    effect: deny
  - action: shell
    resource: "* mpm-telemetry *"
    effect: deny
  - action: shell
    resource: "* mpm-update *"
    effect: deny
  - action: shell
    resource: "* mpm-lint *"
    effect: deny
  - action: shell
    resource: "* mpm)*"
    effect: deny
  - action: shell
    resource: "* mpm-mcp)*"
    effect: deny
  - action: shell
    resource: "* mpm-scheduler)*"
    effect: deny
  - action: shell
    resource: "* mpm-critic)*"
    effect: deny
  - action: shell
    resource: "* mpm-telemetry)*"
    effect: deny
  - action: shell
    resource: "* mpm-update)*"
    effect: deny
  - action: shell
    resource: "* mpm-lint)*"
    effect: deny
  - action: shell
    resource: "*\"mpm*"
    effect: deny
  - action: shell
    resource: "*'mpm*"
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
  # The `-C` form is closed too: `make -C <dir> install` runs the same
  # recipe, and it does not begin with `make install`.
  - action: shell
    resource: "make -C * install*"
    effect: deny
  - action: shell
    resource: "make -C * uninstall*"
    effect: deny
  - action: shell
    resource: "make -C * deploy*"
    effect: deny

  # ── The installers are closed at the command position. ─────────────
  #
  # `./install.sh` is the canonical "build plus systemd plus PATH
  # symlinks" installer, and `install.sh` is the bare form. A rule of
  # the form `* install.sh*` needs a SPACE before the name and
  # therefore misses `./install.sh`, so the rules are anchored at the
  # command position instead.
  #
  # The harm is RUNNING the installer, not READING it: a review of this
  # repository needs to read `install.sh`, and a rule of the form
  # `*/install.sh` refuses that read as well — it matches any command
  # that MENTIONS the path, `ls -la agent_installation/mpm-opencode/
  # install.sh` included. So the path forms are closed at the command
  # position (`./install.sh`) and through the interpreters that can
  # run a script file (`sh`, `bash`), and a bare path in an argument
  # is not closed at all.
  - action: shell
    resource: "install.sh"
    effect: deny
  - action: shell
    resource: "install.sh *"
    effect: deny
  - action: shell
    resource: "./install.sh"
    effect: deny
  - action: shell
    resource: "./install.sh *"
    effect: deny
  - action: shell
    resource: "sh install.sh *"
    effect: deny
  - action: shell
    resource: "bash install.sh *"
    effect: deny
  - action: shell
    resource: "sh */install.sh *"
    effect: deny
  - action: shell
    resource: "bash */install.sh *"
    effect: deny
  - action: shell
    resource: "uninstall.sh"
    effect: deny
  - action: shell
    resource: "uninstall.sh *"
    effect: deny
  - action: shell
    resource: "./uninstall.sh"
    effect: deny
  - action: shell
    resource: "./uninstall.sh *"
    effect: deny
  - action: shell
    resource: "sh uninstall.sh *"
    effect: deny
  - action: shell
    resource: "bash uninstall.sh *"
    effect: deny
  - action: shell
    resource: "sh */uninstall.sh *"
    effect: deny
  - action: shell
    resource: "bash */uninstall.sh *"
    effect: deny

  # ── Nothing may be written into a live MPM workspace. ──────────────
  #
  # The WRITE routes are the shell's own verbs: a copy, a move and a
  # removal, each with the workspace path in the command.
  #
  # A REDIRECTION is listed too, and it is the one rule here with a
  # known cost. The permission layer's shell resource is the raw
  # command text, and the host does NOT intercept a redirection's
  # target: a command that writes into a runtime root is executed
  # unless a rule says otherwise. So the rule is a path substring, and
  # a path substring matches TEXT as well as a path — a heredoc that
  # merely QUOTES a runtime path after a ">" is refused too.
  #
  # That cost is accepted deliberately: the alternative is a reviewer
  # that can overwrite the operator's live database, and a stray file
  # in a runtime root is a nuisance while a corrupted database is a
  # disaster. The workaround is to assemble such a path at runtime
  # rather than writing it literally.
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
- The MCP resource surface and the session and worktree management
  tools are closed: a resource can carry the substrate's bytes just as
  a tool can, and a reviewer does not move its own session.
- You cannot EDIT anything, you cannot launch another agent and you
  cannot ask the operator a question.
- You cannot open a file inside a live MPM workspace with the read
  tool, and the external-directory boundary of a runtime root is closed.
- The MPM binaries are refused wherever they are NAMED: the CLI, the
  MCP bridge, the scheduler, the critic, the telemetry sidecar, the
  updater and the linter. A name standing as its own WORD is caught,
  so a prefix (`env mpm ...`), a substitution (`$(echo mpm)`) or a
  quote (`"mpm" ...`) does not carry you past it. So is `install.sh`,
  `uninstall.sh`, the make install target, and the service-control
  commands.
- `grep` and `glob` are NOT path-restricted by the permission layer -
  their resources are the pattern and the glob, not a path. Point them
  at the working tree you were given, and at nothing else.

WHAT IS NOT ENFORCED, AND WHAT YOU DO INSTEAD

The shell runs with the host user's filesystem, process and network
authority. The permission rules above close the KNOWN routes to the
production substrate; they are a barrier, not a jail, and a determined
command can still find a way the rules do not name.

THREE things are NOT closed, and you must not rely on them being closed:

- The BROWSER is still available to you. Its tools are not governed by
  the permission rules, so a `file://` URL could open a file on this
  machine. The browser is inert unless a desktop browser is connected
  to this session, but if one IS connected, do not use it. A code
  review does not need a browser.
- The CAPABILITY LOADER is still available to you. The host does not
  gate the `skill` tool by any permission action, so loading a
  capability is your own discipline. Do not load one: a capability is
  text from elsewhere, and a review that has read instructions it did
  not ask for is not a review you can trust.
- The shell's matcher sees TEXT, and two routes are NOT caught. A name
  SPLIT by a substitution is not the name: `$(echo mp)m` assembles it
  and runs the binary. And the host exempts `cd` from the name rule,
  so `cd cmd/mpm && go run . --version` reaches the binary without
  naming it. A script file runs the binary too. The rules also close
  the write routes into a runtime root by matching the path, so a
  command that merely QUOTES such a path after a ">" is refused. If a
  command of yours is refused for either reason, assemble the text at
  runtime instead of writing it literally — and remember that the
  SANDBOX below is what makes such a route harmless.

So:

- RUN EVERY COMMAND INSIDE THE SANDBOX. That is the default, not an
  option: the launcher
  `agent_installation/mpm-opencode/scripts/reviewer_sandbox.py` creates
  a disposable HOME, TMPDIR and MPM_WORKSPACE and runs a command
  inside them, so that anything it writes lands in the sandbox and not
  in the operator's runtime root. Use it as
  `reviewer_sandbox.py run -- <command>`; the launcher prints the
  paths it created. The permission rules above are a barrier, not a
  jail: a route they do not name is still a route, and the sandbox is
  what makes such a route harmless.
- NEVER run an MPM command against the operator's workspace, in any
  spelling. If you need the substrate at all, run it inside the
  sandbox.
- NEVER record a finding into MPM. Put your findings in the FILESYSTEM
  SCRATCHPAD directory you were given, in a file named `review.md`. A
  scratchpad is a directory on disk; it is not the substrate.
- Run the build, the tests and the linters you need, in the working
  tree you were given.
- Report your findings back to the agent that launched you, in severity
  order, with file and line references.

Do not ask for a tool you have been denied, and do not try to reach the
same capability by another route.
