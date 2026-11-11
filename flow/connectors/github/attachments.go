package github

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/urmzd/saige/flow/attachment"
	"github.com/urmzd/saige/flow/attachment/intake"
)

// Where GitHub keeps the images and videos people paste into issues, pull
// requests, comments and reviews.
//
// A body references a file by one of these URL forms:
//
//	https://github.com/user-attachments/assets/<uuid>         (current, images and videos)
//	https://github.com/user-attachments/files/<id>/<name>     (current, other files)
//	https://github.com/<owner>/<repo>/assets/<id>/<uuid>      (earlier)
//	https://user-images.githubusercontent.com/...             (legacy)
//	https://private-user-images.githubusercontent.com/...?jwt (signed, from body_html)
//
// and a github.com form answers with a redirect to signed storage:
// /user-attachments/assets and /<owner>/<repo>/assets redirect to
// AssetStorageHost, /user-attachments/files to ObjectsHost. Those were
// observed on live public repositories, not read from documentation, which
// does not name them.
const (
	AssetStorageHost  = "github-production-user-asset-6210df.s3.amazonaws.com"
	ObjectsHost       = "objects.githubusercontent.com"
	UserImagesHost    = "user-images.githubusercontent.com"
	PrivateImagesHost = "private-user-images.githubusercontent.com"
)

// Body kinds whose rendered HTML can be asked for.
const (
	BodyIssue         = "issue"          // an issue's or pull request's own body
	BodyComment       = "comment"        // an issue or pull request conversation comment
	BodyReviewComment = "review_comment" // a comment on a line of a pull request
	BodyReview        = "review"         // a pull request review's summary
)

// BodyAPI is the optional capability to read a body as GitHub renders it
// (application/vnd.github.full+json). For a private repository that is the
// only way to a file: the attachment URLs in the Markdown need a browser
// session, while body_html carries short-lived signed URLs that need none.
// HTTPAPI and Fake implement it. The installation token goes to the API host
// and never to a file host.
type BodyAPI interface {
	BodyHTML(ctx context.Context, ref Ref, kind string, id int64) (string, error)
}

// fileAllowed is the host policy for fetching a file GitHub holds: the
// attachment forms above, and the storage hosts they redirect to. Anything
// else on github.com (a page, an archive, an API path) is refused, so a body
// cannot make the deployment fetch arbitrary GitHub URLs.
func fileAllowed(u *url.URL) error {
	host := strings.ToLower(u.Hostname())
	switch host {
	case UserImagesHost, PrivateImagesHost, AssetStorageHost, ObjectsHost:
		return nil
	case "github.com":
		if githubAttachmentPath(u.EscapedPath()) {
			return nil
		}
		return fmt.Errorf("%w: %s is not a GitHub attachment path", intake.ErrNotAllowed, u.Path)
	}
	return fmt.Errorf("%w: host %q is not a GitHub file host", intake.ErrNotAllowed, host)
}

var repoAssetPath = regexp.MustCompile(`^/[A-Za-z0-9-]+/[A-Za-z0-9._-]+/assets/[0-9]+/[0-9a-fA-F-]+$`)

func githubAttachmentPath(p string) bool {
	return strings.HasPrefix(p, "/user-attachments/assets/") ||
		strings.HasPrefix(p, "/user-attachments/files/") ||
		repoAssetPath.MatchString(p)
}

// media is one file a body references.
type media struct {
	url, alt string
}

var (
	markdownImage = regexp.MustCompile(`!\[([^\]\n]*)\]\(\s*<?(https://[^\s)>]+)>?(?:\s+"[^"]*")?\s*\)`)
	htmlTag       = regexp.MustCompile(`(?is)<(img|video|source)\b[^>]*>`)
	htmlAttr      = regexp.MustCompile(`(?is)\b(src|alt)\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	bareLine      = regexp.MustCompile(`(?m)^[ \t]*(https://\S+)[ \t\r]*$`)
)

// mediaIn finds the GitHub-hosted files a Markdown body references, in the
// order they appear: Markdown images, <img>, <video> and <source> tags, and a
// bare attachment URL on a line of its own, which is how GitHub embeds an
// uploaded video. A reference to anything other than a GitHub attachment (a
// badge, an external image) is a link, not a file someone attached, and is
// left alone. A file referenced twice is taken once.
func mediaIn(body string) []media {
	type hit struct {
		at int
		m  media
	}
	var hits []hit
	for _, m := range markdownImage.FindAllStringSubmatchIndex(body, -1) {
		hits = append(hits, hit{m[0], media{url: body[m[4]:m[5]], alt: body[m[2]:m[3]]}})
	}
	for _, m := range htmlTag.FindAllStringIndex(body, -1) {
		if mm, ok := tagMedia(body[m[0]:m[1]]); ok {
			hits = append(hits, hit{m[0], mm})
		}
	}
	for _, m := range bareLine.FindAllStringSubmatchIndex(body, -1) {
		hits = append(hits, hit{m[2], media{url: body[m[2]:m[3]]}})
	}
	// Order by position: the three patterns were scanned separately.
	for i := 1; i < len(hits); i++ {
		for j := i; j > 0 && hits[j].at < hits[j-1].at; j-- {
			hits[j], hits[j-1] = hits[j-1], hits[j]
		}
	}
	out := make([]media, 0, len(hits))
	seen := map[string]bool{}
	for _, h := range hits {
		u, err := url.Parse(h.m.url)
		if err != nil || u.Scheme != "https" || fileAllowed(u) != nil || seen[h.m.url] {
			continue
		}
		seen[h.m.url] = true
		out = append(out, h.m)
	}
	return out
}

// tagMedia reads the src and alt of an <img>, <video> or <source> tag, with
// HTML entities decoded.
func tagMedia(tag string) (media, bool) {
	var m media
	for _, a := range htmlAttr.FindAllStringSubmatch(tag, -1) {
		v := html.UnescapeString(a[2] + a[3])
		switch strings.ToLower(a[1]) {
		case "src":
			m.url = v
		case "alt":
			m.alt = v
		}
	}
	return m, m.url != ""
}

// candidates turns a body's files into intake candidates. A private
// repository's body is read again as HTML for its signed URLs; if that
// fails, each file is carried as unread rather than fetched without the
// access it needs.
func (c *Connector) candidates(ctx context.Context, ref Ref, private bool, kind string, id int64, markdown string) []intake.Candidate {
	if c.fetcher == nil {
		return nil
	}
	found := mediaIn(markdown)
	if len(found) == 0 {
		return nil
	}
	if private {
		signed, err := c.signed(ctx, ref, kind, id)
		if err != nil {
			out := make([]intake.Candidate, len(found))
			for i, m := range found {
				refusal := fmt.Errorf("github: %w: the signed URL for a private repository's file could not be read: %w", intake.ErrRefused, err)
				out[i] = intake.Candidate{Name: nameOf(m.url), Alt: m.alt, Fetch: func(context.Context, string, intake.Meta) (attachment.Attachment, error) {
					return attachment.Attachment{}, refusal
				}}
			}
			return out
		}
		found = signed
	}
	return c.fetchAll(found)
}

// signed reads a body's rendered HTML and returns its files with the signed
// URLs GitHub put there.
func (c *Connector) signed(ctx context.Context, ref Ref, kind string, id int64) ([]media, error) {
	api, ok := c.cfg.API.(BodyAPI)
	if !ok {
		return nil, fmt.Errorf("this API cannot read rendered bodies")
	}
	body, err := api.BodyHTML(ctx, ref, kind, id)
	if err != nil {
		return nil, err
	}
	return mediaIn(body), nil
}

func (c *Connector) fetchAll(found []media) []intake.Candidate {
	out := make([]intake.Candidate, 0, len(found))
	for _, m := range found {
		link := m.url
		out = append(out, intake.Candidate{Name: nameOf(link), Alt: altOf(m.alt), Fetch: func(ctx context.Context, tenant string, meta intake.Meta) (attachment.Attachment, error) {
			if c.redirectForTest != nil {
				link = c.redirectForTest(link)
			}
			return c.fetcher.Fetch(ctx, tenant, link, meta)
		}})
	}
	return out
}

// nameOf is a display name from a URL: its last path segment when it looks
// like a file name. An asset id is not a name; the response's own file name
// is used instead (see intake.Fetcher).
func nameOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	base := path.Base(u.Path)
	if !strings.Contains(base, ".") {
		return ""
	}
	return base
}

// altOf drops the alt text GitHub writes by default, which says nothing.
func altOf(alt string) string {
	switch strings.ToLower(strings.TrimSpace(alt)) {
	case "", "image", "img", "screenshot", "untitled":
		return ""
	}
	return alt
}

// noHeaders is the per-hop credential policy for file downloads: none, to
// any host. The installation token belongs to the API host, and the file
// hosts take either no credential (public storage) or a signature in the URL.
func noHeaders(*url.URL) http.Header { return nil }

// bodyOf names the body that raised an event: the comment or review when the
// event is one, otherwise the issue or pull request itself.
func bodyOf(event string, h hook) (kind string, id int64, text string) {
	switch {
	case event == "issue_comment" && h.Comment != nil:
		return BodyComment, h.Comment.ID, h.Comment.Body
	case event == "pull_request_review_comment" && h.Comment != nil:
		return BodyReviewComment, h.Comment.ID, h.Comment.Body
	case event == "pull_request_review" && h.Review != nil:
		return BodyReview, h.Review.ID, h.Review.Body
	case h.PullRequest != nil:
		return BodyIssue, 0, h.PullRequest.Body
	case h.Issue != nil:
		return BodyIssue, 0, h.Issue.Body
	}
	return "", 0, ""
}

// bodyPath is the API path a rendered body is read from.
func bodyPath(ref Ref, kind string, id int64) (string, error) {
	base := "/repos/" + url.PathEscape(ref.Owner) + "/" + url.PathEscape(ref.Repo)
	switch kind {
	case BodyIssue:
		return base + "/issues/" + strconv.Itoa(ref.Number), nil
	case BodyComment:
		return base + "/issues/comments/" + strconv.FormatInt(id, 10), nil
	case BodyReviewComment:
		return base + "/pulls/comments/" + strconv.FormatInt(id, 10), nil
	case BodyReview:
		return base + "/pulls/" + strconv.Itoa(ref.Number) + "/reviews/" + strconv.FormatInt(id, 10), nil
	}
	return "", fmt.Errorf("github: no rendered body for %q", kind)
}
