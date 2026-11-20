// sidecar.go — the credential holder for the contained worker.
//
// # WHAT THIS IS
//
// A SMALL, CONSTRAINED MODEL API. It is not a proxy. The worker's runtime
// is configured with a base URL pointing at a relay inside its namespace;
// that relay forwards to this sidecar's unix socket; and this sidecar is
// the ONLY process that holds a provider credential.
//
// # WHY IT EXISTS
//
// Measured on this host: a session-level permission grant overrides an
// agent-level deny, and a process inside the sandbox can read the
// environment of the process that started it. So a credential placed in
// the worker's environment is a credential the worker can reach. The only
// structure that survives that is to keep the credential in a DIFFERENT
// PROCESS outside the namespace and expose a narrow protocol.
//
// WHAT IT REFUSES
//
//   - any path other than the one chat-completions endpoint;
//   - any method other than POST;
//   - a model outside ModelAllowList;
//   - a body over MaxRequestBytes;
//   - a `max_tokens` / `max_completion_tokens` outside the configured
//     ceiling;
//   - an upstream response over MaxResponseBytes;
//   - a request that runs longer than Timeout.
//
// It NEVER forwards a caller-supplied URL, host header or `base_url`: the
// upstream is fixed at construction, so the worker cannot use the sidecar
// to reach anything but the one provider.
//
// # AUDIT
//
// Every request appends ONE bounded line to the audit sink: the outcome, the
// model, the request and response sizes, and a duration. It never records
// prompt text, completion text, headers or credentials. A refused request
// is recorded as a refusal with its reason, so "the worker tried something
// the sidecar would not do" is visible rather than silent.
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
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// SidecarConfig configures the credential holder.
type SidecarConfig struct {
	// SocketPath is the unix socket the relay connects to.
	SocketPath string
	// UpstreamBaseURL is the provider's OpenAI-compatible base, e.g.
	// "https://api.deepseek.com". It is FIXED: nothing the caller sends
	// can change it.
	UpstreamBaseURL string
	// APIKey is the provider credential. It is read from the adapter's
	// environment by the caller and never written to disk, argv or the
	// sandbox.
	APIKey string
	// ModelAllowList maps the model id the WORKER asks for onto the
	// upstream model. A caller asking for anything else is refused.
	ModelAllowList map[string]string
	// MaxRequestBytes bounds the request body.
	MaxRequestBytes int64
	// MaxResponseBytes bounds the upstream body.
	MaxResponseBytes int64
	// MaxTokens bounds a caller's completion-length request.
	MaxTokens int
	// Timeout bounds one upstream exchange.
	Timeout time.Duration
	// Audit is the sink for bounded audit lines. Nil discards them.
	Audit io.Writer
}

// Default bounds. They are deliberately small: a worker's prompt is an
// assignment objective and its references, not a corpus.
const (
	DefaultMaxRequestBytes  int64 = 4 << 20
	DefaultMaxResponseBytes int64 = 64 << 20
	DefaultMaxTokens              = 32768
	DefaultSidecarTimeout         = 10 * time.Minute
)

// Sidecar is a running credential holder.
type Sidecar struct {
	cfg      SidecarConfig
	ln       net.Listener
	srv      *http.Server
	requests atomic.Int64
	refusals atomic.Int64
}

// NewSidecar validates the configuration and binds the socket.
func NewSidecar(cfg SidecarConfig) (*Sidecar, error) {
	if cfg.SocketPath == "" {
		return nil, errors.New("opencode: the sidecar needs a socket path")
	}
	if cfg.UpstreamBaseURL == "" {
		return nil, errors.New("opencode: the sidecar needs an upstream base URL")
	}
	if cfg.APIKey == "" {
		return nil, errors.New("opencode: the sidecar has no credential")
	}
	if len(cfg.ModelAllowList) == 0 {
		return nil, errors.New("opencode: the sidecar has an empty model allow-list")
	}
	if cfg.MaxRequestBytes <= 0 {
		cfg.MaxRequestBytes = DefaultMaxRequestBytes
	}
	if cfg.MaxResponseBytes <= 0 {
		cfg.MaxResponseBytes = DefaultMaxResponseBytes
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = DefaultMaxTokens
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultSidecarTimeout
	}

	// OWNERSHIP BEFORE REMOVAL.
	//
	// The first version unlinked the path unconditionally, so a second
	// sidecar would silently orphan the socket a live first one was still
	// serving — and the worker's relay would then connect to whichever
	// inode it happened to resolve. A socket file is not evidence of
	// staleness; being able to CONNECT to it is evidence of ownership.
	//
	// So: dial it. A successful connection means somebody is listening and
	// this constructor refuses. A failed one means the path is stale (a
	// crashed sidecar leaves the inode behind), and only then is it
	// removed.
	if _, statErr := os.Stat(cfg.SocketPath); statErr == nil {
		conn, dialErr := net.DialTimeout("unix", cfg.SocketPath, 2*time.Second)
		if dialErr == nil {
			_ = conn.Close()
			return nil, fmt.Errorf(
				"opencode: another sidecar is already listening on %s; refusing to take it over",
				cfg.SocketPath)
		}
		if rmErr := os.Remove(cfg.SocketPath); rmErr != nil {
			return nil, fmt.Errorf("opencode: remove the stale sidecar socket: %w", rmErr)
		}
	}

	ln, err := net.Listen("unix", cfg.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("opencode: bind the sidecar socket: %w", err)
	}
	// Only this user may reach the socket.
	if err := os.Chmod(cfg.SocketPath, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("opencode: restrict the sidecar socket: %w", err)
	}

	s := &Sidecar{cfg: cfg, ln: ln}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	s.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 30 * time.Second,
	}
	return s, nil
}

// SocketPath is where the relay should connect.
func (s *Sidecar) SocketPath() string { return s.cfg.SocketPath }

// Serve blocks until the context ends.
func (s *Sidecar) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutdownCtx)
	}()
	err := s.srv.Serve(s.ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Close releases the socket.
func (s *Sidecar) Close() error {
	_ = s.ln.Close()
	_ = os.Remove(s.cfg.SocketPath)
	return nil
}

// Requests and Refusals are the counters a test asserts on.
func (s *Sidecar) Requests() int64 { return s.requests.Load() }
func (s *Sidecar) Refusals() int64 { return s.refusals.Load() }

// refuse writes a structured, credential-free error and records it.
func (s *Sidecar) refuse(w http.ResponseWriter, code int, reason string) {
	s.refusals.Add(1)
	s.audit("refused", reason, "", 0, 0, 0)
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"type":    "mpm_sidecar_refused",
			"message": reason,
		},
	})
}

// audit appends one bounded line. It carries no prompt, no completion and
// no header.
func (s *Sidecar) audit(outcome, reason, model string, reqBytes, respBytes int64, d time.Duration) {
	if s.cfg.Audit == nil {
		return
	}
	line := fmt.Sprintf("sidecar outcome=%s reason=%s model=%s req_bytes=%d resp_bytes=%d duration_ms=%d\n",
		outcome, sanitiseField(reason), sanitiseField(model), reqBytes, respBytes, d.Milliseconds())
	_, _ = io.WriteString(s.cfg.Audit, line)
}

// sanitiseField keeps an audit field to a token: no newlines, no quoting
// surprises, and a bounded length.
func sanitiseField(v string) string {
	v = strings.ReplaceAll(v, "\n", " ")
	v = strings.ReplaceAll(v, "\r", " ")
	v = strings.ReplaceAll(v, " ", "_")
	if len(v) > 120 {
		v = v[:120]
	}
	if v == "" {
		return "-"
	}
	return v
}

func (s *Sidecar) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	s.requests.Add(1)

	if r.Method != http.MethodPost {
		s.refuse(w, http.StatusMethodNotAllowed, "the sidecar accepts POST only")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, s.cfg.MaxRequestBytes+1))
	if err != nil {
		s.refuse(w, http.StatusBadRequest, "the request body could not be read")
		return
	}
	if int64(len(body)) > s.cfg.MaxRequestBytes {
		s.refuse(w, http.StatusRequestEntityTooLarge, "the request body exceeds the sidecar's limit")
		return
	}

	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		s.refuse(w, http.StatusBadRequest, "the request body is not a JSON object")
		return
	}

	// The model must be one the operator allowed. The
	// caller's id is translated to the upstream id; the worker never
	// learns the upstream name.
	requestedModel, _ := req["model"].(string)
	upstreamModel, ok := s.cfg.ModelAllowList[requestedModel]
	if !ok {
		s.refuse(w, http.StatusForbidden, "the requested model is not on the sidecar's allow-list")
		return
	}
	req["model"] = upstreamModel

	// The completion length is bounded. Both spellings are checked
	// because providers differ and a limit enforced on one is not a
	// limit.
	//
	// AND A CEILING IS INJECTED WHEN THE CALLER SUPPLIES NONE. Checking
	// only the supplied field would leave an omitted field unbounded —
	// which is the ordinary case, since a client that does not care about
	// length simply omits it. An omitted limit is therefore replaced with
	// the ceiling rather than trusted.
	sawLengthField := false
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		if v, present := req[key]; present {
			sawLengthField = true
			n, isNum := v.(float64)
			if !isNum || n < 0 {
				s.refuse(w, http.StatusBadRequest, "the completion length is not a number")
				return
			}
			if int(n) > s.cfg.MaxTokens {
				s.refuse(w, http.StatusForbidden, "the requested completion length exceeds the sidecar's ceiling")
				return
			}
		}
	}
	if !sawLengthField {
		req["max_tokens"] = s.cfg.MaxTokens
	}

	// No caller-supplied destination. These fields are removed rather
	// than rejected so a client that always sends one still works — but
	// removing them is what guarantees the sidecar cannot be used as a
	// general proxy.
	delete(req, "base_url")
	delete(req, "baseURL")
	delete(req, "api_base")
	delete(req, "endpoint")

	upstreamBody, err := json.Marshal(req)
	if err != nil {
		s.refuse(w, http.StatusBadRequest, "the request could not be re-encoded")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()

	url := strings.TrimSuffix(s.cfg.UpstreamBaseURL, "/") + "/chat/completions"
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(upstreamBody))
	if err != nil {
		s.refuse(w, http.StatusInternalServerError, "the sidecar could not build the upstream request")
		return
	}
	upReq.Header.Set("content-type", "application/json")
	// The credential is attached HERE, in the process that owns it.
	upReq.Header.Set("authorization", "Bearer "+s.cfg.APIKey)
	if acc := r.Header.Get("accept"); acc != "" {
		upReq.Header.Set("accept", acc)
	}

	resp, err := http.DefaultClient.Do(upReq)
	if err != nil {
		// A context deadline is the one failure worth naming precisely:
		// it is the difference between "the provider said no" and "we
		// gave up", and a caller retrying needs to know which.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			s.refuse(w, http.StatusGatewayTimeout, "the upstream request exceeded the sidecar's timeout")
			return
		}
		s.refuse(w, http.StatusBadGateway, "the upstream request failed")
		return
	}
	defer resp.Body.Close()

	// Pass the status and the content type through, and bound the body.
	for _, h := range []string{"content-type", "cache-control"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	limited := io.LimitReader(resp.Body, s.cfg.MaxResponseBytes+1)
	written, copyErr := io.Copy(w, limited)
	if copyErr != nil {
		s.audit("upstream_copy_failed", "the response could not be relayed", requestedModel, int64(len(body)), written, time.Since(start))
		return
	}
	if written > s.cfg.MaxResponseBytes {
		// The ceiling was hit mid-stream. The status line is already
		// sent, so the honest signal is a truncation record rather than
		// a second header.
		s.audit("upstream_truncated", "the response exceeded the sidecar's limit", requestedModel, int64(len(body)), written, time.Since(start))
		return
	}

	outcome := "ok"
	reason := "-"
	if resp.StatusCode >= 400 {
		outcome = "upstream_error"
		reason = fmt.Sprintf("upstream_status_%d", resp.StatusCode)
	}
	s.audit(outcome, reason, requestedModel, int64(len(body)), written, time.Since(start))
}
