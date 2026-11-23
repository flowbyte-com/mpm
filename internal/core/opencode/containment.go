// containment.go — the OS-enforced boundary the worker's whole process tree
// runs inside.
//
// # WHAT THIS IS
//
// A builder for a bubblewrap mount namespace whose only visible content is
// the authorised worktree, a scratch tree and the runtime's own files. The
// runtime, its tools and every child process execute inside it.
//
// This reuses the SHAPE of agent_installation/mpm-opencode/scripts/
// reviewer_bwrap.py — the same descriptor-bound trees, the same scrubbed
// environment, the same --disable-userns / --assert-userns-disabled pair,
// and the same "--" separator that stops a command vector being parsed as
// bubblewrap options. It differs in exactly two ways, because the worker's
// job differs from Charlie's:
//
//  1. THE NETWORK NAMESPACE IS UNSHARED, and it is unshared by the OUTER
//     `unshare`, not by bubblewrap. That ordering is load-bearing:
//     bubblewrap's own --unshare-net produces a namespace owned by the
//     PARENT user namespace, so `ip link set lo up` inside it fails with
//     EPERM. `unshare --user --map-root-user --net` makes the network
//     namespace a child of the NEW user namespace, so its owner can bring
//     loopback up. Without loopback there is no address for the relay, and
//     without the relay there is no inference.
//
//     Measured consequence of the ordering: `lo` is up, egress to the
//     internet is BLOCKED, and the host's own loopback services — the live
//     OpenCode service included — are UNREACHABLE.
//
//  2. THE SIDECAR SOCKET IS BOUND IN READ-ONLY. The runtime reaches the
//     inference sidecar through a unix socket, which is a filesystem
//     object and therefore survives network-namespace separation. The
//     runtime never holds a provider credential; the sidecar does, and the
//     sidecar lives outside this namespace.
//
// # WHAT THE WORKER CANNOT SEE
//
// The operator's home, the production MPM database, the live OpenCode
// configuration, unrelated repositories, /etc/shadow, /etc/sudoers and
// /root are not mounted, so inside the namespace they do not exist. That
// is an OS fact, not a permission rule — it holds even if the agent's
// permission configuration is wrong.
//
// # NO PROVIDER SECRET IN THE LAUNCH
//
// Two measured leaks are closed here and must stay closed:
//
//   - `--clearenv` applies to the COMMAND, not to bubblewrap's own init
//     process, whose /proc/1/environ therefore exposes the CALLER's whole
//     environment. So the builder NEVER launches bubblewrap from the
//     adapter's environment: `env` is passed explicitly, and it carries
//     exactly the variables in Spec.Env.
//   - `--setenv NAME VALUE` puts the value in ARGV, readable through
//     /proc/<pid>/cmdline. So no credential is ever passed with --setenv;
//     the only secret the runtime needs is the sidecar socket's PATH, and
//     the credential stays in the sidecar.
package opencode

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SandboxTree names the directories the namespace exposes, relative to the
// sandbox root. They mirror reviewer_bwrap.py's TREE so an operator who has
// read one has read the other.
var SandboxTree = []string{"home", "tmp", "ocfg", "odata", "ocache", "ostate", "findings"}

// Namespace paths. Everything the runtime may touch lives under Prefix.
const (
	Prefix       = "/sandbox"
	PrefixWorker = Prefix + "/worker"
	PrefixHome   = Prefix + "/home"
	PrefixTmp    = Prefix + "/tmp"
	PrefixOCfg   = Prefix + "/ocfg"
	PrefixOData  = Prefix + "/odata"
	PrefixOCache = Prefix + "/ocache"
	PrefixOState = Prefix + "/ostate"
	PrefixFind   = Prefix + "/findings"
	// The sidecar socket, bound read-only.
	PrefixSocket = "/sandbox/sidecar.sock"
	// The relay's loopback address inside the namespace.
	RelayAddr = "127.0.0.1:18777"
)

// runtimeDirs are bound read-only so an executable can run. They are the
// SYSTEM's toolchain; the worker is file-edit-only, so no extra toolchain
// root is discovered or bound.
var runtimeDirs = []string{"/usr", "/usr/local"}

// RuntimeDirs returns the host directories the worker namespace binds
// read-only.
//
// They are EXPORTED so a caller that must keep a repository out of the
// worker's namespace can ask the containment itself instead of
// duplicating the list. A second copy of this list would be a copy that
// drifts.
func RuntimeDirs() []string {
	out := make([]string, len(runtimeDirs))
	copy(out, runtimeDirs)
	return out
}

// mergedSymlinks reconstruct the traditional top-level names on hosts where
// /bin, /lib and friends are symlinks into /usr.
var mergedSymlinks = map[string]string{
	"/bin":   "usr/bin",
	"/sbin":  "usr/sbin",
	"/lib":   "usr/lib",
	"/lib64": "usr/lib64",
}

// Spec is one worker execution's containment.
type Spec struct {
	// Root is the host directory holding the sandbox trees.
	Root string
	// Workspace is the authorised worktree. It is bound READ-WRITE at
	// PrefixWorker and is the runtime's working directory.
	Workspace string
	// OpenCodeBin is the runtime binary, bound read-only.
	OpenCodeBin string
	// MPMBin is the relay binary, bound read-only. It is executed with
	// RelayEnvVar set, so it runs the relay rather than the CLI.
	MPMBin string
	// SidecarSocket is the host path of the inference sidecar's socket.
	SidecarSocket string
	// SidecarBaseURL is what the RUNTIME is configured to call. It must be
	// the relay's address, never the provider.
	SidecarBaseURL string
	// Env is the COMPLETE environment the sandboxed process receives. It
	// is passed to bubblewrap's own process via exec.Cmd.Env, so
	// bubblewrap's init cannot inherit the adapter's environment, and no
	// provider credential is among these values.
	Env map[string]string
	// RelayCommand is started in the BACKGROUND inside the namespace,
	// before Command runs. It is the bridge to the sidecar socket.
	RelayCommand []string
	// Command is the command run inside the namespace, after "--".
	Command []string
}

// RelayEnvVar selects the relay mode when MPMBin is executed.
//
// It is an ENVIRONMENT switch rather than a CLI verb on purpose: the relay
// is an internal helper of this package, not an operator-facing command,
// and giving it a verb would advertise a capability nobody should invoke by
// hand.
const RelayEnvVar = "MPM_OPENCODE_RELAY_SOCKET"

// BuildTree creates the sandbox's directory tree and returns the paths.
//
// It REFUSES a root that resolves inside the workspace it is sandboxing, so
// a mistaken configuration cannot expose the worktree's parent, and it
// refuses to operate on a root that is not absolute.
func BuildTree(spec Spec) error {
	if !filepath.IsAbs(spec.Root) {
		return fmt.Errorf("opencode: the sandbox root must be absolute, got %q", spec.Root)
	}
	if !filepath.IsAbs(spec.Workspace) {
		return fmt.Errorf("opencode: the workspace must be absolute, got %q", spec.Workspace)
	}
	rootResolved, err := resolveExisting(spec.Root)
	if err != nil {
		return err
	}
	wsResolved, err := resolveExisting(spec.Workspace)
	if err != nil {
		return err
	}
	if rootResolved == wsResolved || strings.HasPrefix(rootResolved+string(os.PathSeparator), wsResolved+string(os.PathSeparator)) {
		return fmt.Errorf("opencode: the sandbox root %q is inside the workspace %q", spec.Root, spec.Workspace)
	}

	if err := os.MkdirAll(spec.Root, 0o700); err != nil {
		return fmt.Errorf("opencode: create the sandbox root: %w", err)
	}
	for _, name := range SandboxTree {
		if err := os.MkdirAll(filepath.Join(spec.Root, name), 0o700); err != nil {
			return fmt.Errorf("opencode: create the sandbox tree %q: %w", name, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(spec.Root, "ocfg", "opencode", "agents"), 0o700); err != nil {
		return fmt.Errorf("opencode: create the agent directory: %w", err)
	}
	return nil
}

// resolveExisting returns an absolute, symlink-resolved path, tolerating a
// path that does not exist yet by resolving its nearest existing ancestor.
func resolveExisting(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("opencode: resolve %q: %w", p, err)
	}
	cur := abs
	var tail []string
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return resolved, nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
}

// Argv returns the full argv that runs the spec's command inside the
// namespace, including the outer `unshare` that makes the network namespace
// a child of a new user namespace.
//
// THE ORDER IS THE SECURITY PROPERTY. `unshare` creates the user namespace
// and the network namespace; the shell inside brings loopback up and execs
// bubblewrap; bubblewrap creates the mount, PID, IPC, UTS and cgroup
// namespaces and applies the scrubbed environment.
func (spec Spec) Argv() ([]string, error) {
	if len(spec.Command) == 0 {
		return nil, fmt.Errorf("opencode: the containment has no command")
	}
	if spec.OpenCodeBin == "" {
		return nil, fmt.Errorf("opencode: the containment names no runtime binary")
	}
	for _, p := range []string{spec.Root, spec.Workspace, spec.OpenCodeBin, spec.SidecarSocket} {
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("opencode: every containment path must be absolute, got %q", p)
		}
	}

	// The inner bubblewrap argv. Everything after "--" is the COMMAND;
	// bubblewrap parses nothing after the separator as an option.
	inner := []string{"bwrap"}
	for _, p := range runtimeDirs {
		if _, err := os.Stat(p); err == nil {
			inner = append(inner, "--ro-bind", p, p)
		}
	}
	// SORTED, because a map's iteration order varies and the argv must be
	// deterministic — the same reason sortedKeys exists.
	links := make([]string, 0, len(mergedSymlinks))
	for link := range mergedSymlinks {
		links = append(links, link)
	}
	sort.Strings(links)
	for _, link := range links {
		if fi, err := os.Lstat(link); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			inner = append(inner, "--symlink", mergedSymlinks[link], link)
		}
	}
	inner = append(inner,
		"--ro-bind", spec.OpenCodeBin, "/opt/opencode/opencode",
		"--ro-bind", spec.SidecarSocket, PrefixSocket,
	)
	if spec.MPMBin != "" {
		// The relay is executed from the MPM binary itself, so the
		// worker's namespace needs no second artifact. It is bound
		// read-only and is only reachable by the launcher.
		if !filepath.IsAbs(spec.MPMBin) {
			return nil, fmt.Errorf("opencode: the MPM binary path must be absolute, got %q", spec.MPMBin)
		}
		inner = append(inner, "--ro-bind", spec.MPMBin, "/opt/mpm/mpm")
	}
	inner = append(inner,
		"--bind", spec.Workspace, PrefixWorker,
		"--bind", filepath.Join(spec.Root, "home"), PrefixHome,
		"--bind", filepath.Join(spec.Root, "ocfg"), PrefixOCfg,
		"--bind", filepath.Join(spec.Root, "odata"), PrefixOData,
		"--bind", filepath.Join(spec.Root, "ocache"), PrefixOCache,
		"--bind", filepath.Join(spec.Root, "ostate"), PrefixOState,
		"--bind", filepath.Join(spec.Root, "findings"), PrefixFind,
		// THE PID NAMESPACE MUST EXIST BEFORE /proc IS MOUNTED. Mounting
		// first exposed the HOST's /proc under a name that looked like
		// the sandbox's, so a process inside could read the caller's
		// environment through /proc/1/environ.
		"--unshare-pid", "--unshare-ipc", "--unshare-uts", "--unshare-cgroup",
		"--unshare-user",
		"--disable-userns", "--assert-userns-disabled",
		"--die-with-parent", "--new-session",
		"--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp",
		// The environment. `--clearenv` first, then exactly the values
		// the caller supplied — and the caller is the adapter, which
		// supplies no credential. Because bubblewrap's OWN process is
		// started from spec.Env (see exec.Cmd.Env in Runtime.Run), its
		// /proc/1/environ carries the same short list and not the
		// adapter's.
		"--clearenv",
	)
	for _, k := range sortedKeys(spec.Env) {
		inner = append(inner, "--setenv", k, spec.Env[k])
	}
	inner = append(inner,
		// The unshare flags are emitted before /proc above. The INNER
		// user namespace is required by --disable-userns, and it is safe
		// because the network namespace was already created by the
		// OUTER unshare.
		"--chdir", PrefixWorker,
		"--",
	)

	// THE RELAY MUST RUN INSIDE THE MOUNT NAMESPACE.
	//
	// The first version started it in the outer launcher — which runs on
	// the HOST, before bwrap has built the namespace — so the binary was
	// absent and every run failed with ECONNREFUSED against the relay's
	// address. The relay belongs after the "--", where it can see the
	// bound socket.
	//
	// bash, not sh: the readiness probe uses /dev/tcp, which dash does not
	// have. A fixed sleep would be a race; probing the port is a fact.
	innerScript := ""
	if len(spec.RelayCommand) > 0 {
		innerScript += shellQuote(spec.RelayCommand) + " &\n"
		innerScript += "i=0; while [ $i -lt 200 ]; do " +
			"(exec 3<>/dev/tcp/" + RelayAddr + ") 2>/dev/null && break; " +
			"i=$((i+1)); sleep 0.05; done\n"
	}
	innerScript += "exec " + shellQuote(spec.Command) + "\n"
	// `/usr/bin/bash`, not `/bin/bash`: /usr is ALWAYS bound, whereas /bin
	// is recreated only on merged-/usr hosts. The readiness probe below
	// needs bash's /dev/tcp, so sh will not do.
	inner = append(inner, mergedPath("/usr/bin/bash", "/bin/bash"), "-c", innerScript)

	// The outer launcher. It brings loopback up in a network namespace
	// owned by the new user namespace, starts the relay in the
	// BACKGROUND, then execs bubblewrap. `exec` matters: the shell is
	// REPLACED, so there is no extra process holding the namespace open.
	//
	// The relay is a CHILD of the shell and is reparented when the shell
	// execs bubblewrap; bubblewrap's --die-with-parent means it dies with
	// the namespace, so a finished run leaves nothing behind.
	// The launcher is built as an explicit statement list. Two details
	// are load-bearing and were both wrong in the first version:
	//
	//   1. `ip link set lo up` must be TERMINATED before the next
	//      statement. Without a separator the next word becomes an
	//      argument to `ip` ("either \"dev\" is duplicate, or \"exec\"
	//      is garbage") and the namespace never starts.
	//   2. The relay must be backgrounded as a GROUP. `A && B & C` binds
	//      the whole `A && B` into the background, so the loopback would
	//      be brought up asynchronously and race the readiness probe.
	//
	// So: the shell runs one statement at a time, and only the relay is
	// backgrounded.
	script := "ip link set lo up || exit 1\n" +
		"exec " + shellQuote(inner) + "\n"

	return []string{
		"unshare", "--user", "--map-root-user", "--net", "--",
		"/bin/sh", "-c", script,
	}, nil
}

// shellQuote renders an argv as a single-quoted shell word list.
//
// Every argument is single-quoted, and an embedded single quote becomes
// '\” — the standard idiom. The containment's own command therefore cannot
// inject shell syntax, and the "--" separator inside `inner` still stops
// the vector reaching bubblewrap's parser.
func shellQuote(argv []string) string {
	parts := make([]string, 0, len(argv))
	for _, a := range argv {
		parts = append(parts, "'"+strings.ReplaceAll(a, "'", `'\''`)+"'")
	}
	return strings.Join(parts, " ")
}

// mergedPath returns the first of the candidates that exists on the HOST.
// It is used for binaries inside the namespace, where only /usr is
// guaranteed to be bound.
func mergedPath(candidates ...string) string {
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return candidates[0]
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// A stable order keeps the argv deterministic, which is what makes a
	// test able to assert on it.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
