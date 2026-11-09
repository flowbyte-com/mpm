# Bundle validation: the candidate scope and the installed scope

> **The rule in one line.** A bundle being **assembled** is checked for
> *construction* — everything it carries must be accounted for. A bundle
> that was **already installed** is checked for *integrity* — what it
> declares must still be there, unchanged.

## Why there are two scopes

A release bundle is a directory under `~/.mpm/releases/<rev>/`. It is
**immutable by contract**: it is what a rollback gives back, and the
installer refuses to replace one that already exists.

It is **not immutable in practice**. Two things write into a release
directory after it has been installed:

| what | written by | when |
|---|---|---|
| `agent_installation/scripts/__pycache__/*.pyc` | Python, when a tool from the release imports a sibling module from the release | the first time an operator or an installer runs a tool from the installed release |
| `agent_installation/mpm-opencode/dist/mpm-build-provenance.json` | the adapter's own `install.sh`, into the canonical adapter directory | when the adapter provisions the plugin |

Neither is provisioned by the bundle's producer. Neither is named by
the bundle's declaration. Both are legitimate.

So a release directory accumulates files that a **candidate** directory
never has. A validator that applies the candidate rules to an installed
release refuses a release for having been *used* — and the only remedy
it can offer is to delete it, which is the one thing an immutable
rollback target must never be asked to be.

The two scopes answer that without weakening either:

| rule | candidate | installed |
|---|---|---|
| the five binaries are present, regular and executable | ✅ | ✅ |
| the bundle is Git-free | ✅ | ✅ |
| the bundle carries no persistent state | ✅ | ✅ |
| `RELEASE.json` is readable and names the expected revision | ✅ | ✅ |
| the binaries report the revision they claim | ✅ | ✅ |
| the runtime payload is present and loadable | ✅ | ✅ |
| the host-asset declaration is present, version 1, and names at least one asset | ✅ | ✅ |
| every declaration key is a safe bundle-relative path | ✅ | ✅ |
| every **declared** asset is present as a regular file with its declared digest | ✅ | ✅ |
| every file under `agent_installation/` is declared by something | ✅ | ❌ |
| the schema preflight (`schema/lifecycle_events.sql`) is present, non-empty and declares `lifecycle_events` | ✅ | ❌ (optional; validated if present) |

### What the installed scope still detects

The compatibility rule is narrow on purpose. An installed release is
still refused when:

- it carries **no host-asset declaration** — it was built before the
  contract existed and is genuinely incomplete. This is the very defect
  the contract exists to detect, and detecting it is not traded away
  for the convenience of an installed directory;
- a **declared** asset is missing, or its bytes no longer match the
  digest the declaration records — that is corruption;
- a declaration key is unsafe (absolute, containing `..`, carrying a
  backslash, or not in normal form);
- the schema preflight **is** present and is empty or does not declare
  `lifecycle_events`;
- any of the structural rules (binaries, Git-free, no state, identity,
  anti-mixture, payload) fails.

### What the installed scope no longer detects

An **undeclared extra file** under `agent_installation/`. That is not
incompleteness and not corruption: the declared assets are all present
and intact. The extra file is either a runtime dropping or a file
nobody provisioned — and for an installed release, the first is the
normal case.

## Where each scope is applied

| path | scope | why |
|---|---|---|
| the promotion (`internal/update/orchestrate.go`) | **candidate** | the bundle is being assembled; nothing is installed yet |
| the dry run (`mpm-update --dry-run`) | **candidate** | it reports what the promotion would install |
| re-promotion of an already-installed revision (`internal/update/activate.go`, `installRelease`) | **installed** | the release directory already exists; the only outcome available is "use the one that is there" |
| the installer's existing-release check (`install.sh`) | **installed** | the same case, on the shell side |
| **rollback** (`mpm-update --rollback`) | **none** | a rollback restores a pointer to a release that was already installed and already proven. Validating it would refuse exactly the releases a rollback exists to reach. |

The shell and the Go validators implement the same two scopes, and a
test pins that they agree:
`TestTheShellAndGoValidatorsAgreeOnTheHostAssets` in
`internal/update/hostassets_test.go`.

### A known divergence, present before this contract

The two validators do not agree about one rule: **a bundle that carries
no `bin/mpm-update`**.

- the Go validator reports it and does not refuse it — a release built
  before the standalone updater existed still validates
  (`internal/update/verify.go`, the `updater` check);
- the shell validator refuses it — a release carrying no updater is one
  whose recovery executable can never be provisioned
  (`scripts/lib/runtime_bundle.sh`, `RB_BINARIES`).

This divergence **predates the host-asset contract** and is not
introduced by it: it is the same on the head this contract was built
on. It is not a host-asset or schema-preflight rule, so it is out of
scope here; it is recorded because the compatibility matrix below
reports it, and a reader comparing the two columns deserves to know
that the difference is in the binary rule, not in the asset rule.

Measured against the live releases (disposable copies):

| release | Go candidate | Go installed | shell candidate | shell installed |
|---|---|---|---|---|
| `77738cfd…` (carries the declaration and the preflight) | refused | **valid** | refused | **valid** |
| `baadfeb98…` | refused | **valid** | refused | **valid** |
| `3d63244e…`, `aeec40f1…` (predate `mpm-update`) | refused | **valid** | refused | refused |
| `4a870c8a…` (the defective release: no declaration) | refused | refused | refused | refused |

Every "refused" in the candidate columns is the completeness rule
(undeclared runtime droppings) or the missing declaration — exactly
what the candidate scope is for. The `4a870c8a…` row is the point of
the declaration rule: that release was built by the defective builder,
so it is genuinely incomplete, and it stays refused in the installed
scope too. It remains a valid **rollback** target, because a rollback
does not validate.

## The other half of the fix: the runtime must not write into a release

The installed scope tolerates runtime droppings. It would be better if
the runtime did not produce them at all, and now it does not:

- `agent_installation/mpm-opencode/scripts/install_agents_instructions.py`
  and `agent_installation/scripts/check_installed_managed_blocks.py` set
  `sys.dont_write_bytecode = True` **before** they import their sibling
  modules. The import system compiles and caches a module the moment it
  is imported, so the flag has to be in place before the import
  statement runs.

The tolerance remains, because the droppings are already on disk in
releases that were installed before this change — and a compatibility
rule that only works for installations that happened to be clean is not
a compatibility rule.

## The schema preflight

`schema/lifecycle_events.sql` carries the substrate's own
`CREATE TABLE ... lifecycle_events` statement. The schema-collision
preflight compares the live table against it; a bundle that does not
carry it degrades that check to a **no-op**, silently, and the check
reports success because it compared the live table against nothing.

So:

- a **candidate** must carry it, and it must be non-empty and declare
  the table. `DROP TABLE lifecycle_events;` and
  `CREATE TABLE something_else (...)` do not satisfy it.
- `stageSchemaDDL` (Go), `install.sh` and `scripts/assemble-release-bundle.sh`
  all **fail closed**: an unreadable schema source or an unextractable
  DDL is an error, not "this checkout ships no DDL".
- an **installed** release may lack it — releases promoted before the
  preflight travelled with the bundle did. If it is present, it must be
  valid.

## Where the rules are implemented

| rule | Go | shell |
|---|---|---|
| the scopes | `internal/update/hostassets.go` (`BundleScope`), `internal/update/verify.go` (`ValidateBundle`, `ValidateInstalledRelease`) | `scripts/lib/runtime_bundle.sh` (`rb_validate_bundle`, `rb_validate_installed_release`) |
| the declaration keys | `internal/update/hostassets.go` (`hostAssetKeyProblem`, `pathStaysInside`) | `scripts/lib/runtime_bundle.sh` (`rb_validate_host_assets`, `key_problem`, `stays_inside`) |
| the schema preflight | `internal/update/ddl.go` (`schemaPreflightProblems`) | `scripts/lib/runtime_bundle.sh` (`rb_validate_schema_preflight`) |

The authority for the asset set is unchanged:
`scripts/install_runtime_assets.py`, whose own `load_manifest` refuses
an unsafe manifest path. The bundle validators are not weaker than the
authority they adopted.
