package evidence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The GitHub READER.
//
// # WHY THERE IS NO CLIENT LIBRARY
//
// The module vendors no GitHub client, and PR 5 does not add one. A
// credential framework is out of scope, and a client library is a
// dependency this codebase would then carry forever.
//
// So this is a small HTTP reader over the REST API: three GETs. It uses
// NO credentials for a public repository, which is what lets the read-only
// `check` path work for a fresh reviewer who has nothing but a URL.
//
// # THE ERRORS ARE THE POINT
//
// A reader that cannot reach GitHub must say so, and it must say it
// DIFFERENTLY from "the thing is not there". The two are different facts
// and a reader that confuses them will report a missing pull request as a
// verified absence, or a verified absence as a network problem.

var (
	// ErrNotFound says the resource does not exist.
	ErrNotFound = errors.New("evidence: the GitHub resource does not exist")

	// ErrUnauthorized says GitHub refused the request.
	ErrUnauthorized = errors.New("evidence: GitHub refused the request")

	// ErrUnavailable says GitHub could not be reached.
	ErrUnavailable = errors.New("evidence: GitHub could not be reached")
)

// isMissing reports whether an error is a not-found.
func isMissing(err error) bool { return errors.Is(err, ErrNotFound) }

// Client reads GitHub over the REST API.
//
// It is safe for concurrent use: it holds no mutable state.
type Client struct {
	// BaseURL is the API's root. It is a field so that a test can point
	// the reader at a local server.
	BaseURL string
	// HTTP is the transport.
	HTTP *http.Client
	// Token is an OPTIONAL read credential.
	//
	// It is never written to a log, never included in an error, and
	// never sent anywhere but the API's own host.
	Token string
}

// DefaultBaseURL is the public GitHub API.
const DefaultBaseURL = "https://api.github.com"

// NewClient returns a reader for one repository.
//
// The token is optional: a PUBLIC repository is readable without one.
func NewClient(token string) *Client {
	return &Client{
		BaseURL: DefaultBaseURL,
		HTTP: &http.Client{
			Timeout: 20 * time.Second,
		},
		Token: token,
	}
}

// PullRequest reads one pull request's facts.
func (c *Client) PullRequest(ctx context.Context, repo string, number int) (*GitHubFacts, error) {
	if repo == "" {
		return nil, fmt.Errorf("%w: no repository was named", ErrUnavailable)
	}
	body, err := c.get(ctx, fmt.Sprintf("repos/%s/pulls/%d", repo, number), nil)
	if err != nil {
		return nil, err
	}
	var pr struct {
		Number         int    `json:"number"`
		State          string `json:"state"`
		Draft          bool   `json:"draft"`
		Title          string `json:"title"`
		HTMLURL        string `json:"html_url"`
		Merged         bool   `json:"merged"`
		MergedAt       string `json:"merged_at"`
		MergeCommitSHA string `json:"merge_commit_sha"`
		Head           struct {
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			SHA string `json:"sha"`
		} `json:"base"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := json.Unmarshal(body, &pr); err != nil {
		return nil, fmt.Errorf("evidence: decode the pull request: %w", err)
	}
	facts := &GitHubFacts{
		Repository:  repo,
		Number:      pr.Number,
		State:       pr.State,
		IsDraft:     pr.Draft,
		Title:       pr.Title,
		HeadSHA:     pr.Head.SHA,
		BaseSHA:     pr.Base.SHA,
		MergeCommit: pr.MergeCommitSHA,
		MergedAt:    pr.MergedAt,
		URL:         pr.HTMLURL,
	}
	if pr.Merged || pr.MergedAt != "" {
		facts.State = "merged"
	}
	// THE REVIEWS are GitHub's OWN. They are not the host-captured
	// findings of a contained reviewer, and the record keeps them
	// apart.
	reviews, err := c.pullRequestReviews(ctx, repo, number)
	if err == nil {
		facts.Reviews = reviews
	}
	return facts, nil
}

// pullRequestReviews reads a pull request's GitHub-native reviews.
//
// A failure here is not fatal: the reviews are one field of the facts,
// and the facts are still worth having without them.
func (c *Client) pullRequestReviews(ctx context.Context, repo string, number int) ([]GitHubReview, error) {
	body, err := c.get(ctx, fmt.Sprintf("repos/%s/pulls/%d/reviews", repo, number), nil)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		State    string `json:"state"`
		Body     string `json:"body"`
		CommitID string `json:"commit_id"`
		User     struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("evidence: decode the reviews: %w", err)
	}
	out := make([]GitHubReview, 0, len(raw))
	for _, r := range raw {
		out = append(out, GitHubReview{
			Author: r.User.Login,
			State:  r.State,
			Body:   r.Body,
			SHA:    r.CommitID,
		})
	}
	return out, nil
}

// WorkflowRuns reads the workflow runs for a commit.
//
// The runs come back with their JOBS, because a run's own conclusion and
// its jobs' conclusions are different claims, and the evidence contract
// keeps them apart.
func (c *Client) WorkflowRuns(ctx context.Context, repo string, headSHA string) ([]CIRun, error) {
	if repo == "" {
		return nil, fmt.Errorf("%w: no repository was named", ErrUnavailable)
	}
	q := url.Values{}
	if headSHA != "" {
		q.Set("head_sha", headSHA)
	}
	body, err := c.get(ctx, fmt.Sprintf("repos/%s/actions/runs", repo), q)
	if err != nil {
		return nil, err
	}
	var raw struct {
		WorkflowRuns []struct {
			ID         int64  `json:"id"`
			Name       string `json:"name"`
			HeadSHA    string `json:"head_sha"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			HTMLURL    string `json:"html_url"`
		} `json:"workflow_runs"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("evidence: decode the workflow runs: %w", err)
	}
	out := make([]CIRun, 0, len(raw.WorkflowRuns))
	for _, run := range raw.WorkflowRuns {
		r := CIRun{
			ID:         run.ID,
			Name:       run.Name,
			HeadSHA:    run.HeadSHA,
			Status:     run.Status,
			Conclusion: run.Conclusion,
			URL:        run.HTMLURL,
		}
		jobs, err := c.runJobs(ctx, repo, run.ID)
		if err == nil {
			r.Jobs = jobs
		}
		out = append(out, r)
	}
	return out, nil
}

// runJobs reads one run's jobs.
func (c *Client) runJobs(ctx context.Context, repo string, runID int64) ([]CIJob, error) {
	body, err := c.get(ctx, fmt.Sprintf("repos/%s/actions/runs/%d/jobs", repo, runID), nil)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Jobs []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			HTMLURL    string `json:"html_url"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("evidence: decode the jobs: %w", err)
	}
	out := make([]CIJob, 0, len(raw.Jobs))
	for _, j := range raw.Jobs {
		out = append(out, CIJob{
			Name:       j.Name,
			Status:     j.Status,
			Conclusion: j.Conclusion,
			URL:        j.HTMLURL,
		})
	}
	return out, nil
}

// Content reads a file from a ref.
//
// It asks for the RAW representation, so the bytes are the bytes and not
// a JSON envelope around them.
func (c *Client) Content(ctx context.Context, repo string, ref string, path string) ([]byte, error) {
	if repo == "" {
		return nil, fmt.Errorf("%w: no repository was named", ErrUnavailable)
	}
	q := url.Values{}
	if ref != "" {
		q.Set("ref", ref)
	}
	return c.get(ctx, fmt.Sprintf("repos/%s/contents/%s", repo, path), q)
}

// get performs one GET and maps the failure modes.
func (c *Client) get(ctx context.Context, path string, q url.Values) ([]byte, error) {
	target := strings.TrimRight(c.base(), "/") + "/" + path
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build the request: %v", ErrUnavailable, err)
	}
	req.Header.Set("Accept", "application/vnd.github.raw+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.Token != "" {
		// The token goes in the header and nowhere else. It is never
		// logged, never put in an error message, and never part of a
		// URL that could be printed.
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, redactURLError(err))
	}
	defer resp.Body.Close()
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("%w: %s", ErrUnauthorized, path)
	case resp.StatusCode >= 300:
		return nil, fmt.Errorf("%w: %s answered %s", ErrUnavailable, path, resp.Status)
	}
	if rerr != nil {
		return nil, fmt.Errorf("%w: read the response: %v", ErrUnavailable, rerr)
	}
	return body, nil
}

func (c *Client) base() string {
	if c.BaseURL == "" {
		return DefaultBaseURL
	}
	return c.BaseURL
}

// redactURLError removes anything that could carry a credential from an
// error.
//
// The transport's errors embed the URL, and a URL can carry a token in
// its query. An error is printed to the operator, so it is scrubbed.
func redactURLError(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, "?"); i >= 0 {
		if j := strings.Index(msg[i:], " "); j > 0 {
			msg = msg[:i] + "?" + "[redacted]" + msg[i+j:]
		}
	}
	return msg
}

// ParseRepository splits "owner/name" and refuses anything else.
//
// A repository that is not "owner/name" is not a repository, and
// publishing to a path that is not one is how a publication ends up
// somewhere it should not be.
func ParseRepository(repo string) (string, string, error) {
	repo = strings.TrimSpace(repo)
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("evidence: the repository %q is not in the form owner/name", repo)
	}
	return parts[0], parts[1], nil
}

// PRNumberFromURL is the number in a pull request URL.
//
// It is the same rule as PRNumber, kept separate because a URL is
// matched and a reference is parsed.
func PRNumberFromURL(raw string) (int, bool) {
	m := pullURLPattern.FindStringSubmatch(raw)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}
