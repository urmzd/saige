package github

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

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connector"
	"github.com/urmzd/saige/flow/connectors/internal/httpjson"
)

// LatestCommentsAPI is the optional capability to read an issue's or pull
// request's latest conversation comments without reading the rest of its
// context again. HTTPAPI and Fake implement it.
type LatestCommentsAPI interface {
	// LatestComments is the last page of the issue's comments, oldest
	// first, and the page before it when the last holds fewer than want
	// comments that keep reports true for (nil keeps every comment). It
	// makes at most three requests and returns at most two pages.
	LatestComments(ctx context.Context, ref Ref, want int, keep func(ContextComment) bool) ([]ContextComment, error)
}

// ErrCommentsChanged reports comments deleted while they were read, so the
// pages no longer meet: the caller keeps what it read before and says the
// latest may be missing.
var ErrCommentsChanged = errors.New("github: comments changed while read")

// maxCommentsBody bounds one page's answer.
const maxCommentsBody = 4 << 20

// maxLatestCommentsPage bounds the last page a Link header may name.
const maxLatestCommentsPage = 10000

// ReadComments reads the latest conversation comments of the issue or pull
// request reference names, or of source itself when reference is empty. It
// is a read like github_read_context: reference takes the forms
// ParseReadReference reads for an issue or pull request, and a repository
// other than source's own is read only when Config.ReadRepositories or
// Config.MayRead grants it and Config.ReadRefused does not refuse it, else
// the error is a *ReadError and nothing is requested. want and keep are
// LatestCommentsAPI's. The API must implement LatestCommentsAPI.
func (c *Connector) ReadComments(ctx context.Context, source address.Address, reference string, want int, keep func(ContextComment) bool) ([]ContextComment, error) {
	target, err := c.readTarget(ctx, source, reference)
	if err != nil {
		return nil, err
	}
	ref, err := c.ref(target)
	if err != nil {
		return nil, err
	}
	api, ok := c.cfg.API.(LatestCommentsAPI)
	if !ok {
		return nil, fmt.Errorf("%w: github comment reads", connector.ErrUnsupported)
	}
	return api.LatestComments(ctx, ref, want, keep)
}

// LatestComments implements LatestCommentsAPI. It reads
// GET /repos/{owner}/{repo}/issues/{n}/comments, which GitHub lists oldest
// first: page 1, then the page its Link header names rel="last", then the
// one before that when the last page is short of want kept comments. A last
// page that comes back empty, or a page before it that is not full, means
// comments were deleted meanwhile: ErrCommentsChanged.
//
// Every request goes to the API's own base. The Link header gives only a
// page number, bounded; the request is built here and never taken from the
// header. A redirect to another host or scheme is refused, so the token
// stays with the host it was given for.
func (a *HTTPAPI) LatestComments(ctx context.Context, ref Ref, want int, keep func(ContextComment) bool) ([]ContextComment, error) {
	if err := validReadRef(ref, 1); err != nil {
		return nil, err
	}
	first, last, err := a.commentsPage(ctx, ref, 1)
	if err != nil || last <= 1 {
		return first, err
	}
	tail, _, err := a.commentsPage(ctx, ref, last)
	if err != nil {
		return nil, err
	}
	if len(tail) == 0 {
		// Comments were deleted since page 1 named its last page: the latest
		// are not known, and an empty page must not read as none.
		return nil, fmt.Errorf("%w: page %d of %s is empty", ErrCommentsChanged, last, ref)
	}
	if kept(tail, keep) >= want {
		return tail, nil
	}
	before := first
	if last-1 > 1 {
		if before, _, err = a.commentsPage(ctx, ref, last-1); err != nil {
			return nil, err
		}
		if len(before) < contextPageSize {
			return nil, fmt.Errorf("%w: page %d of %s is short", ErrCommentsChanged, last-1, ref)
		}
	}
	return append(before, tail...), nil
}

func kept(comments []ContextComment, keep func(ContextComment) bool) int {
	n := 0
	for _, c := range comments {
		if keep == nil || keep(c) {
			n++
		}
	}
	return n
}

// commentsPage is one page of an issue's comments and the last page its
// Link header names, 0 when it names none.
func (a *HTTPAPI) commentsPage(ctx context.Context, ref Ref, page int) ([]ContextComment, int, error) {
	target := fmt.Sprintf("%s/repos/%s/%s/issues/%d/comments?per_page=%d&page=%d", a.client.Base, ref.Owner, ref.Repo, ref.Number, contextPageSize, page)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("github: comments on %s: %w", ref, err)
	}
	for k, vs := range a.client.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := sameHostOnly(a.client.HTTP).Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("github: comments on %s: %w", ref, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxCommentsBody))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, 0, fmt.Errorf("github: comments on %s: %w", ref, &httpjson.Error{Status: resp.StatusCode, Method: http.MethodGet, URL: target, Body: strings.TrimSpace(string(snippet))})
	}
	var out []ContextComment
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxCommentsBody)).Decode(&out); err != nil {
		return nil, 0, fmt.Errorf("github: comments on %s: %w", ref, err)
	}
	return out[:min(len(out), contextPageSize)], lastPage(resp.Header.Get("Link")), nil
}

// errOtherHost is why sameHostOnly stops a redirect.
var errOtherHost = errors.New("github: redirect to another host refused")

// sameHostOnly is base, refusing a redirect that leaves the host or scheme
// of the request it started with. A redirect that stays is left to base's
// own policy.
func sameHostOnly(base *http.Client) *http.Client {
	out := &http.Client{Timeout: httpjson.DefaultTimeout}
	if base != nil {
		out = &http.Client{Transport: base.Transport, Jar: base.Jar, Timeout: base.Timeout, CheckRedirect: base.CheckRedirect}
	}
	next := out.CheckRedirect
	out.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if first := via[0].URL; req.URL.Scheme != first.Scheme || req.URL.Host != first.Host {
			return errOtherHost
		}
		if next != nil {
			return next(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return out
}

// lastPage is the page number a Link header names rel="last", 0 when it
// names none or one out of bounds.
func lastPage(link string) int {
	for _, part := range strings.Split(link, ",") {
		target, params, ok := strings.Cut(strings.TrimSpace(part), ";")
		if !ok || !strings.Contains(params, `rel="last"`) {
			continue
		}
		u, err := url.Parse(strings.Trim(strings.TrimSpace(target), "<>"))
		if err != nil {
			return 0
		}
		n, err := strconv.Atoi(u.Query().Get("page"))
		if err != nil || n < 1 || n > maxLatestCommentsPage {
			return 0
		}
		return n
	}
	return 0
}

// LatestComments implements LatestCommentsAPI over the comments ReadContext
// lists, with the same page size.
func (f *Fake) LatestComments(_ context.Context, ref Ref, want int, keep func(ContextComment) bool) ([]ContextComment, error) {
	if err := validReadRef(ref, 1); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	all := copyContext(f.contexts[ref.String()]).Comments
	for _, c := range f.comments {
		if c.Ref == ref && c.ReviewRoot == 0 {
			all = append(all, ContextComment{ID: c.ID, Body: c.Body})
		}
	}
	start := max(0, (len(all)-1)/contextPageSize*contextPageSize)
	if kept(all[start:], keep) < want {
		start = max(0, start-contextPageSize)
	}
	return all[start:], nil
}

var (
	_ LatestCommentsAPI = (*HTTPAPI)(nil)
	_ LatestCommentsAPI = (*Fake)(nil)
)
