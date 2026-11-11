package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connector"
)

// ContextAPI reads the conversation the agent is authorized to answer on.
// Pagination is explicit so large threads do not silently lose evidence.
type ContextAPI interface {
	ReadContext(context.Context, Ref, int) (Context, error)
}

// Context is one page of repository evidence, including the subject on every page.
type Context struct {
	Subject        ContextSubject   `json:"subject"`
	Comments       []ContextComment `json:"comments"`
	Reviews        []ContextComment `json:"reviews,omitempty"`
	ReviewComments []ContextComment `json:"review_comments,omitempty"`
	Files          []ContextFile    `json:"files,omitempty"`
	Page           int              `json:"page"`
	NextPage       int              `json:"next_page,omitempty"`
}

// ContextSubject describes an issue or pull request and its exact head revision.
type ContextSubject struct {
	Reactions   *ReactionCounts `json:"reactions,omitempty"`
	Number      int             `json:"number"`
	Title       string          `json:"title"`
	Body        string          `json:"body"`
	State       string          `json:"state"`
	URL         string          `json:"html_url"`
	PullRequest *struct {
		URL string `json:"url"`
	} `json:"pull_request,omitempty"`
	Head struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"head,omitempty"`
}

// ContextComment retains links and diff positions for evidence and replies.
type ContextComment struct {
	Reactions *ReactionCounts `json:"reactions,omitempty"`
	ID        int64           `json:"id"`
	Body      string          `json:"body"`
	URL       string          `json:"html_url"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
	Path        string `json:"path,omitempty"`
	Line        int    `json:"line,omitempty"`
	DiffHunk    string `json:"diff_hunk,omitempty"`
	InReplyToID int64  `json:"in_reply_to_id,omitempty"`
	State       string `json:"state,omitempty"`
}

// ReactionCounts carries native GitHub feedback into the next context read.
// Reactions inform follow-up answers; they do not authorize actions.
type ReactionCounts struct {
	Total      int `json:"total_count"`
	ThumbsUp   int `json:"+1"`
	ThumbsDown int `json:"-1"`
}

// ContextFile is a changed file. GitHub may omit Patch for binary or large files.
type ContextFile struct {
	Filename string `json:"filename"`
	Status   string `json:"status"`
	Patch    string `json:"patch,omitempty"`
	BlobURL  string `json:"blob_url"`
}

const contextPageSize = 30

// ReadContext reads issue context, plus PR reviews, diff comments and file patches.
// A next_page value means one or more collections may have further results.
func (a *HTTPAPI) ReadContext(ctx context.Context, ref Ref, page int) (Context, error) {
	var out Context
	if err := validReadRef(ref, page); err != nil {
		return out, err
	}
	out.Page = page
	base := fmt.Sprintf("/repos/%s/%s", ref.Owner, ref.Repo)
	get := func(path string, dst any) error {
		if err := a.client.Do(ctx, http.MethodGet, base+path, nil, dst); err != nil {
			return fmt.Errorf("github: read context for %s: %w", ref, err)
		}
		return nil
	}
	if err := get(fmt.Sprintf("/issues/%d", ref.Number), &out.Subject); err != nil {
		return Context{}, err
	}
	suffix := fmt.Sprintf("?per_page=%d&page=%d", contextPageSize, page)
	if err := get(fmt.Sprintf("/issues/%d/comments", ref.Number)+suffix, &out.Comments); err != nil {
		return Context{}, err
	}
	if out.Subject.PullRequest != nil {
		var pr ContextSubject
		if err := get(fmt.Sprintf("/pulls/%d", ref.Number), &pr); err != nil {
			return Context{}, err
		}
		out.Subject.Head = pr.Head
		for _, read := range []struct {
			path string
			dst  any
		}{
			{"reviews", &out.Reviews}, {"comments", &out.ReviewComments}, {"files", &out.Files},
		} {
			if err := get(fmt.Sprintf("/pulls/%d/%s", ref.Number, read.path)+suffix, read.dst); err != nil {
				return Context{}, err
			}
		}
		var after ContextSubject
		if err := get(fmt.Sprintf("/pulls/%d", ref.Number), &after); err != nil {
			return Context{}, err
		}
		if after.Head.SHA != out.Subject.Head.SHA {
			return Context{}, fmt.Errorf("github: PR head changed while reading context; retry against the new commit")
		}
	}
	if len(out.Comments) == contextPageSize || len(out.Reviews) == contextPageSize || len(out.ReviewComments) == contextPageSize || len(out.Files) == contextPageSize {
		out.NextPage = page + 1
	}
	return out, nil
}

func validReadRef(ref Ref, page int) error {
	if address.ValidWorkspace(ref.Owner) != nil || !validRepository(ref.Repo) || ref.Number <= 0 || page <= 0 {
		return fmt.Errorf("github: context needs a valid repository, positive issue number and page")
	}
	return nil
}

func validRepository(repo string) bool {
	if repo == "" || repo == "." || repo == ".." {
		return false
	}
	for _, r := range repo {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func (c *Connector) readContext(ctx context.Context, target address.Address, raw json.RawMessage) (connector.Result, error) {
	if target.Connector == Name {
		if _, err := c.ref(target); err != nil {
			return connector.Result{}, err
		}
	}
	var args struct {
		Address string `json:"address"`
		Number  int    `json:"number"`
		Page    int    `json:"page"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&args); err != nil {
		return connector.Result{}, fmt.Errorf("github: read context args: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return connector.Result{}, fmt.Errorf("github: read context needs one arguments object")
	}
	if args.Number < 0 || args.Page < 0 {
		return connector.Result{}, fmt.Errorf("github: number and page must not be negative")
	}
	if args.Page == 0 {
		args.Page = 1
	}
	target, err := c.readTarget(target, args.Address)
	if err != nil {
		return connector.Result{}, err
	}
	ref, kind, err := c.resolve(target, args.Number)
	if err != nil {
		return connector.Result{}, err
	}
	api, ok := c.cfg.API.(ContextAPI)
	if !ok {
		return connector.Result{}, fmt.Errorf("%w: github context reads", connector.ErrUnsupported)
	}
	result, err := api.ReadContext(ctx, ref, args.Page)
	if err != nil {
		return connector.Result{}, err
	}
	if pinned := target.Params["head_sha"]; pinned != "" && ref.Number == mustTargetNumber(target) && result.Subject.Head.SHA != pinned {
		return connector.Result{}, fmt.Errorf("github: PR head changed from pinned %s to %s; request a review of the new commit", pinned, result.Subject.Head.SHA)
	}
	data, err := json.Marshal(result)
	if err != nil {
		return connector.Result{}, fmt.Errorf("github: encode context: %w", err)
	}
	// Summary is the tool result the model receives, not just the audit metadata.
	return connector.Result{Summary: string(data), Data: data, Address: refAddress(ref, kind)}, nil
}

func mustTargetNumber(target address.Address) int {
	// readContext has already validated the address through ref.
	var number int
	_, _ = fmt.Sscan(target.Path[2], &number)
	return number
}
