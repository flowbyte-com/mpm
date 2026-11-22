package evidence

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// The PUBLISHER.
//
// Publication is a CONTROLLED evidence operation, not another way for an
// agent to gain privileges.
//
// # WHAT THE PUBLISHER MAY DO
//
// It may read the assignment store and it may read VERIFIED GitHub
// metadata. It may then write the result to the location the OPERATOR
// configured.
//
// # WHAT IT MAY NOT DO
//
// It may not be pointed at an arbitrary repository or an arbitrary path
// by a model-facing payload: the target comes from the operator's
// configuration file, and a payload that names a different one is
// ignored. It may not publish a review the service did not record, and it
// may not publish an approval nobody gave.
//
// # THE WRITE CREDENTIALS
//
// The writes go through the operator's own `gh`, so the credential never
// enters this process, never enters a record, and never reaches a
// contained worker or an isolated reviewer. The read path needs no
// credential at all for a public repository.

// Target is the CONFIGURED publication authority.
//
// It names where the evidence goes. It is read from the operator's
// configuration, never from a payload.
type Target struct {
	// Repository is "owner/name".
	Repository string `json:"repository"`
	// Branch is the branch the evidence lives on.
	Branch string `json:"branch"`
}

// DefaultTargetFile is the configuration file's name inside the MPM
// directory.
const DefaultTargetFile = "evidence.json"

// LoadTarget reads the operator's publication configuration.
//
// It REFUSES when the file is missing, unreadable, or does not name a
// repository and a branch. A publisher that invented a target would
// publish the evidence somewhere the operator did not choose, and the
// whole point of a configured authority is that the operator chose.
func LoadTarget(path string) (*Target, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf(
				"evidence: there is no publication configuration at %s; "+
					"publication needs an explicitly configured target", path)
		}
		return nil, fmt.Errorf("evidence: read the publication configuration: %w", err)
	}
	var t Target
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("evidence: the publication configuration is not valid: %w", err)
	}
	if _, _, err := ParseRepository(t.Repository); err != nil {
		return nil, fmt.Errorf("evidence: the publication configuration: %w", err)
	}
	if strings.TrimSpace(t.Branch) == "" {
		return nil, fmt.Errorf("evidence: the publication configuration names no branch")
	}
	return &t, nil
}

// Writer stages the files of a publication and publishes them.
//
// The interface is at the FILE level rather than the "publish this tree"
// level, because a publication that is interrupted half-way must be
// retryable, and that can only be guaranteed if the stages are visible.
type Writer interface {
	// PutFile stages one file.
	PutFile(ctx context.Context, path string, content []byte) error
	// Commit publishes the staged files and returns the commit id.
	Commit(ctx context.Context, message string) (string, error)
	// Abort discards the staged files.
	Abort(ctx context.Context) error
}

// Publication is the outcome of a publication.
type Publication struct {
	// SnapshotID identifies the published set of records.
	SnapshotID string `json:"snapshot_id"`
	// CommitSHA is the commit that published them. It is empty for a
	// dry run, because nothing was published.
	CommitSHA string `json:"commit_sha,omitempty"`
	// Files are the published paths, in publication order.
	Files []string `json:"files"`
	// Records is how many records were published.
	Records int `json:"records"`
	// DryRun says the publication was not written.
	DryRun bool `json:"dry_run"`
}

// Publisher writes a plan.
type Publisher struct {
	// Writer stages and commits.
	Writer Writer
	// DryRun builds the publication and stops.
	//
	// The plan is the SAME plan the real publication would write, so a
	// dry run is a faithful rehearsal rather than a second rendering.
	DryRun bool
}

// Publish writes the plan.
//
// The order is the plan's order: the immutable records first and the
// mutable pointer last, and the commit at the end. An interruption
// before the commit leaves the branch's pointer naming the PREVIOUS
// snapshot, which is true — never a corpus that is half written.
func (p *Publisher) Publish(ctx context.Context, plan *Plan) (*Publication, error) {
	if plan == nil {
		return nil, fmt.Errorf("evidence: there is no plan to publish")
	}
	out := &Publication{
		SnapshotID: plan.SnapshotID,
		Records:    len(plan.Records),
		DryRun:     p.DryRun,
	}
	for _, f := range plan.Files {
		out.Files = append(out.Files, f.Path)
	}
	if p.DryRun {
		return out, nil
	}
	if p.Writer == nil {
		return nil, fmt.Errorf("evidence: there is no writer to publish through")
	}
	staged := 0
	for _, f := range plan.Files {
		if err := p.Writer.PutFile(ctx, f.Path, f.Content); err != nil {
			_ = p.Writer.Abort(ctx)
			return nil, fmt.Errorf("evidence: stage %s: %w", f.Path, err)
		}
		staged++
	}
	message := fmt.Sprintf("evidence: publish snapshot %s", plan.SnapshotID)
	sha, err := p.Writer.Commit(ctx, message)
	if err != nil {
		_ = p.Writer.Abort(ctx)
		return nil, fmt.Errorf("evidence: publish the snapshot: %w", err)
	}
	out.CommitSHA = sha
	return out, nil
}

// The `gh`-backed writer.
//
// It uses the Git Data API: blobs, then a tree, then a commit, then the
// ref. The commit is the ATOMIC step, so the evidence branch's pointer
// moves to the new snapshot only when the whole corpus is in place.
type GHWriter struct {
	// Owner is the repository's owner.
	Owner string
	// Name is the repository's name.
	Name string
	// Branch is the branch the evidence lives on.
	Branch string
	// Binary is the `gh` executable. It is a field so a test can point
	// it at a script.
	Binary string
	// blobs are the staged files.
	blobs []blobEntry
}

// NewGHWriter returns a writer for one repository and branch.
func NewGHWriter(repository, branch string) (*GHWriter, error) {
	owner, name, err := ParseRepository(repository)
	if err != nil {
		return nil, err
	}
	return &GHWriter{Owner: owner, Name: name, Branch: branch, Binary: "gh"}, nil
}

// PutFile stages one file as a blob.
func (w *GHWriter) PutFile(ctx context.Context, path string, content []byte) error {
	payload := map[string]string{
		"content":  base64.StdEncoding.EncodeToString(content),
		"encoding": "base64",
	}
	var out struct {
		SHA string `json:"sha"`
	}
	if err := w.api(ctx, "POST", fmt.Sprintf("repos/%s/%s/git/blobs", w.Owner, w.Name),
		payload, &out); err != nil {
		return err
	}
	w.blobs = append(w.blobs, blobEntry{path: path, sha: out.SHA})
	return nil
}

// blobEntry is one staged file.
type blobEntry struct {
	path string
	sha  string
}

// Commit publishes the staged files.
//
// The tree is built on the branch's CURRENT tree, so a file the
// publisher does not know about is not deleted by a publication.
func (w *GHWriter) Commit(ctx context.Context, message string) (string, error) {
	baseTree, parent, err := w.currentTree(ctx)
	if err != nil {
		return "", err
	}
	tree := map[string]interface{}{}
	entries := make([]map[string]string, 0, len(w.blobs))
	for _, b := range w.blobs {
		entries = append(entries, map[string]string{
			"path": b.path,
			"mode": "100644",
			"type": "blob",
			"sha":  b.sha,
		})
	}
	tree["tree"] = entries
	if baseTree != "" {
		tree["base_tree"] = baseTree
	}
	var treeOut struct {
		SHA string `json:"sha"`
	}
	if err := w.api(ctx, "POST", fmt.Sprintf("repos/%s/%s/git/trees", w.Owner, w.Name),
		tree, &treeOut); err != nil {
		return "", err
	}
	commit := map[string]interface{}{
		"message": message,
		"tree":    treeOut.SHA,
	}
	if parent != "" {
		commit["parents"] = []string{parent}
	}
	var commitOut struct {
		SHA string `json:"sha"`
	}
	if err := w.api(ctx, "POST", fmt.Sprintf("repos/%s/%s/git/commits", w.Owner, w.Name),
		commit, &commitOut); err != nil {
		return "", err
	}
	if parent == "" {
		ref := map[string]string{
			"ref": "refs/heads/" + w.Branch,
			"sha": commitOut.SHA,
		}
		if err := w.api(ctx, "POST", fmt.Sprintf("repos/%s/%s/git/refs", w.Owner, w.Name),
			ref, nil); err != nil {
			return "", err
		}
	} else {
		ref := map[string]string{"sha": commitOut.SHA}
		if err := w.api(ctx, "PATCH",
			fmt.Sprintf("repos/%s/%s/git/refs/heads/%s", w.Owner, w.Name, w.Branch),
			ref, nil); err != nil {
			return "", err
		}
	}
	return commitOut.SHA, nil
}

// Abort discards the staged files.
//
// Nothing was written to the branch — the commit is the step that
// publishes — so an abort only forgets what was staged.
func (w *GHWriter) Abort(ctx context.Context) error {
	w.blobs = nil
	return nil
}

// currentTree reads the evidence branch's current tree and commit.
//
// A branch that does not exist yet has neither, and the publication
// creates it.
func (w *GHWriter) currentTree(ctx context.Context) (tree string, parent string, err error) {
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	path := fmt.Sprintf("repos/%s/%s/git/ref/heads/%s", w.Owner, w.Name, w.Branch)
	if err := w.api(ctx, "GET", path, nil, &ref); err != nil {
		if isNotFound(err) {
			return "", "", nil
		}
		return "", "", err
	}
	parent = ref.Object.SHA
	var commit struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err := w.api(ctx, "GET",
		fmt.Sprintf("repos/%s/%s/git/commits/%s", w.Owner, w.Name, parent), nil, &commit); err != nil {
		return "", "", err
	}
	return commit.Tree.SHA, parent, nil
}

// api performs one `gh api` call.
//
// The credential stays in `gh`: this process never sees a token, and the
// error text is scrubbed before it is printed.
func (w *GHWriter) api(ctx context.Context, method, path string, input interface{}, out interface{}) error {
	binary := w.Binary
	if binary == "" {
		binary = "gh"
	}
	args := []string{"api", "--method", method, path}
	var stdin []byte
	if input != nil {
		raw, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("evidence: encode the request: %w", err)
		}
		stdin = raw
		args = append(args, "--input", "-")
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return &ghError{message: scrub(stderr.String()), err: err}
	}
	if out == nil || stdout.Len() == 0 {
		return nil
	}
	if err := json.Unmarshal(stdout.Bytes(), out); err != nil {
		return fmt.Errorf("evidence: decode the response: %w", err)
	}
	return nil
}

// ghError is a failed `gh api` call.
type ghError struct {
	message string
	err     error
}

func (e *ghError) Error() string {
	if e.message == "" {
		return fmt.Sprintf("evidence: the GitHub call failed: %v", e.err)
	}
	return fmt.Sprintf("evidence: the GitHub call failed: %s", e.message)
}

// isNotFound reports whether a `gh api` failure was a 404.
func isNotFound(err error) bool {
	var gh *ghError
	if !errors.As(err, &gh) {
		return false
	}
	return strings.Contains(gh.message, "404")
}

// scrub removes anything that could carry a credential from `gh`'s
// stderr.
func scrub(text string) string {
	text = strings.TrimSpace(text)
	if len(text) > 400 {
		text = text[:400] + "…"
	}
	for _, marker := range []string{"ghp_", "gho_", "ghs_", "github_pat_", "sk-"} {
		if i := strings.Index(text, marker); i >= 0 {
			text = text[:i] + "[redacted]"
			break
		}
	}
	return text
}
