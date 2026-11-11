package github_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/attachment/intake"
	"github.com/urmzd/saige/flow/blob"
	"github.com/urmzd/saige/flow/connectors"
	"github.com/urmzd/saige/flow/connectors/github"
	"github.com/urmzd/saige/flow/connectors/mention"
)

const (
	assetPNG = "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00"
	assetMP4 = "\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom\x00\x00\x00\x08free"
)

// A body as GitHub's editor writes it: a pasted screenshot as a Markdown
// image, a resized one as an <img> tag, an uploaded video as a bare URL on its
// own line, a legacy image, a repository asset, and things that are links
// rather than attached files.
const pastedBody = "@docs-bot why does the build fail?\r\n\r\n" +
	"![build log](https://github.com/user-attachments/assets/0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0)\r\n\r\n" +
	`<img width="640" alt="the &quot;red&quot; job" src="https://github.com/user-attachments/assets/11111111-2222-3333-4444-555555555555" />` + "\r\n\r\n" +
	"https://github.com/user-attachments/assets/66666666-7777-8888-9999-000000000000\r\n\r\n" +
	"![Image](https://user-images.githubusercontent.com/1502855/123456789-abcdef01.png)\r\n" +
	"![older](https://github.com/acme/handbook/assets/1502855/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee)\r\n" +
	"[![CI](https://img.shields.io/badge/ci-red-red)](https://example.com)\r\n" +
	"![again](https://github.com/user-attachments/assets/0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0)\r\n" +
	"See https://github.com/acme/handbook/pull/7 and https://github.com/user-attachments/assets/not-on-its-own-line too.\r\n" +
	"<video src=\"https://github.com/user-attachments/assets/77777777-8888-9999-aaaa-bbbbbbbbbbbb\" controls></video>\r\n"

// The files a body references are found in the order they appear, each once,
// with their alt text, and nothing that is merely a link is taken for one.
func TestTheFilesInABodyAreFoundInOrderAndLinksAreLeftAlone(t *testing.T) {
	got := github.MediaIn(pastedBody)
	want := [][2]string{
		{"https://github.com/user-attachments/assets/0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0", "build log"},
		{"https://github.com/user-attachments/assets/11111111-2222-3333-4444-555555555555", `the "red" job`},
		{"https://github.com/user-attachments/assets/66666666-7777-8888-9999-000000000000", ""},
		{"https://user-images.githubusercontent.com/1502855/123456789-abcdef01.png", "Image"},
		{"https://github.com/acme/handbook/assets/1502855/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "older"},
		{"https://github.com/user-attachments/assets/77777777-8888-9999-aaaa-bbbbbbbbbbbb", ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("found\n %q\nwant\n %q", got, want)
	}
	// A rendered body carries the signed form.
	rendered := `<p><a target="_blank" rel="noopener noreferrer" href="https://private-user-images.githubusercontent.com/1/2-3.png?jwt=eyJ.a.b"><img src="https://private-user-images.githubusercontent.com/1/2-3.png?jwt=eyJ.a.b" alt="build log" style="max-width: 100%;"></a> <img class="emoji" alt=":tada:" src="https://github.githubassets.com/images/icons/emoji/unicode/1f389.png"></p>`
	if got := github.MediaIn(rendered); len(got) != 1 || !strings.HasPrefix(got[0][0], "https://private-user-images.githubusercontent.com/") || got[0][1] != "build log" {
		t.Fatalf("rendered body: %q", got)
	}
}

// Files are fetched from GitHub's attachment forms and the storage they
// redirect to, and from nothing else on github.com or anywhere.
func TestGitHubFilesAreFetchedOnlyFromAttachmentHostsAndPaths(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://github.com/user-attachments/assets/0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0":      true,
		"https://github.com/user-attachments/files/17000000/trace.log":                         true,
		"https://github.com/acme/handbook/assets/1502855/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee": true,
		"https://user-images.githubusercontent.com/1502855/1-2.png":                            true,
		"https://private-user-images.githubusercontent.com/1/2.png?jwt=x":                      true,
		"https://github-production-user-asset-6210df.s3.amazonaws.com/1/2.png?X-Amz-Signature": true,
		"https://objects.githubusercontent.com/github-production-repository-file-5c1aeb/1/2":   true,
		"https://github.com/acme/handbook/archive/refs/heads/main.zip":                         false,
		"https://github.com/acme/handbook/assets/../settings":                                  false,
		"https://github.com/login":                                        false,
		"https://api.github.com/repos/acme/handbook":                      false,
		"https://github.com.evil.example/user-attachments/assets/x":       false,
		"https://evil-bucket.s3.amazonaws.com/x.png":                      false,
		"https://raw.githubusercontent.com/acme/handbook/main/secret.env": false,
	} {
		u, _ := url.Parse(raw)
		if got := github.FileAllowed(u) == nil; got != want {
			t.Errorf("%s: allowed = %v, want %v", raw, got, want)
		}
	}
	for _, raw := range []string{"https://api.github.com/x", "https://github.com/user-attachments/assets/x", "https://objects.githubusercontent.com/x"} {
		u, _ := url.Parse(raw)
		if h := github.NoHeaders(u); h != nil {
			t.Errorf("%s was sent headers %v; no credential goes to a file host", raw, h)
		}
	}
}

func loopback(a netip.Addr) error {
	if a.Unmap() == netip.MustParseAddr("127.0.0.1") {
		return nil
	}
	return intake.PublicOnly(a)
}

// assets serves files at GitHub's paths and records any credential it was sent.
func assets(t *testing.T, bodies map[string]string) (*httptest.Server, *atomic.Value) {
	t.Helper()
	var auth atomic.Value
	auth.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a := r.Header.Get("Authorization"); a != "" {
			auth.Store(a)
		}
		body, ok := bodies[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &auth
}

// local rewrites a GitHub file URL to the test server, keeping its path, and
// lets the fetcher reach it while still holding redirects to the real policy.
func local(c *github.Connector, store blob.Store, srv *httptest.Server) {
	base, _ := url.Parse(srv.URL)
	github.ServeFilesFrom(c, intake.NewFetcher(store, func(u *url.URL) error {
		if u.Host == base.Host {
			return nil
		}
		return github.FileAllowed(u)
	}, intake.WithPlainHTTP(), intake.WithNetworkPolicy(loopback), intake.WithHeaders(github.NoHeaders)), func(raw string) string {
		u, _ := url.Parse(raw)
		return srv.URL + u.Path
	})
}

func commentWith(body string, private bool) string {
	b := strings.Replace(issueComment, `"body": "can @docs-bot summarize what changed here, and @docs-bot check the retry path too?"`,
		fmt.Sprintf(`"body": %q`, body), 1)
	return strings.Replace(b, `"private": false`, fmt.Sprintf(`"private": %v`, private), 1)
}

// The files in a public repository's comment become the event's attachments,
// in order, stored under the connector's tenant, fetched without any
// credential. A file GitHub no longer serves is carried as unread.
func TestFilesInAPublicCommentBecomeTheEventsAttachments(t *testing.T) {
	store := blob.NewMemory()
	srv, auth := assets(t, map[string]string{
		"/user-attachments/assets/0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0": assetPNG,
		"/user-attachments/assets/66666666-7777-8888-9999-000000000000": assetMP4,
	})
	c, tags := start(t, github.Config{Agents: mention.NewNames("docs-bot"), Tenant: "acme", Files: connectors.Files{Store: store}})
	local(c, store, srv)

	body := "@docs-bot why?\n\n![build log](https://github.com/user-attachments/assets/0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0)\n" +
		"https://github.com/user-attachments/assets/66666666-7777-8888-9999-000000000000\n" +
		"![gone](https://github.com/user-attachments/assets/deadbeef-0000-0000-0000-000000000000)\n"
	w := httptest.NewRecorder()
	c.ServeHTTP(w, deliver(t, "issue_comment", "d-files-1", commentWith(body, false)))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	tag := receive(t, tags)
	files := request(t, tag).Attachments
	if tag.Tenant != "acme" || len(files) != 3 {
		t.Fatalf("event tenant %q attachments %+v", tag.Tenant, files)
	}
	if a := files[0]; a.MediaType != "image/png" || a.Alt != "build log" || a.Origin.String() != "github://urmzd/fiber/issues/42" {
		t.Errorf("screenshot = %+v", a)
	}
	if a := files[1]; a.MediaType != "video/mp4" {
		t.Errorf("video = %+v", a)
	}
	if a := files[2]; a.Unread != "the surface did not return it" || a.Alt != "gone" {
		t.Errorf("missing file = %+v", a)
	}
	if got := auth.Load().(string); got != "" {
		t.Errorf("a file host was sent %q", got)
	}
}

// A private repository's files are read through the signed URLs in the
// rendered body, which the API returns for the installation token. When that
// read fails, each file is carried as unread and the event is still published.
func TestFilesInAPrivateCommentAreFetchedThroughSignedURLs(t *testing.T) {
	store := blob.NewMemory()
	srv, _ := assets(t, map[string]string{"/1502855/signed-0b1c.png": assetPNG})
	api := github.NewFake()
	ref := github.Ref{Owner: "urmzd", Repo: "fiber", Number: 42}
	api.SetBodyHTML(ref, github.BodyComment, 2201234567,
		`<p>why?</p><p><a href="https://private-user-images.githubusercontent.com/1502855/signed-0b1c.png?jwt=eyJhbGciOi.x.y"><img src="https://private-user-images.githubusercontent.com/1502855/signed-0b1c.png?jwt=eyJhbGciOi.x.y" alt="build log"></a></p>`)
	c, tags := start(t, github.Config{API: api, Agents: mention.NewNames("docs-bot"), Tenant: "acme", Files: connectors.Files{Store: store}})
	local(c, store, srv)

	body := "@docs-bot why?\n\n![build log](https://github.com/user-attachments/assets/0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0)\n"
	c.ServeHTTP(httptest.NewRecorder(), deliver(t, "issue_comment", "d-files-2", commentWith(body, true)))
	files := request(t, receive(t, tags)).Attachments
	if len(files) != 1 || files[0].MediaType != "image/png" || files[0].Alt != "build log" {
		t.Fatalf("attachments = %+v", files)
	}

	unsigned := strings.Replace(commentWith(body, true), `"id": 2201234567`, `"id": 2209999999`, 1)
	c.ServeHTTP(httptest.NewRecorder(), deliver(t, "issue_comment", "d-files-3", unsigned))
	files = request(t, receive(t, tags)).Attachments
	if len(files) != 1 || files[0].Unread != "the surface refused access to it" {
		t.Fatalf("without a signed URL: %+v", files)
	}
}

// The conversation is listed from the comments API with rendered bodies:
// every page, oldest first, the comment id as the stable id, the app's own
// comments marked Own, and files brought in only when asked.
func TestTheGitHubConversationIsListedFromTheCommentsAPI(t *testing.T) {
	store := blob.NewMemory()
	files, _ := assets(t, map[string]string{"/1/signed.png": assetPNG})
	var pages atomic.Int64
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/handbook/issues/7/comments" || r.Header.Get("Accept") != "application/vnd.github.full+json" ||
			r.Header.Get("Authorization") != "Bearer ghs_test" {
			http.Error(w, "unexpected "+r.URL.String()+" "+r.Header.Get("Accept"), http.StatusBadRequest)
			return
		}
		pages.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "1" {
			var b strings.Builder
			b.WriteString("[")
			for i := range 100 {
				if i > 0 {
					b.WriteString(",")
				}
				switch i {
				case 0:
					b.WriteString(`{"id":1,"user":{"login":"grace","type":"User"},"body":"see ![x](https://github.com/user-attachments/assets/aaaa)","body_html":"<p>see <img src=\"https://private-user-images.githubusercontent.com/1/signed.png?jwt=e.y.j\" alt=\"x\"></p>","created_at":"2026-10-01T10:00:00Z"}`)
				case 1:
					b.WriteString(`{"id":2,"user":{"login":"fiber[bot]","type":"Bot"},"body":"Here is the summary.","created_at":"2026-10-01T10:01:00Z"}`)
				default:
					fmt.Fprintf(&b, `{"id":%d,"user":{"login":"ada","type":"User"},"body":"comment %d","created_at":"2026-10-01T11:00:00Z"}`, i+1, i+1)
				}
			}
			b.WriteString("]")
			_, _ = io.WriteString(w, b.String())
			return
		}
		_, _ = io.WriteString(w, `[{"id":101,"user":{"login":"ada","type":"User"},"body":"last","created_at":"2026-10-02T09:00:00Z"}]`)
	}))
	t.Cleanup(api.Close)

	c, err := github.New(github.Config{
		API:    github.NewHTTPAPIAt(api.URL, "ghs_test", api.Client()),
		Bots:   map[string]string{"fiber[bot]": "docs-bot"},
		Tenant: "acme",
		Files:  connectors.Files{Store: store},
	})
	if err != nil {
		t.Fatal(err)
	}
	local(c, store, files)
	surface := address.MustParse("github://acme/handbook/pull/7?head_sha=abc")
	msgs, err := c.Messages(context.Background(), "acme", surface)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if pages.Load() != 2 || len(msgs) != 101 {
		t.Fatalf("listed %d messages over %d pages, want 101 over 2", len(msgs), pages.Load())
	}
	if m := msgs[0]; m.ID != "1" || m.Actor.ID != "grace" || m.Own || m.Files == nil {
		t.Errorf("first comment = %+v", m)
	}
	if m := msgs[1]; m.ID != "2" || !m.Own || m.Files != nil {
		t.Errorf("the app's own comment = %+v", m)
	}
	if m := msgs[100]; m.ID != "101" || m.Text != "last" {
		t.Errorf("last comment = %+v", m)
	}
	atts, err := msgs[0].Files(context.Background())
	if err != nil || len(atts) != 1 || atts[0].MediaType != "image/png" || atts[0].Origin.String() != "github://acme/handbook/pull/7" {
		t.Fatalf("files of the first comment = %+v, %v", atts, err)
	}
	if _, err := c.Messages(context.Background(), "globex", surface); err == nil {
		t.Error("another tenant's listing was answered")
	}
}
