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
  # The binaries are closed in SEVEN shapes, each measured as refused:
  #
  #   1. the name at the COMMAND POSITION — `mpm ...` (the rules
  #      above);
  #   2. the name as its own WORD — `env mpm ...`, `xargs mpm ...`,
  #      `grep -rn mpm .`, the bare `mpm` at the end of a command,
  #      and `$(echo mpm)` — the rule is `* mpm *`, so a SPACE
  #      precedes the name;
  #   3. the name separated from its neighbours by a TAB —
  #      `mpm<TAB>--version`, `env mpm<TAB>--version`,
  #      `echo<TAB>mpm` — the rules are `*mpm\t*` (a tab AFTER the
  #      name) and `*\tmpm*` (a tab BEFORE it, shared by every
  #      binary);
  #   4. the name as the LAST SEGMENT OF A PATH — `go run ./cmd/mpm`,
  #      `go run ./cmd/mpm remember "..."`, `go build -o /tmp/x
  #      ./cmd/mpm`, `/home/v/.local/bin/mpm` — the rule is
  #      `*/mpm *`;
  #   5. the name as the last segment of a path with a TRAILING SLASH
  #      — `go run ./cmd/mpm/` — the rule is `*/mpm/`;
  #   6. the name inside a GO PACKAGE PATTERN — `go run ./cmd/mpm/...`,
  #      `go list ./cmd/mpm/...`, and the OTHER spellings of the same
  #      pattern (`go run ./cmd/mpm/./...`, `go run ./cmd/mpm//...`,
  #      `go run mpm/...`) — the rule is `*mpm/*...*`;
  #   7. the name inside a QUOTE — `"mpm" --version`,
  #      `'mpm' --version` and `sh -c 'mpm --version'` — the rules
  #      are `*"mpm*` and `*'mpm*`.
  #
  # Shape 4 is the one that closes the WRITE route, and its earlier
  # form was WRONG. The rule used to be `*/mpm ?*`, which was
  # MEASURED to be the wrong shape: the host expands `*` before `?`,
  # so that pattern compiles to a regex that needs a space AND one
  # more character after the name — and `go run ./cmd/mpm` and
  # `go run ./cmd/mpm/`, with nothing after the name, matched no rule
  # at all. Both RAN the binary. The end-anchored form `*/mpm *`
  # closes them, and so does `go build -o /tmp/x ./cmd/mpm`.
  #
  # Shape 6 exists for the same reason. A GO PACKAGE PATTERN puts
  # something after the name — `./cmd/mpm/...` — so the LAST-SEGMENT
  # shapes did not match it, and the write route was open through it:
  # MEASURED, `go run ./cmd/mpm/... --version` ran the CLI and
  # `go run ./cmd/mpm/... remember "..."` WROTE A MEMORY.
  #
  # The rule is `*mpm/*...*` and NOT `*/mpm/...*`, and the difference
  # is the whole point. The narrower rule fires only when the three
  # dots follow the name IMMEDIATELY, and Go accepts other spellings
  # of the same pattern: MEASURED, `go run ./cmd/mpm/./... --version`,
  # `go run ./cmd/mpm//... --version` and `go run ./cmd/mpm/././...
  # --version` all RAN the CLI, and `go run ./cmd/mpm/./... remember
  # "..."` WROTE A MEMORY. The family rule fires whenever the name is
  # a segment, then a slash, then anything, then the pattern — so
  # every spelling is closed, whether or not anything precedes the
  # name (`go run mpm/...` is closed too), and `go list ./cmd/mpm/...`
  # and `go run ./cmd/mpm/...` with it.
  #
  # Shapes 4, 5 and 6 cost READ denials, and they are paid
  # deliberately: `ls /path/to/mpm` and `ls cmd/mpm` are refused,
  # because a path whose last segment is the binary's name is the
  # shape the write route takes. The trade is the one this definition
  # makes everywhere: a read a reviewer cannot run is a nuisance, and
  # a write into the operator's live database is a disaster. A
  # SUBDIRECTORY of either is still allowed (`ls /path/to/mpm/docs`,
  # `cat cmd/mpm/main.go`), and so is the `cd` spelling
  # (`cd /path/to/mpm && git log`) — but the `cd` spelling is the
  # SAME MECHANISM as an open route, so it is not recommended.
  #
  # Shape 3 is load-bearing too, and it was MEASURED: with the tab
  # rules removed, `mpm<TAB>--version`, `env mpm<TAB>--version` and
  # `echo<TAB>mpm` all RAN the binary. The host's YAML reader expands
  # the `\t` escape into a real tab, which is what the rules carry.
  #
  # Shape 7 is load-bearing, and the measurement is unambiguous:
  # with these two rules REMOVED, `"mpm" --version`,
  # `'mpm' --version`, `sh -c 'mpm --version'` and
  # `bash -c "mpm --version"` all RAN the binary. So the quoted
  # forms are closed by these rules and by nothing else.
  #
  # The last two rules are shared by every binary because every one
  # of them starts with `mpm`.
  #
  # The cost that REMAINS is the quoted form: `grep -rn
  # "mpm_memory" .` is refused — by shape 7, which is THIS
  # definition's rule, not the host's — while the unquoted
  # `grep -rn mpm_memory .` is allowed. Quoting is the natural way
  # to write a search, so a reviewer that meets the refusal should
  # retry unquoted. The alternative — dropping shape 7 — was
  # measured, and it lets the binary run.
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
    resource: "*mpm\t*"
    effect: deny
  - action: shell
    resource: "*mpm-mcp\t*"
    effect: deny
  - action: shell
    resource: "*mpm-scheduler\t*"
    effect: deny
  - action: shell
    resource: "*mpm-critic\t*"
    effect: deny
  - action: shell
    resource: "*mpm-telemetry\t*"
    effect: deny
  - action: shell
    resource: "*mpm-update\t*"
    effect: deny
  - action: shell
    resource: "*mpm-lint\t*"
    effect: deny
  - action: shell
    resource: "*\tmpm*"
    effect: deny
  - action: shell
    resource: "*/mpm *"
    effect: deny
  - action: shell
    resource: "*/mpm/"
    effect: deny
  - action: shell
    resource: "*mpm/*...*"
    effect: deny
  - action: shell
    resource: "*/mpm-mcp *"
    effect: deny
  - action: shell
    resource: "*/mpm-mcp/"
    effect: deny
  - action: shell
    resource: "*mpm-mcp/*...*"
    effect: deny
  - action: shell
    resource: "*/mpm-scheduler *"
    effect: deny
  - action: shell
    resource: "*/mpm-scheduler/"
    effect: deny
  - action: shell
    resource: "*mpm-scheduler/*...*"
    effect: deny
  - action: shell
    resource: "*/mpm-critic *"
    effect: deny
  - action: shell
    resource: "*/mpm-critic/"
    effect: deny
  - action: shell
    resource: "*mpm-critic/*...*"
    effect: deny
  - action: shell
    resource: "*/mpm-telemetry *"
    effect: deny
  - action: shell
    resource: "*/mpm-telemetry/"
    effect: deny
  - action: shell
    resource: "*mpm-telemetry/*...*"
    effect: deny
  - action: shell
    resource: "*/mpm-update *"
    effect: deny
  - action: shell
    resource: "*/mpm-update/"
    effect: deny
  - action: shell
    resource: "*mpm-update/*...*"
    effect: deny
  - action: shell
    resource: "*/mpm-lint *"
    effect: deny
  - action: shell
    resource: "*/mpm-lint/"
    effect: deny
  - action: shell
    resource: "*mpm-lint/*...*"
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
  updater and the linter. The name is caught as its own WORD (`env
  mpm ...`, `$(echo mpm)`), separated by a TAB (`mpm<TAB>--version`),
  as the LAST SEGMENT OF A PATH (`go run ./cmd/mpm`, `go run
  ./cmd/mpm/`, `/home/v/.local/bin/mpm`), inside a GO PACKAGE PATTERN
  (`go run ./cmd/mpm/...`, `go run ./cmd/mpm/./...`), and inside a
  QUOTE (`"mpm" ...`, `sh -c 'mpm ...'`). So is `install.sh`,
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

FIVE things are NOT closed, and you must not rely on them being closed:

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
- The shell's REDIRECTION targets are not intercepted. The write
  routes into a runtime root are closed by matching the PATH, so a
  command that merely QUOTES such a path after a ">" is refused too.
  That cost is accepted deliberately: a stray file in a runtime root
  is a nuisance, and a corrupted live database is a disaster.
- The shell's matcher sees TEXT, and seven routes are NOT caught. A
  name SPLIT by a substitution is not the name: `$(echo mp)m`
  assembles it and runs the binary. A name in a VARIABLE is not the
  name either: `M=mpm; $M --version` runs the binary. A SCRIPT FILE
  runs whatever it says: `bash script.sh` never names the binary at
  all. A SHELL GLOB expands to the name before the matcher ever sees
  it: `go run ./cmd/mp?` runs the binary, and `go build -o /tmp/x
  ./cmd/mp?` produces a runnable binary under a name no rule can see.
  A BRACE is expanded the same way: `go run ./cmd/mpm{,.mcp}` runs
  the binary, and the matcher never sees the name, because the shell
  expands braces before it does.
  A pattern that names a DIRECTORY of packages does not name the
  binary at all: `go build ./cmd/...` compiles every MPM binary into
  the current directory, and the text holds no name to match. The
  BUILD is not refused — there is nothing in it to refuse — but the
  binaries it produces are refused when they are RUN (`./mpm
  --version`), and `go run` over such a pattern refuses outright,
  because the pattern matches more than one package. And the host
  does not match a `cd` segment's TEXT, so the name can
  be parked in a directory argument and the binary run without it:
  `cd cmd/mpm && go run . --version` reaches the binary, and
  `cd cmd/mpm && go build -o /tmp/x .` builds it under a name no
  rule can see. (The LITERAL path forms ARE caught: `go run
  ./cmd/mpm`, `go run ./cmd/mpm/`, `go run ./cmd/mpm/...` and
  `go run ./cmd/mpm remember "..."` are all refused — but a glob is
  not literal, so the glob spelling is the open one.) If a command
  of yours is refused for any of these reasons, assemble the text at
  runtime instead of writing it literally — and remember that the
  SANDBOX below is what makes such a route harmless.
- The permission layer reads the CONTENT of a file write as well. A
  command whose text contains the binary's name is refused, so a
  document that merely MENTIONS it cannot be written with the name
  spelled out. Assemble the name at runtime, or use the scratchpad's
  own tooling.

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
