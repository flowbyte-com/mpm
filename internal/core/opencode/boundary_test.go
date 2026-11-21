// boundary_test.go — Continuity 001, PR 3: the runtime boundary's own
// tests.
//
// # WHAT IS UNDER TEST
//
// The pieces that decide the worker's blast radius:
//
//   - the CONTAINMENT argv, because its ORDER is what makes loopback exist
//     while egress does not, and because a credential passed with
//     `--setenv` would be readable through /proc/<pid>/cmdline;
//   - the SIDECAR, because it is the only process holding a credential and
//     it must be a narrow model API rather than a general proxy;
//   - the AGENT DEFINITION, because a definition that drifts from the
//     canonical template is a worker with a different surface than the one
//     that was reviewed.
//
// The OS-level containment itself (what is visible from inside the
// namespace) is a REAL-HOST check, not a unit test: it needs bubblewrap and
// a kernel, and faking it would prove nothing. It is recorded in the PR's
// evidence, and it is gated behind an explicit opt-in here.
package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestWorkerDefinitionIsPinnedToTheTemplate proves the embedded definition
// IS the reviewed one.
//
// The adapter installs the embedded copy into every sandbox. If the two
// drifted, the worker would run a surface nobody reviewed, and nothing else
// in the suite would notice.
func TestWorkerDefinitionIsPinnedToTheTemplate(t *testing.T) {
	tpl := filepath.Join("..", "..", "..",
		"agent_installation", "mpm-opencode", "templates", "agents", "mpm-worker.md")
	onDisk, err := os.ReadFile(tpl)
	if err != nil {
		t.Fatalf("read the canonical template: %v", err)
	}
	if !bytes.Equal(onDisk, WorkerAgentDefinition) {
		t.Fatalf("the embedded worker definition differs from %s; "+
			"the sandbox would run a surface nobody reviewed", tpl)
	}
}

// TestWorkerDefinitionDeniesExecution is the file-edit-only contract.
//
// It parses the frontmatter rather than trusting a comment, because the
// comment is not what the host reads.
func TestWorkerDefinitionDeniesExecution(t *testing.T) {
	body := string(WorkerAgentDefinition)
	for _, want := range []string{
		"- action: \"*\"",
		"effect: deny",
		"- action: shell",
		"- action: browser",
		"- action: webfetch",
		"- action: websearch",
		"- action: skill",
		"- action: subagent",
		"- action: execute",
		"- action: mpm_*",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the worker definition is missing %q", want)
		}
	}
	// The wildcard deny must be the FIRST agent rule. The host resolves
	// last-match-wins, so a wildcard deny at the end would be shadowed by
	// every allow above it.
	firstAction := strings.Index(body, "- action:")
	if firstAction < 0 {
		t.Fatal("the definition has no permission rules")
	}
	if !strings.Contains(body[firstAction:firstAction+40], "\"*\"") {
		t.Errorf("the first permission rule is not the wildcard deny: %q",
			body[firstAction:firstAction+40])
	}
	// And no allow for any execution action.
	for _, forbidden := range []string{"action: shell\n    resource: \"*\"\n    effect: allow"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the worker definition ALLOWS execution: %q", forbidden)
		}
	}
}

// TestContainmentArgvKeepsTheOrderingThatMakesLoopbackPossible pins the
// composition the containment depends on.
//
// `bwrap --unshare-net` produces a network namespace owned by the PARENT
// user namespace, and `ip link set lo up` inside it fails with EPERM — so
// there is no address for the relay and therefore no inference. The outer
// `unshare --user --map-root-user --net` is what makes the namespace a
// child of the NEW user namespace, whose owner CAN bring loopback up.
func TestContainmentArgvKeepsTheOrderingThatMakesLoopbackPossible(t *testing.T) {
	root := t.TempDir()
	spec := Spec{
		Root:          filepath.Join(root, "sandbox"),
		Workspace:     filepath.Join(root, "work"),
		OpenCodeBin:   "/bin/true",
		MPMBin:        "/bin/true",
		SidecarSocket: filepath.Join(root, "sidecar.sock"),
		Env:           map[string]string{"HOME": PrefixHome},
		RelayCommand:  []string{"/opt/mpm/mpm"},
		Command:       []string{"/opt/opencode/opencode", "--standalone"},
	}
	if err := BuildTree(spec); err != nil {
		t.Fatalf("build the tree: %v", err)
	}
	argv, err := spec.Argv()
	if err != nil {
		t.Fatalf("argv: %v", err)
	}

	joined := strings.Join(argv, " ")
	if argv[0] != "unshare" {
		t.Errorf("argv[0] = %q, want the outer unshare", argv[0])
	}
	for _, want := range []string{"--user", "--map-root-user", "--net"} {
		if !contains(argv, want) {
			t.Errorf("the outer launcher is missing %q", want)
		}
	}
	for _, want := range []string{"--disable-userns", "--assert-userns-disabled", "--unshare-pid"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the inner bubblewrap argv is missing %q", want)
		}
	}
	// `--unshare-net` must NOT be on bubblewrap: it would create the
	// netns in the wrong user namespace.
	if strings.Contains(joined, "--unshare-net") {
		t.Error("bubblewrap is asked to unshare the network; the outer unshare must own it")
	}
	// The script must bring loopback up before exec'ing bubblewrap.
	if !strings.Contains(joined, "ip link set lo up") {
		t.Error("the launcher does not bring loopback up")
	}
	// The sidecar socket must be bound, and the workspace must be the
	// runtime's working directory.
	if !strings.Contains(joined, PrefixSocket) {
		t.Error("the sidecar socket is not bound into the namespace")
	}
	// The inner argv is shell-quoted, so the chdir pair arrives as two
	// quoted words.
	if !strings.Contains(joined, "'--chdir' '"+PrefixWorker+"'") {
		t.Errorf("the runtime is not started in the authorised worktree: %s", joined)
	}
}

// TestContainmentRefusesARootInsideTheWorkspace pins the one structural
// refusal in the builder.
func TestContainmentRefusesARootInsideTheWorkspace(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "work")
	if err := os.MkdirAll(ws, 0o700); err != nil {
		t.Fatal(err)
	}
	err := BuildTree(Spec{Root: filepath.Join(ws, "sandbox"), Workspace: ws})
	if err == nil {
		t.Fatal("a sandbox root inside the workspace was accepted")
	}
	if !strings.Contains(err.Error(), "inside the workspace") {
		t.Errorf("refused, but not by the containment rule: %v", err)
	}
}

// TestContainmentNeverCarriesACredentialInArgv is the measured-leak
// regression.
//
// `--setenv NAME VALUE` puts the value in ARGV. So the containment must
// never be handed a credential to set: the only secret the runtime needs is
// the sidecar's socket PATH, and the credential stays in the sidecar's
// process.
func TestContainmentNeverCarriesACredentialInArgv(t *testing.T) {
	root := t.TempDir()
	spec := Spec{
		Root:          filepath.Join(root, "s"),
		Workspace:     filepath.Join(root, "w"),
		OpenCodeBin:   "/bin/true",
		SidecarSocket: filepath.Join(root, "sidecar.sock"),
		Env:           map[string]string{"HOME": PrefixHome, RelayEnvVar: PrefixSocket},
		Command:       []string{"/opt/opencode/opencode"},
	}
	if err := BuildTree(spec); err != nil {
		t.Fatal(err)
	}
	argv, err := spec.Argv()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range argv {
		low := strings.ToLower(a)
		for _, forbidden := range []string{"sk-", "api_key", "apikey", "bearer "} {
			if strings.Contains(low, forbidden) {
				t.Errorf("the containment argv carries a credential-shaped value %q in %q", forbidden, a)
			}
		}
	}
}

// TestRelayModeFromEnv pins the switch.
func TestRelayModeFromEnv(t *testing.T) {
	t.Setenv(RelayEnvVar, "")
	if _, ok := RelayModeFromEnv(); ok {
		t.Error("the relay mode is active with no socket set")
	}
	t.Setenv(RelayEnvVar, "/run/sidecar.sock")
	got, ok := RelayModeFromEnv()
	if !ok || got != "/run/sidecar.sock" {
		t.Errorf("relay mode = (%q,%v), want (/run/sidecar.sock,true)", got, ok)
	}
}

// ── the sidecar ───────────────────────────────────────────────────────────

// fakeUpstream records what the sidecar forwarded and answers with a
// stream-shaped body.
type fakeUpstream struct {
	srv        *httptest.Server
	gotAuth    string
	gotBody    []byte
	gotPath    string
	statusCode int
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	u := &fakeUpstream{statusCode: 200}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.gotAuth = r.Header.Get("authorization")
		u.gotPath = r.URL.Path
		u.gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(u.statusCode)
		_, _ = io.WriteString(w, "data: {\"ok\":true}\n\n")
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func newTestSidecar(t *testing.T, upstream string, audit *bytes.Buffer) *Sidecar {
	t.Helper()
	dir := t.TempDir()
	cfg := SidecarConfig{
		SocketPath:      filepath.Join(dir, "sidecar.sock"),
		UpstreamBaseURL: upstream,
		APIKey:          "test-credential-value",
		ModelAllowList:  map[string]string{"permitted-model": "upstream-model"},
		MaxRequestBytes: 4096,
		MaxTokens:       1024,
		Timeout:         10 * time.Second,
	}
	if audit != nil {
		cfg.Audit = audit
	}
	s, err := NewSidecar(cfg)
	if err != nil {
		t.Fatalf("new sidecar: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = s.Close() })
	go func() { _ = s.Serve(ctx) }()
	return s
}

// postToSidecar sends a body over the unix socket and returns the reply.
func postToSidecar(t *testing.T, socket string, body string) (int, string) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}
	resp, err := client.Post("http://unix/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post to the sidecar: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// TestSidecarRefusesAModelOffTheAllowList is the allow-list.
func TestSidecarRefusesAModelOffTheAllowList(t *testing.T) {
	up := newFakeUpstream(t)
	s := newTestSidecar(t, up.srv.URL, nil)

	code, body := postToSidecar(t, s.SocketPath(), `{"model":"some-other-model","messages":[]}`)
	if code != http.StatusForbidden {
		t.Errorf("status %d, want %d (%s)", code, http.StatusForbidden, body)
	}
	if up.gotPath != "" {
		t.Errorf("a refused request still reached the upstream: %q", up.gotPath)
	}
	if s.Refusals() == 0 {
		t.Error("the refusal was not counted")
	}
}

// TestSidecarBoundsTheRequestSize is the size ceiling.
func TestSidecarBoundsTheRequestSize(t *testing.T) {
	up := newFakeUpstream(t)
	s := newTestSidecar(t, up.srv.URL, nil)

	huge := `{"model":"permitted-model","messages":[{"role":"user","content":"` +
		strings.Repeat("x", 8192) + `"}]}`
	code, body := postToSidecar(t, s.SocketPath(), huge)
	if code != http.StatusRequestEntityTooLarge {
		t.Errorf("status %d, want %d (%s)", code, http.StatusRequestEntityTooLarge, body)
	}
}

// TestSidecarBoundsTheCompletionLength is the token ceiling.
func TestSidecarBoundsTheCompletionLength(t *testing.T) {
	up := newFakeUpstream(t)
	s := newTestSidecar(t, up.srv.URL, nil)

	code, body := postToSidecar(t, s.SocketPath(),
		`{"model":"permitted-model","max_tokens":999999,"messages":[]}`)
	if code != http.StatusForbidden {
		t.Errorf("status %d, want %d (%s)", code, http.StatusForbidden, body)
	}
}

// TestSidecarIsNotAGeneralProxy is the architectural claim.
//
// A caller-supplied destination must not survive. The upstream the sidecar
// uses is FIXED at construction, so the worker cannot aim it anywhere else.
func TestSidecarIsNotAGeneralProxy(t *testing.T) {
	up := newFakeUpstream(t)
	other := newFakeUpstream(t)
	s := newTestSidecar(t, up.srv.URL, nil)

	code, _ := postToSidecar(t, s.SocketPath(), fmt.Sprintf(
		`{"model":"permitted-model","base_url":%q,"baseURL":%q,"messages":[]}`,
		other.srv.URL, other.srv.URL))
	if code != http.StatusOK {
		t.Fatalf("status %d, want 200", code)
	}
	if other.gotPath != "" {
		t.Error("the sidecar forwarded to a caller-supplied destination")
	}
	if up.gotPath != "/chat/completions" {
		t.Errorf("the upstream path is %q, want the fixed one", up.gotPath)
	}
	// And the substitute destination was STRIPPED from the body.
	if bytes.Contains(up.gotBody, []byte("base_url")) || bytes.Contains(up.gotBody, []byte("baseURL")) {
		t.Errorf("the caller-supplied destination survived into the upstream body: %s", up.gotBody)
	}
}

// TestSidecarAttachesTheCredential proves the credential is added by the
// sidecar and not supplied by the caller.
func TestSidecarAttachesTheCredential(t *testing.T) {
	up := newFakeUpstream(t)
	s := newTestSidecar(t, up.srv.URL, nil)

	if code, body := postToSidecar(t, s.SocketPath(),
		`{"model":"permitted-model","messages":[]}`); code != http.StatusOK {
		t.Fatalf("status %d (%s)", code, body)
	}
	if up.gotAuth != "Bearer test-credential-value" {
		t.Errorf("the upstream saw authorization %q", up.gotAuth)
	}
}

// TestSidecarAuditCarriesNoContent is the evidence rule.
func TestSidecarAuditCarriesNoContent(t *testing.T) {
	up := newFakeUpstream(t)
	var audit bytes.Buffer
	s := newTestSidecar(t, up.srv.URL, &audit)

	secretPrompt := "PROMPT_TEXT_THAT_MUST_NOT_BE_LOGGED"
	if code, body := postToSidecar(t, s.SocketPath(), fmt.Sprintf(
		`{"model":"permitted-model","messages":[{"role":"user","content":%q}]}`,
		secretPrompt)); code != http.StatusOK {
		t.Fatalf("status %d (%s)", code, body)
	}

	line := audit.String()
	if strings.Contains(line, secretPrompt) {
		t.Errorf("the audit log recorded the prompt: %s", line)
	}
	if strings.Contains(line, "test-credential-value") {
		t.Errorf("the audit log recorded the credential: %s", line)
	}
	for _, want := range []string{"outcome=ok", "model=permitted-model", "req_bytes="} {
		if !strings.Contains(line, want) {
			t.Errorf("the audit line is missing %q: %s", want, line)
		}
	}
}

// TestSidecarRefusesAGet pins the method.
func TestSidecarRefusesAGet(t *testing.T) {
	up := newFakeUpstream(t)
	s := newTestSidecar(t, up.srv.URL, nil)

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", s.SocketPath())
		},
	}}
	resp, err := client.Get("http://unix/v1/chat/completions")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
}

// TestSidecarRequiresAConfiguration pins the constructor's refusals, so a
// misconfigured sidecar is an error rather than an open door.
func TestSidecarRequiresAConfiguration(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		cfg  SidecarConfig
	}{
		{"no socket", SidecarConfig{UpstreamBaseURL: "http://x", APIKey: "k", ModelAllowList: map[string]string{"a": "b"}}},
		{"no upstream", SidecarConfig{SocketPath: filepath.Join(dir, "a.sock"), APIKey: "k", ModelAllowList: map[string]string{"a": "b"}}},
		{"no credential", SidecarConfig{SocketPath: filepath.Join(dir, "b.sock"), UpstreamBaseURL: "http://x", ModelAllowList: map[string]string{"a": "b"}}},
		{"no models", SidecarConfig{SocketPath: filepath.Join(dir, "c.sock"), UpstreamBaseURL: "http://x", APIKey: "k"}},
	}
	for _, tc := range cases {
		if _, err := NewSidecar(tc.cfg); err == nil {
			t.Errorf("%s: the sidecar was constructed", tc.name)
		}
	}
}

// TestSidecarSocketIsPrivate pins the socket's mode.
func TestSidecarSocketIsPrivate(t *testing.T) {
	up := newFakeUpstream(t)
	s := newTestSidecar(t, up.srv.URL, nil)
	fi, err := os.Stat(s.SocketPath())
	if err != nil {
		t.Fatalf("stat the socket: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("the sidecar socket is %o, want 600", perm)
	}
}

// TestSessionNotFoundIsDistinguishable is the recovery contract's hinge.
//
// The adapter's decision differs depending on WHICH failure it sees: a
// session the runtime does not know is safe to relaunch (nothing was
// created), while an unreachable runtime is ambiguous and must stop. So the
// not-found error has to be identifiable with errors.Is through whatever
// wrapping the caller adds.
func TestSessionNotFoundIsDistinguishable(t *testing.T) {
	wrapped := fmt.Errorf("inspect session %q: %w", "ses_x", ErrSessionNotFound)
	if !errors.Is(wrapped, ErrSessionNotFound) {
		t.Fatal("ErrSessionNotFound does not survive wrapping, so the adapter " +
			"cannot tell a missing session from a broken runtime")
	}
	other := fmt.Errorf("inspect session %q: %w", "ses_x", fmt.Errorf("connection refused"))
	if errors.Is(other, ErrSessionNotFound) {
		t.Fatal("an unrelated failure is reported as a missing session")
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// TestSidecarRefusesToTakeOverALiveSocket is finding 4.
//
// Unlinking the socket path unconditionally would orphan the socket a live
// sidecar is still serving, and the relay would connect to whichever inode
// it resolved. Ownership is established by CONNECTING, not by the file
// existing.
func TestSidecarRefusesToTakeOverALiveSocket(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sidecar.sock")

	// A live listener standing in for another sidecar.
	live, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	_, err = NewSidecar(SidecarConfig{
		SocketPath:      path,
		UpstreamBaseURL: "http://127.0.0.1:1",
		APIKey:          "k",
		ModelAllowList:  map[string]string{"m": "m"},
	})
	if err == nil {
		_ = live.Close()
		t.Fatal("a second sidecar took over a live socket")
	}
	if !strings.Contains(err.Error(), "already listening") {
		t.Errorf("refused, but not by the ownership rule: %v", err)
	}

	// The live listener is UNDISTURBED: its socket was not unlinked.
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("the live sidecar's socket was removed: %v", statErr)
	}
	_ = live.Close()
}

// TestSidecarReclaimsAStaleSocket is the restart half of finding 4.
//
// A crashed sidecar leaves the inode behind. Nobody is listening, so the
// next start must reclaim it — otherwise a restart after a crash would be
// impossible without manual cleanup.
func TestSidecarReclaimsAStaleSocket(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sidecar.sock")

	// Create a socket and close it, leaving a STALE inode. Go unlinks on
	// close by default, which is what a clean shutdown does; a CRASH does
	// not, so the test turns that off to reproduce the crash.
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ln.SetUnlinkOnClose(false)
	_ = ln.Close()
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("the stale socket did not survive the close: %v", statErr)
	}

	s, err := NewSidecar(SidecarConfig{
		SocketPath:      path,
		UpstreamBaseURL: "http://127.0.0.1:1",
		APIKey:          "k",
		ModelAllowList:  map[string]string{"m": "m"},
	})
	if err != nil {
		t.Fatalf("a stale socket was not reclaimed: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
}

// TestReviewerDefinitionIsPinnedToTheTemplate is the reviewer half of the
// worker's pin (finding 3c).
//
// The adapter writes the EMBEDDED definition into every review configuration.
// If the embedded copy drifted from the canonical template, a review would run
// a surface nobody reviewed — and because the definition is a permission
// boundary, nothing else in the suite would notice.
func TestReviewerDefinitionIsPinnedToTheTemplate(t *testing.T) {
	tpl := filepath.Join("..", "..", "..",
		"agent_installation", "mpm-opencode", "templates", "agents", "mpm-reviewer.md")
	onDisk, err := os.ReadFile(tpl)
	if err != nil {
		t.Fatalf("read the canonical reviewer template: %v", err)
	}
	if !bytes.Equal(onDisk, ReviewerAgentDefinition) {
		t.Fatalf("the embedded reviewer definition differs from %s", tpl)
	}
	// And it is the one the check expects: sandbox_exec allowed, shell
	// denied, mpm_assignment denied.
	b := string(ReviewerAgentDefinition)
	for _, want := range []string{"action: sandbox_exec", "action: shell", "action: mpm_assignment"} {
		if !strings.Contains(b, want) {
			t.Errorf("the reviewer definition is missing %q", want)
		}
	}
}

// TestSidecarReclaimsAStaleSocketConcurrently is finding 4's second half.
//
// Sequential reclamation is not the interesting case: two constructors racing
// on one stale path both see "connection refused", both unlink, and both bind
// — one silently taking the socket the other just created. The lifetime lock
// is what makes that impossible, and this test starts the race.
func TestSidecarReclaimsAStaleSocketConcurrently(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sidecar.sock")

	// Leave a STALE socket: a crashed sidecar does not unlink.
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ln.SetUnlinkOnClose(false)
	_ = ln.Close()

	const racers = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]*Sidecar, racers)
	errs := make([]error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // fire together
			results[i], errs[i] = NewSidecar(SidecarConfig{
				SocketPath:      path,
				UpstreamBaseURL: "http://127.0.0.1:1",
				APIKey:          "k",
				ModelAllowList:  map[string]string{"m": "m"},
			})
		}(i)
	}
	close(start)
	wg.Wait()

	// ASSERT THE REFUSAL, NOT THE RACE OUTCOME.
	//
	// Counting winners alone is a property of the SCHEDULE, not of the
	// lock: without the lock the dominant interleaving is "one racer wins
	// os.Remove, the rest get ENOENT/EADDRINUSE", which also yields exactly
	// one winner — measured at a 65-77% pass rate. The lock's actual
	// guarantee is that every loser is REFUSED BY NAME, so that is what
	// this test asserts, and it fails 100% without the lock.
	winners := 0
	refused := 0
	for i := range results {
		if errs[i] == nil {
			winners++
			t.Cleanup(func() { _ = results[i].Close() })
			continue
		}
		if strings.Contains(errs[i].Error(), "another sidecar holds") {
			refused++
		}
	}
	if winners != 1 {
		t.Fatalf("%d constructors won the stale socket, want exactly 1", winners)
	}
	if refused != racers-1 {
		t.Fatalf("only %d of %d losers were refused by the lifetime lock; the rest "+
			"failed for an incidental reason, so this test would pass without it",
			refused, racers-1)
	}
}

// TestSnapshotRevisionRefusesADirtyWorktree is finding 2's dirty half.
//
// Uncommitted changes are not a revision: a review of them cannot be described
// by an object id, so the evidence would name a commit that does not contain
// what was read.
func TestSnapshotRevisionRefusesADirtyWorktree(t *testing.T) {
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q", ".")
	run("config", "user.email", "a@b.c")
	run("config", "user.name", "a")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-qm", "initial")

	rt, err := NewRuntime(Config{
		Root: t.TempDir(), OpenCodeBin: "/bin/true", RepoPath: repo,
		Model: "m", AllowedAgents: []string{"mpm-reviewer"},
	})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}

	// CLEAN: pinned, and the snapshot IS the revision.
	rev, snapshot, err := rt.snapshotRevision()
	if err != nil {
		t.Fatalf("clean snapshot: %v", err)
	}
	if at, _ := observeRevision(snapshot); at != rev {
		t.Errorf("the snapshot is at %q, not the pinned %q", at, rev)
	}

	// THE TREE MOVES AFTER THE PIN. The snapshot must not.
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("commit", "-qam", "second")
	if at, _ := observeRevision(snapshot); at != rev {
		t.Error("the snapshot followed the moving HEAD instead of staying pinned")
	}
	if now, _ := observeRevision(repo); now == rev {
		t.Error("the repository did not actually move; the test proves nothing")
	}

	// DIRTY: refused.
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("uncommitted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := rt.snapshotRevision(); err == nil {
		t.Error("a dirty worktree was accepted for review")
	} else if !strings.Contains(err.Error(), "uncommitted") {
		t.Errorf("refused, but not as a dirty worktree: %v", err)
	}
}

// TestFindingsRequireAssistantAuthorship is finding 3.
//
// A successful session that contains only the submitted prompt, an empty
// assistant turn or a diagnostic must not count as a completed review — and
// the transcript is still recorded so a reader sees what happened.
func TestFindingsRequireAssistantAuthorship(t *testing.T) {
	cases := []struct {
		name     string
		msgs     []reviewMessage
		authored bool
	}{
		{"only the prompt", []reviewMessage{
			{Type: "user", Text: "please review this"},
		}, false},
		{"empty assistant", []reviewMessage{
			{Type: "user", Text: "please review this"},
			{Type: "assistant", Content: json.RawMessage(`[{"type":"text","text":"   "}]`)},
		}, false},
		{"diagnostics only", []reviewMessage{
			{Type: "user", Text: "please review this"},
			{Type: "idle", Error: json.RawMessage(`{"type":"unknown","message":"boom"}`)},
		}, false},
		{"real findings", []reviewMessage{
			{Type: "user", Text: "please review this"},
			{Type: "assistant", Content: json.RawMessage(`[{"type":"text","text":"F1: the fence is wrong"}]`)},
		}, true},
	}
	for _, tc := range cases {
		transcript, authored := findingsFromMessages(tc.msgs)
		if authored != tc.authored {
			t.Errorf("%s: authored = %v, want %v", tc.name, authored, tc.authored)
		}
		if transcript == "" {
			t.Errorf("%s: the transcript was not recorded", tc.name)
		}
	}
}

// TestCompletionGuardRequiresAssistantText is Fix 2.
//
// The completion guard used to be answered by the RENDERED transcript, and
// the rendering includes tool activity: "[tool sandbox_exec]" is not
// whitespace. A session where Charlie only RAN TOOLS therefore looked like a
// review.
//
// This exercises the REAL guard — `captureReviewFindings` is the function
// `Review` calls — against the whole acceptance table, and checks that the
// transcript survives a refusal, because a reader still needs to see what
// the reviewer actually did.
func TestCompletionGuardRequiresAssistantText(t *testing.T) {
	cases := []struct {
		name    string
		msgs    []reviewMessage
		accept  bool
		wantIn  string
		wantOut string
	}{
		{
			name:    "user prompt only",
			msgs:    []reviewMessage{{Type: "user", Text: "please review this"}},
			accept:  false,
			wantIn:  "[user] please review this",
			wantOut: "",
		},
		{
			name: "empty assistant message",
			msgs: []reviewMessage{
				{Type: "user", Text: "please review this"},
				{Type: "assistant", Content: json.RawMessage(`[{"type":"text","text":"   "}]`)},
			},
			accept:  false,
			wantIn:  "[user] please review this",
			wantOut: "",
		},
		{
			name: "tool activity only",
			msgs: []reviewMessage{
				{Type: "user", Text: "please review this"},
				{Type: "assistant", Content: json.RawMessage(
					`[{"type":"tool","name":"sandbox_exec","input":{"command":["ls"]}}]`)},
			},
			accept:  false,
			wantIn:  "[tool sandbox_exec]",
			wantOut: "",
		},
		{
			name: "diagnostics only",
			msgs: []reviewMessage{
				{Type: "user", Text: "please review this"},
				{Type: "idle", Error: json.RawMessage(`{"type":"unknown","message":"boom"}`)},
			},
			accept:  false,
			wantIn:  "[idle] ",
			wantOut: "",
		},
		{
			name: "tool activity plus assistant textual findings",
			msgs: []reviewMessage{
				{Type: "user", Text: "please review this"},
				{Type: "assistant", Content: json.RawMessage(
					`[{"type":"tool","name":"sandbox_exec","input":{"command":["ls"]}}]`)},
				{Type: "assistant", Content: json.RawMessage(
					`[{"type":"text","text":"F1: the fence is wrong"}]`)},
			},
			accept:  true,
			wantIn:  "[tool sandbox_exec]",
			wantOut: "F1: the fence is wrong",
		},
		{
			name: "assistant textual findings only",
			msgs: []reviewMessage{
				{Type: "user", Text: "please review this"},
				{Type: "assistant", Content: json.RawMessage(
					`[{"type":"text","text":"F1: the fence is wrong"}]`)},
			},
			accept:  true,
			wantIn:  "[user] please review this",
			wantOut: "F1: the fence is wrong",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			transcript, err := captureReviewFindings(tc.msgs)

			// THE GATE.
			if tc.accept && err != nil {
				t.Fatalf("the review was refused: %v", err)
			}
			if !tc.accept {
				if err == nil {
					t.Fatal("the review was accepted")
				}
				if !errors.Is(err, ErrReviewerUnavailable) {
					t.Errorf("refused with %v, want ErrReviewerUnavailable", err)
				}
			}

			// THE TRANSCRIPT SURVIVES BOTH WAYS, and it keeps the prompt,
			// the tool activity and the diagnostics.
			if transcript == "" {
				t.Fatal("the transcript was not recorded")
			}
			if tc.wantIn != "" && !strings.Contains(transcript, tc.wantIn) {
				t.Errorf("the transcript does not carry %q:\n%s", tc.wantIn, transcript)
			}
			if tc.wantOut == "" {
				// A REFUSAL must not carry the assistant's words — there
				// are none.
				if strings.Contains(transcript, "F1: the fence is wrong") {
					t.Errorf("a refused review carries findings:\n%s", transcript)
				}
			} else if !strings.Contains(transcript, tc.wantOut) {
				t.Errorf("the transcript does not carry %q:\n%s", tc.wantOut, transcript)
			}
		})
	}
}

// TestToolActivityCannotSatisfyTheAuthorshipGate is the sharp half of Fix 2,
// stated on its own.
//
// The rendered transcript of a tool-only assistant message is NOT empty — it
// is "[tool sandbox_exec]" — so any authorship check that reads the
// rendering would call that a review.
func TestToolActivityCannotSatisfyTheAuthorshipGate(t *testing.T) {
	toolOnly := json.RawMessage(
		`[{"type":"tool","name":"sandbox_exec","input":{"command":["ls"]}}]`)
	mixed := json.RawMessage(
		`[{"type":"tool","name":"sandbox_exec","input":{"command":["ls"]}},` +
			`{"type":"text","text":"F1: the fence is wrong"}]`)

	if got := assistantText(toolOnly); strings.TrimSpace(got) == "" {
		t.Fatal("the transcript dropped the tool activity; the record would lie")
	} else if !strings.Contains(got, "[tool sandbox_exec]") {
		t.Errorf("the tool marker is gone: %q", got)
	}

	if got := assistantAuthoredText(toolOnly); strings.TrimSpace(got) != "" {
		t.Errorf("tool activity counted as authored text: %q", got)
	}
	if got := assistantAuthoredText(mixed); !strings.Contains(got, "F1: the fence is wrong") {
		t.Errorf("the assistant's own words were dropped: %q", got)
	}
	if got := assistantAuthoredText(json.RawMessage(`[]`)); strings.TrimSpace(got) != "" {
		t.Errorf("an empty message counted as authored text: %q", got)
	}
	if got := assistantAuthoredText(nil); strings.TrimSpace(got) != "" {
		t.Errorf("a missing payload counted as authored text: %q", got)
	}
	if got := assistantAuthoredText(json.RawMessage(`"not a part list"`)); strings.TrimSpace(got) != "" {
		t.Errorf("an unparseable payload counted as authored text: %q", got)
	}
}
