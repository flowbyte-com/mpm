package buildinfo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// UpdateStatus is the classification of the installed revision against
// the upstream main branch.
type UpdateStatus string

const (
	// UpdateCurrent means the installed commit IS the upstream head.
	UpdateCurrent UpdateStatus = "current"
	// UpdateBehind means upstream main is ahead of the installed commit.
	UpdateBehind UpdateStatus = "behind"
	// UpdateAhead means the installed commit is ahead of upstream main.
	UpdateAhead UpdateStatus = "ahead"
	// UpdateDiverged means the two revisions have diverged.
	UpdateDiverged UpdateStatus = "diverged"
	// UpdateUnknown means the relationship could not be established.
	UpdateUnknown UpdateStatus = "unknown"
)

// defaultTestRepository is the upstream repository the check compares
// against unless the caller overrides it.
const defaultTestRepository = "flowbyte-com/mpm"

// defaultAPIBaseURL is the public GitHub API endpoint. The check never
// requires credentials: unauthenticated access is enough for the two
// read-only calls it makes.
const defaultAPIBaseURL = "https://api.github.com"

// defaultCheckTimeout bounds the whole check. The check is opt-in and
// interactive, so a short bound is right: a stalled network must not
// hang the operator's terminal.
const defaultCheckTimeout = 8 * time.Second

// UpdateResult is the outcome of an update check.
//
// Every field except Status is best-effort: when the status is
// UpdateUnknown the distances are meaningless and Error explains why.
type UpdateResult struct {
	Status         UpdateStatus `json:"status"`
	BehindBy       int          `json:"behind_by"`
	AheadBy        int          `json:"ahead_by"`
	UpstreamCommit string       `json:"upstream_commit"`
	CheckedAt      time.Time    `json:"checked_at"`
	Error          string       `json:"error,omitempty"`
}

// Checker performs the opt-in update check.
//
// The zero value is usable: unset fields fall back to the public GitHub
// API, the built-in repository and a bounded timeout.
type Checker struct {
	// BaseURL is the API root, e.g. https://api.github.com.
	BaseURL string

	// Repository is the upstream "owner/name".
	Repository string

	// HTTPClient carries the timeout. Tests override it.
	HTTPClient *http.Client

	// Now supplies the check timestamp. Tests override it.
	Now func() time.Time
}

// DefaultChecker returns a checker configured for the public GitHub API.
//
// MPM_UPDATE_CHECK_API_BASE redirects the check at a different API root
// (a GitHub Enterprise instance, or a local stand-in). It carries no
// user data: the only things sent are the upstream repository name and
// two source revision identifiers, both already public.
func DefaultChecker() Checker {
	base := strings.TrimSpace(os.Getenv("MPM_UPDATE_CHECK_API_BASE"))
	if base == "" {
		base = defaultAPIBaseURL
	}
	return Checker{
		BaseURL:    base,
		Repository: Repository,
		HTTPClient: &http.Client{Timeout: defaultCheckTimeout},
	}
}

// Check compares the installed commit against the upstream main branch.
//
// The distance is taken from the GitHub comparison API, which computes it
// from the repository's commit graph. It is never inferred from SHA
// strings, timestamps or version numbers — those order nothing.
//
// Every failure mode degrades to UpdateUnknown with an explanation.
func (c Checker) Check(ctx context.Context, installedCommit string) UpdateResult {
	now := time.Now().UTC()
	if c.Now != nil {
		now = c.Now().UTC()
	}

	// The check as a WHOLE is bounded, not just each request. The
	// client timeout bounds a single call; without this deadline a
	// check that makes two calls could take twice the bound, and an
	// operator waiting on a stalled network would wait for it.
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()

	res := UpdateResult{Status: UpdateUnknown, CheckedAt: now}

	if isPlaceholder(installedCommit) {
		res.Error = "this binary carries no embedded source commit, so it cannot be compared with upstream"
		return res
	}

	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = defaultAPIBaseURL
	}
	repo := c.Repository
	if repo == "" {
		repo = Repository
	}
	if repo == "" {
		repo = defaultTestRepository
	}
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultCheckTimeout}
	}

	upstream, err := c.upstreamHead(ctx, client, base, repo)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.UpstreamCommit = upstream

	comparison, err := c.compare(ctx, client, base, repo, installedCommit, upstream)
	if err != nil {
		res.Error = err.Error()
		return res
	}

	// GitHub reports the comparison from the base's point of view:
	// "ahead" means the COMPARE HEAD (upstream main) is ahead of the
	// base (the installed commit). The installed binary is therefore
	// behind by ahead_by, and ahead by behind_by.
	switch comparison.Status {
	case "identical":
		res.Status = UpdateCurrent
	case "ahead":
		res.Status = UpdateBehind
		res.BehindBy = comparison.AheadBy
	case "behind":
		res.Status = UpdateAhead
		res.AheadBy = comparison.BehindBy
	case "diverged":
		res.Status = UpdateDiverged
		res.BehindBy = comparison.AheadBy
		res.AheadBy = comparison.BehindBy
	default:
		res.Error = fmt.Sprintf("upstream returned an unrecognised comparison status %q", comparison.Status)
	}

	return res
}

// timeout returns the bound applied to the check as a whole.
//
// It follows the HTTP client's own timeout when one is set, so a caller
// that tightened the client timeout (a test, or an operator behind a
// slow proxy) gets the same bound on the whole check.
func (c Checker) timeout() time.Duration {
	if c.HTTPClient != nil && c.HTTPClient.Timeout > 0 {
		return c.HTTPClient.Timeout
	}
	return defaultCheckTimeout
}

// upstreamHead resolves the current commit of the upstream main branch.
func (c Checker) upstreamHead(ctx context.Context, client *http.Client, base, repo string) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/git/ref/heads/main", base, repo)

	body, status, err := c.get(ctx, client, url)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", describeHTTPFailure("resolve the upstream main branch", status, body)
	}

	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.Unmarshal(body, &ref); err != nil {
		return "", fmt.Errorf("could not read the upstream main branch reference: %w", err)
	}
	if ref.Object.SHA == "" {
		return "", fmt.Errorf("upstream returned no commit for the main branch")
	}
	return ref.Object.SHA, nil
}

// compare asks the upstream API for the ancestry between the installed
// commit and the upstream head.
func (c Checker) compare(ctx context.Context, client *http.Client, base, repo, installed, upstream string) (comparison, error) {
	url := fmt.Sprintf("%s/repos/%s/compare/%s...%s", base, repo, installed, upstream)

	body, status, err := c.get(ctx, client, url)
	if err != nil {
		return comparison{}, err
	}
	if status != http.StatusOK {
		if status == http.StatusNotFound {
			return comparison{}, fmt.Errorf(
				"the installed commit %s is not known to %s, so the distance cannot be established",
				shortSHA(installed), repo)
		}
		return comparison{}, describeHTTPFailure("compare the installed commit with upstream", status, body)
	}

	var out comparison
	if err := json.Unmarshal(body, &out); err != nil {
		return comparison{}, fmt.Errorf("could not read the upstream comparison: %w", err)
	}
	return out, nil
}

// comparison is the subset of the GitHub comparison payload MPM uses.
type comparison struct {
	Status   string `json:"status"`
	AheadBy  int    `json:"ahead_by"`
	BehindBy int    `json:"behind_by"`
}

// get performs one GET and returns the body and status code.
func (c Checker) get(ctx context.Context, client *http.Client, url string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("could not build the update request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "mpm/"+Version)

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("could not reach the update service: %w", err)
	}
	defer resp.Body.Close()

	body, err := readAllLimited(resp)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("could not read the update service response: %w", err)
	}
	return body, resp.StatusCode, nil
}

// describeHTTPFailure turns a non-200 response into an operator-readable
// explanation. Rate limiting is called out explicitly because it is the
// expected outcome for an unauthenticated check that has been run often.
func describeHTTPFailure(action string, status int, body []byte) error {
	switch status {
	case http.StatusForbidden, http.StatusTooManyRequests:
		return fmt.Errorf(
			"GitHub API rate limit exceeded while trying to %s (HTTP %d); try again later",
			action, status)
	case http.StatusNotFound:
		return fmt.Errorf("upstream could not %s (HTTP 404): %s", action, firstLine(body))
	default:
		return fmt.Errorf("could not %s (HTTP %d): %s", action, status, firstLine(body))
	}
}

// readAllLimited reads a response body under a hard size cap. The check
// talks to a public API; an unbounded read is an unnecessary way to be
// hurt by someone else's response.
func readAllLimited(resp *http.Response) ([]byte, error) {
	const limit = 1 << 20
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// firstLine trims a response body down to a short, single-line excerpt.
func firstLine(body []byte) string {
	s := strings.TrimSpace(string(body))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const limit = 160
	if len(s) > limit {
		s = s[:limit] + "…"
	}
	if s == "" {
		return "(empty response)"
	}
	return s
}

// shortSHA abbreviates a commit for display.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
