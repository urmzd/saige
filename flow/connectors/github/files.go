package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connector"
)

// FileAPI reads complete source files at an immutable commit for evidence and fixes.
type FileAPI interface {
	ReadFile(context.Context, Ref, string, string) (FileContent, error)
}

// FileContent is complete text, with the exact commit and GitHub permalink.
type FileContent struct {
	Path    string `json:"path"`
	Commit  string `json:"commit"`
	Content string `json:"content"`
	URL     string `json:"url"`
}

// ReadFile reads a file through the Contents API, never following download URLs.
func (a *HTTPAPI) ReadFile(ctx context.Context, ref Ref, sha, name string) (FileContent, error) {
	if err := validReadRef(ref, 1); err != nil {
		return FileContent{}, err
	}
	if !validSHA(sha) {
		return FileContent{}, fmt.Errorf("github: file read requires a full commit SHA")
	}
	if err := validateChanges(ChangeRequest{Message: "read", Files: []FileEdit{{Path: name}}}); err != nil {
		return FileContent{}, err
	}
	segments := strings.Split(name, "/")
	for i := range segments {
		segments[i] = url.PathEscape(segments[i])
	}
	path := fmt.Sprintf("/repos/%s/%s/contents/%s?ref=%s", ref.Owner, ref.Repo, strings.Join(segments, "/"), sha)
	var result struct {
		Type     string `json:"type"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
		URL      string `json:"html_url"`
		Size     int    `json:"size"`
	}
	if err := a.client.Do(ctx, http.MethodGet, path, nil, &result); err != nil {
		return FileContent{}, fmt.Errorf("github: read file: %w", err)
	}
	if result.Type != "file" || result.Encoding != "base64" || result.Size > 256000 {
		return FileContent{}, fmt.Errorf("github: file unavailable as complete text (limit 256000 bytes)")
	}
	decoded, err := base64.StdEncoding.DecodeString(result.Content)
	if err != nil || !utf8.Valid(decoded) || strings.ContainsRune(string(decoded), 0) || len(decoded) > 256000 {
		return FileContent{}, fmt.Errorf("github: file is not supported UTF-8 text")
	}
	return FileContent{Path: name, Commit: sha, Content: string(decoded), URL: result.URL}, nil
}

func (c *Connector) fileAction() connector.Action {
	return connector.Action{Name: "github_read_file", Description: "Read a complete UTF-8 source file at a commit in this run's repository. The ref defaults to the pinned PR head. Use before proposing full-file replacements; binary or oversized files return an error.", Schema: json.RawMessage(`{"type":"object","properties":{"address":{"type":"string","description":"Optional GitHub issue/PR reference in a granted repository."},"path":{"type":"string"},"ref":{"type":"string","description":"Full immutable commit SHA. Defaults to the run's pinned head."}},"required":["path"],"additionalProperties":false}`), Invoke: c.readFile}
}

func (c *Connector) readFile(ctx context.Context, target address.Address, raw json.RawMessage) (connector.Result, error) {
	if target.Connector == Name {
		if _, err := c.ref(target); err != nil {
			return connector.Result{}, err
		}
	}
	var args struct {
		Address string `json:"address"`
		Path    string `json:"path"`
		Ref     string `json:"ref"`
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
	if args.Ref == "" {
		args.Ref = target.Params["head_sha"]
	}
	api, ok := c.cfg.API.(FileAPI)
	if !ok {
		return connector.Result{}, fmt.Errorf("%w: GitHub file reads", connector.ErrUnsupported)
	}
	file, err := api.ReadFile(ctx, ref, args.Ref, args.Path)
	if err != nil {
		return connector.Result{}, err
	}
	data, _ := json.Marshal(file)
	return connector.Result{Summary: string(data), Data: data, Address: refAddress(ref, target.Path[1]).WithParam("head_sha", args.Ref)}, nil
}

// ReadFile returns a detached file from a seeded immutable snapshot.
func (f *Fake) ReadFile(_ context.Context, ref Ref, sha, name string) (FileContent, error) {
	if err := validReadRef(ref, 1); err != nil {
		return FileContent{}, err
	}
	if err := validateChanges(ChangeRequest{Message: "read", Files: []FileEdit{{Path: name}}}); err != nil {
		return FileContent{}, err
	}
	if !validSHA(sha) {
		return FileContent{}, fmt.Errorf("github: file read requires a full commit SHA")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return FileContent{}, f.Err
	}
	content, ok := f.files[ref.String()+":"+sha][name]
	if !ok {
		return FileContent{}, fmt.Errorf("github: file not found")
	}
	if len(content) > 256000 || !utf8.ValidString(content) || strings.ContainsRune(content, 0) {
		return FileContent{}, fmt.Errorf("github: unsupported file content")
	}
	return FileContent{Path: name, Commit: sha, Content: content, URL: fmt.Sprintf("https://github.com/%s/%s/blob/%s/%s", ref.Owner, ref.Repo, sha, name)}, nil
}

var _ FileAPI = (*HTTPAPI)(nil)
var _ FileAPI = (*Fake)(nil)
