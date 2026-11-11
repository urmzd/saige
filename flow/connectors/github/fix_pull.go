package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connector"
)

// FixPullAPI creates a separate reviewable change, never writing a source or
// default branch. The request pins the inspected default-branch commit.
type FixPullAPI interface {
	ReadDefaultBranch(context.Context, Ref) (RepositoryBase, error)
	CreateFixPull(context.Context, Ref, string, FixPullRequest) (FixPullResult, error)
}
type RepositoryBase struct {
	Name string `json:"name"`
	SHA  string `json:"sha"`
}
type FixPullRequest struct {
	Base       RepositoryBase `json:"base"`
	Title      string         `json:"title"`
	Body       string         `json:"body"`
	Changes    ChangeRequest  `json:"changes"`
	SourceKind string         `json:"-"`
	Context    FixPullContext `json:"-"`
}

// FixPullContext contains host-verified links, never model arguments.
type FixPullContext struct {
	SessionURL string
	AlertURL   string
}
type FixPullResult struct {
	Number    int    `json:"number"`
	URL       string `json:"html_url"`
	Branch    string `json:"branch"`
	SHA       string `json:"sha"`
	Recovered bool   `json:"recovered,omitempty"`
}

func validateFixPull(r FixPullRequest) error {
	for _, link := range []string{r.Context.SessionURL, r.Context.AlertURL} {
		if link == "" {
			continue
		}
		u, err := url.Parse(link)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || len(link) > 2048 || strings.ContainsAny(link, "<>\r\n") {
			return fmt.Errorf("github: invalid trusted fix context link")
		}
	}
	if !validSHA(r.Base.SHA) || r.Base.Name == "" || strings.ContainsAny(r.Base.Name, "\x00\r\n") {
		return fmt.Errorf("github: fix requires inspected default branch and immutable SHA")
	}
	if strings.TrimSpace(r.Title) == "" || len(r.Title) > 180 || strings.ContainsAny(r.Title, "\r\n") {
		return fmt.Errorf("github: fix title must be one line of at most 180 bytes")
	}
	if len(r.Body) > 40000 {
		return fmt.Errorf("github: fix description exceeds 40000 bytes")
	}
	if r.SourceKind != "issues" && r.SourceKind != "pull" {
		return fmt.Errorf("github: fix must link a source issue or pull request")
	}
	if err := validateChanges(r.Changes); err != nil {
		return err
	}
	for _, f := range r.Changes.Files {
		name := strings.ToLower(f.Path)
		if name == ".github/workflows" || strings.HasPrefix(name, ".github/workflows/") {
			return fmt.Errorf("github: fixes cannot edit workflow files")
		}
	}
	return nil
}
func fixIdentity(ref Ref, run string, r FixPullRequest) (branch, marker string) {
	raw, _ := json.Marshal(struct {
		Ref     Ref
		Run     string
		Request FixPullRequest
		Kind    string
		Context FixPullContext
	}{ref, run, r, r.SourceKind, r.Context})
	hash := sha256.Sum256(raw)
	id := hex.EncodeToString(hash[:])
	return "humagen/fix-" + id[:24], "<!-- humagen-fix:" + id + " -->"
}
func fixTitle(r FixPullRequest) string {
	return strings.TrimSuffix(strings.TrimSpace(r.Title), " [humagen]") + " [humagen]"
}
func fixBody(ref Ref, r FixPullRequest, marker string) string {
	link := fmt.Sprintf("https://github.com/%s/%s/%s/%d", ref.Owner, ref.Repo, r.SourceKind, ref.Number)
	relation := "Related to "
	if r.SourceKind == "issues" {
		relation = "Fixes "
	}
	context := ""
	if r.Context.SessionURL != "" {
		context += "\n\nSource session: <" + r.Context.SessionURL + ">."
	}
	if r.Context.AlertURL != "" {
		context += "\n\nSource alert: <" + r.Context.AlertURL + ">."
	}
	return strings.TrimSpace(r.Body) + "\n\n" + relation + link + "." + context + "\n\nCreated by Humagen. Tests are reported in the description; creation does not run them.\n\n" + marker
}
func (c *Connector) fixBaseAction() connector.Action {
	return connector.Action{Name: "github_fix_base", Description: "Read the default branch and immutable commit to inspect before preparing a fix PR. Read files at this SHA before proposing changes.", Schema: json.RawMessage(`{"type":"object","properties":{"address":{"type":"string"}},"additionalProperties":false}`), Invoke: func(ctx context.Context, target address.Address, raw json.RawMessage) (connector.Result, error) {
		var args struct {
			Address string `json:"address"`
		}
		if err := decodeArguments(raw, &args); err != nil {
			return connector.Result{}, err
		}
		target, err := c.readTarget(target, args.Address)
		if err != nil {
			return connector.Result{}, err
		}
		ref, err := c.ref(target)
		if err != nil {
			return connector.Result{}, err
		}
		base, err := c.cfg.API.(FixPullAPI).ReadDefaultBranch(ctx, ref)
		if err != nil {
			return connector.Result{}, err
		}
		data, _ := json.Marshal(base)
		return connector.Result{Summary: "Inspect files at " + base.Name + " (" + base.SHA + ") before preparing a fix", Data: data, Address: target.WithParam("head_sha", base.SHA)}, nil
	}}
}
func (c *Connector) fixPullAction() connector.Action {
	return connector.Action{Name: "github_create_fix_pr", Description: "Create a new fix branch and pull request linked to this issue or PR. Requires repository authorization and host approval policy. Never writes the default/source branch or workflows. Use github_fix_base and read files first.", Schema: json.RawMessage(`{"type":"object","properties":{"address":{"type":"string"},"base":{"type":"object","properties":{"name":{"type":"string"},"sha":{"type":"string"}},"required":["name","sha"],"additionalProperties":false},"title":{"type":"string"},"body":{"type":"string"},"changes":{"type":"object","properties":{"message":{"type":"string"},"files":{"type":"array","minItems":1,"maxItems":20,"items":{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"},"delete":{"type":"boolean"}},"required":["path"],"additionalProperties":false}}},"required":["message","files"],"additionalProperties":false}},"required":["base","title","body","changes"],"additionalProperties":false}`), Invoke: c.createFixPull}
}
func (c *Connector) createFixPull(ctx context.Context, target address.Address, raw json.RawMessage) (connector.Result, error) {
	if target.Connector == Name {
		if _, err := c.ref(target); err != nil {
			return connector.Result{}, err
		}
	}
	var input struct {
		Address string         `json:"address"`
		Base    RepositoryBase `json:"base"`
		Title   string         `json:"title"`
		Body    string         `json:"body"`
		Changes ChangeRequest  `json:"changes"`
	}
	if err := decodeArguments(raw, &input); err != nil {
		return connector.Result{}, err
	}
	target, err := c.actionTarget(target, input.Address)
	if err != nil {
		return connector.Result{}, err
	}
	ref, err := c.ref(target)
	if err != nil {
		return connector.Result{}, err
	}
	r := FixPullRequest{Base: input.Base, Title: input.Title, Body: input.Body, Changes: input.Changes, SourceKind: target.Path[1]}
	if err := validateFixPull(r); err != nil {
		return connector.Result{}, err
	}
	run := connector.RunID(ctx)
	if run == "" || c.cfg.AuthorizeFixPull == nil {
		return connector.Result{}, fmt.Errorf("github: fix PR requires host run authorization")
	}
	if err := c.cfg.AuthorizeFixPull(ctx, ref); err != nil {
		return connector.Result{}, err
	}
	if c.cfg.FixPullContext != nil {
		r.Context, err = c.cfg.FixPullContext(ctx, ref)
		if err != nil {
			return connector.Result{}, err
		}
		if err := validateFixPull(r); err != nil {
			return connector.Result{}, err
		}
	}
	result, err := c.cfg.API.(FixPullAPI).CreateFixPull(ctx, ref, run, r)
	if err != nil {
		return connector.Result{}, err
	}
	data, _ := json.Marshal(result)
	return connector.Result{Summary: "Created fix PR " + result.URL + " on " + result.Branch, Address: refAddress(Ref{Owner: ref.Owner, Repo: ref.Repo, Number: result.Number}, "pull"), Data: data}, nil
}
