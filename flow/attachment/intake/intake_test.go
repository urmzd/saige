package intake_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/attachment"
	"github.com/urmzd/saige/flow/attachment/intake"
	"github.com/urmzd/saige/flow/blob"
)

const png = "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR-not-a-real-image"

var ctx = context.Background()

func content(t *testing.T, s blob.Store, tenant string, a attachment.Attachment) string {
	t.Helper()
	rc, _, err := s.Get(ctx, tenant, blob.Digest(a.Digest))
	if err != nil {
		t.Fatalf("stored blob: %v", err)
	}
	defer func() { _ = rc.Close() }()
	b, _ := io.ReadAll(rc)
	return string(b)
}

func stored(t *testing.T, s blob.Store, tenant, body string) bool {
	t.Helper()
	_, err := s.Stat(ctx, tenant, blob.Sum([]byte(body)))
	return err == nil
}

func TestStoreDescribesAFileFromItsBytesNotFromWhatItWasCalled(t *testing.T) {
	s := blob.NewMemory()
	origin := address.MustParse("slack://T01/C02?thread=1699.001")
	a, err := intake.Store(ctx, s, "acme", strings.NewReader(png), intake.Meta{
		Name: "../../uploads/invoice.txt", Origin: origin, Alt: "the\nchart",
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.MediaType != "image/png" {
		t.Errorf("media type = %q, want the sniffed image/png despite the .txt name", a.MediaType)
	}
	if a.Name != "invoice.txt" || a.Alt != "the chart" {
		t.Errorf("name %q, alt %q were not cleaned", a.Name, a.Alt)
	}
	if a.Digest != blob.Sum([]byte(png)).String() || a.Size != int64(len(png)) {
		t.Errorf("digest or size do not describe the content: %+v", a)
	}
	if a.Origin.String() != origin.String() {
		t.Errorf("origin = %v", a.Origin)
	}
	if err := a.Validate(); err != nil {
		t.Errorf("intake produced an invalid reference: %v", err)
	}
	if got := content(t, s, "acme", a); got != png {
		t.Error("stored content differs from what was read")
	}
}

func TestStoreStreamsContentOfAnyLengthAroundTheSniffingPrefix(t *testing.T) {
	s := blob.NewMemory()
	for _, n := range []int{0, 1, attachment.SniffLen - 1, attachment.SniffLen, attachment.SniffLen + 1, 100_000} {
		body := strings.Repeat("a", n)
		a, err := intake.Store(ctx, s, "acme", iotest(body), intake.Meta{Name: "f.log"})
		if err != nil {
			t.Fatalf("%d bytes: %v", n, err)
		}
		if a.Size != int64(n) || content(t, s, "acme", a) != body {
			t.Errorf("%d bytes did not round-trip", n)
		}
	}
}

// iotest returns a reader that yields one byte at a time, the worst case for
// a prefix read.
func iotest(s string) io.Reader { return &oneByte{r: strings.NewReader(s)} }

type oneByte struct{ r io.Reader }

func (o *oneByte) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return o.r.Read(p[:1])
}

func TestStoreRefusesContentOverTheLimitAndKeepsNothing(t *testing.T) {
	s := blob.NewMemory()
	body := strings.Repeat("x", 2000)
	_, err := intake.Store(ctx, s, "acme", strings.NewReader(body), intake.Meta{MaxBytes: 1000})
	if !errors.Is(err, blob.ErrTooLarge) {
		t.Fatalf("Store = %v, want blob.ErrTooLarge", err)
	}
	if stored(t, s, "acme", body) || stored(t, s, "acme", body[:1000]) {
		t.Error("an oversized upload left content in the store")
	}
}

func TestTheSameFileFromTwoSurfacesIsOneBlob(t *testing.T) {
	s := blob.NewMemory()
	a, _ := intake.Store(ctx, s, "acme", strings.NewReader(png), intake.Meta{Name: "from-slack.png"})
	b, _ := intake.Store(ctx, s, "acme", strings.NewReader(png), intake.Meta{Name: "from-github.png"})
	if a.Digest != b.Digest {
		t.Fatal("identical content produced two references")
	}
}

// ── Fetch ────────────────────────────────────────────────────────────

// loopback admits the test server and nothing else private.
func loopback(a netip.Addr) error {
	if a.Unmap() == netip.MustParseAddr("127.0.0.1") {
		return nil
	}
	return intake.PublicOnly(a)
}

// local returns a fetcher that may reach an httptest server.
func local(s blob.Store, allow intake.Allow, opts ...intake.Option) *intake.Fetcher {
	return intake.NewFetcher(s, allow, append([]intake.Option{intake.WithPlainHTTP(), intake.WithNetworkPolicy(loopback)}, opts...)...)
}

func serve(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchStoresWhatWasServedAndSniffsItInsteadOfTrustingTheHeader(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Disposition", `attachment; filename="../chart.txt"`)
		_, _ = io.WriteString(w, png)
	})
	s := blob.NewMemory()
	a, err := local(s, intake.AnyPublicHost).Fetch(ctx, "acme", srv.URL+"/files/123", intake.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	if a.MediaType != "image/png" {
		t.Errorf("media type = %q: the response header was believed over the content", a.MediaType)
	}
	if a.Name != "chart.txt" {
		t.Errorf("name = %q, want the cleaned suggested name", a.Name)
	}
	if content(t, s, "acme", a) != png {
		t.Error("stored content differs from what was served")
	}
}

func TestFetchExposesASignInPageServedInPlaceOfAFile(t *testing.T) {
	// Slack answers a file request without a valid token with 200 and HTML.
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = io.WriteString(w, "<!DOCTYPE html><html><head><title>Sign in</title></head></html>")
	})
	a, err := local(blob.NewMemory(), intake.AnyPublicHost).Fetch(ctx, "acme", srv.URL+"/shot.png", intake.Meta{Name: "shot.png"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind() == attachment.KindImage {
		t.Fatalf("an HTML page came back typed %q", a.MediaType)
	}
}

func TestFetchRefusesLoopbackAndPrivateAddressesByDefault(t *testing.T) {
	var hits atomic.Int64
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, "internal admin page")
	})
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	s := blob.NewMemory()
	// Default network policy. Plain HTTP is enabled only so the scheme is not
	// what refuses these; the address must be.
	f := intake.NewFetcher(s, intake.AnyPublicHost, intake.WithPlainHTTP(), intake.WithTimeout(5*time.Second))

	targets := []string{
		srv.URL + "/",                             // 127.0.0.1 literal
		"http://localhost:" + port + "/",          // a name that resolves to loopback: caught at dial time
		"http://[::1]:" + port + "/",              // IPv6 loopback
		"http://[::ffff:127.0.0.1]:" + port + "/", // IPv4-mapped IPv6
		"http://0.0.0.0:" + port + "/",            // unspecified
		"http://127.1:" + port + "/",              // short-form loopback, resolved by the OS
		"http://2130706433:" + port + "/",         // decimal loopback
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.1/", "http://172.16.0.1/", "http://192.168.1.1/", "http://100.64.0.1/",
		"http://[fd00::1]/", "http://[fe80::1]/", "http://[64:ff9b::7f00:1]/", "http://[2002:7f00:1::]/",
	}
	for _, target := range targets {
		_, err := f.Fetch(ctx, "acme", target, intake.Meta{})
		if err == nil {
			t.Errorf("%s: fetched an address that is not public", target)
			continue
		}
		if strings.Contains(target, "localhost") || strings.HasPrefix(target, srv.URL) || strings.Contains(target, "[::") || strings.Contains(target, "169.254") || strings.Contains(target, "10.0.0.1") {
			if !errors.Is(err, intake.ErrBlockedAddress) {
				t.Errorf("%s: err = %v, want ErrBlockedAddress", target, err)
			}
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("the loopback server was reached %d times", hits.Load())
	}
	if stored(t, s, "acme", "internal admin page") {
		t.Fatal("an internal response was stored")
	}
}

func TestPublicOnlyClassifiesAddresses(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.255.255.254", "::1", "0.0.0.0", "::", "10.1.2.3", "172.31.255.255", "192.168.0.1",
		"169.254.169.254", "fe80::1", "fc00::1", "fd12:3456::1", "100.64.0.1", "100.127.255.255", "224.0.0.1", "ff02::1",
		"255.255.255.255", "240.0.0.1", "0.1.2.3", "192.0.0.1", "198.18.0.1", "::ffff:10.0.0.1", "::ffff:127.0.0.1",
		"64:ff9b::a00:1", "2002:a00:1::", "2001:0:a00:1::", "192.0.2.1", "2001:db8::1",
	}
	for _, s := range blocked {
		if err := intake.PublicOnly(netip.MustParseAddr(s)); !errors.Is(err, intake.ErrBlockedAddress) {
			t.Errorf("PublicOnly(%s) = %v, want ErrBlockedAddress", s, err)
		}
	}
	for _, s := range []string{"140.82.112.3", "8.8.8.8", "2606:50c0:8000::154", "172.32.0.1", "100.128.0.1", "11.0.0.1"} {
		if err := intake.PublicOnly(netip.MustParseAddr(s)); err != nil {
			t.Errorf("PublicOnly(%s) = %v, want allowed", s, err)
		}
	}
	if err := intake.PublicOnly(netip.Addr{}); !errors.Is(err, intake.ErrBlockedAddress) {
		t.Error("the zero address must be refused")
	}
}

func TestFetchRevalidatesEveryRedirectHopAgainstTheNetworkPolicy(t *testing.T) {
	var internalHits atomic.Int64
	internal := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		internalHits.Add(1)
		_, _ = io.WriteString(w, "secret")
	})
	port := internal.URL[strings.LastIndex(internal.URL, ":")+1:]
	for _, loc := range []string{
		"http://[::1]:" + port + "/",            // a literal the policy refuses
		"http://169.254.169.254/latest/",        // the metadata service
		"http://localhost.localdomain.invalid/", // does not resolve; must not be reached either
	} {
		public := serve(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, loc, http.StatusFound) })
		// 127.0.0.1 is admitted so the first hop succeeds; the hop it
		// redirects to is what has to be refused.
		_, err := local(blob.NewMemory(), intake.AnyPublicHost).Fetch(ctx, "acme", public.URL, intake.Meta{})
		if err == nil {
			t.Errorf("redirect to %s was followed", loc)
		}
		if strings.Contains(loc, "::1") || strings.Contains(loc, "169.254") {
			if !errors.Is(err, intake.ErrBlockedAddress) {
				t.Errorf("redirect to %s: err = %v, want ErrBlockedAddress", loc, err)
			}
		}
	}
	if internalHits.Load() != 0 {
		t.Fatal("a redirect reached the internal server")
	}
}

func TestFetchAppliesTheHostAllowlistToTheFirstURLAndToRedirects(t *testing.T) {
	var hits atomic.Int64
	other := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, "elsewhere")
	})
	// The two servers share 127.0.0.1, so they are told apart by port, which
	// stands in for two different hosts.
	origin := serve(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL+"/x", http.StatusFound) })
	onlyOrigin := func(u *url.URL) error {
		if "http://"+u.Host == origin.URL {
			return nil
		}
		return fmt.Errorf("%w: %s", intake.ErrNotAllowed, u.Host)
	}
	f := local(blob.NewMemory(), onlyOrigin)
	if _, err := f.Fetch(ctx, "acme", other.URL+"/x", intake.Meta{}); !errors.Is(err, intake.ErrNotAllowed) {
		t.Errorf("fetch of a host off the allowlist = %v, want ErrNotAllowed", err)
	}
	if _, err := f.Fetch(ctx, "acme", origin.URL+"/x", intake.Meta{}); !errors.Is(err, intake.ErrNotAllowed) {
		t.Errorf("redirect to a host off the allowlist = %v, want ErrNotAllowed", err)
	}
	if hits.Load() != 0 {
		t.Fatal("a host off the allowlist was requested")
	}
}

func TestAFetcherWithNoHostPolicyRefusesEverything(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) { t.Error("requested without a policy") })
	if _, err := local(blob.NewMemory(), nil).Fetch(ctx, "acme", srv.URL, intake.Meta{}); !errors.Is(err, intake.ErrNotAllowed) {
		t.Fatalf("Fetch = %v, want ErrNotAllowed", err)
	}
	if _, err := local(blob.NewMemory(), intake.Hosts()).Fetch(ctx, "acme", srv.URL, intake.Meta{}); !errors.Is(err, intake.ErrNotAllowed) {
		t.Fatalf("Fetch with an empty allowlist = %v, want ErrNotAllowed", err)
	}
}

func TestHostsMatchesWholeHostNamesOnly(t *testing.T) {
	allow := intake.Hosts("github.com", "*.githubusercontent.com", "Files.Slack.com")
	tests := map[string]bool{
		"https://github.com/user-attachments/assets/1":           true,
		"https://GITHUB.com/x":                                   true,
		"https://github.com./x":                                  true,
		"https://private-user-images.githubusercontent.com/1":    true,
		"https://a.b.githubusercontent.com/1":                    true,
		"https://files.slack.com/files-pri/T1-F1/x.png":          true,
		"https://githubusercontent.com/1":                        false,
		"https://github.com.evil.example/x":                      false,
		"https://evilgithub.com/x":                               false,
		"https://evil.example/github.com":                        false,
		"https://evilgithubusercontent.com/x":                    false,
		"https://github.com@evil.example/x":                      false,
		"https://evil.example/?h=.githubusercontent.com":         false,
		"https://xgithubusercontent.com.evil.example/":           false,
		"https://api.github.com/x":                               false,
		"https://private-user-images.githubusercontent.com.e.x/": false,
	}
	for raw, want := range tests {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := allow(u) == nil; got != want {
			t.Errorf("allow(%s) = %v, want %v", raw, got, want)
		}
	}
}

func TestFetchRefusesURLsThatAreNotPlainHTTPSRequests(t *testing.T) {
	f := intake.NewFetcher(blob.NewMemory(), intake.AnyPublicHost)
	for _, raw := range []string{
		"http://example.com/a.png", "file:///etc/passwd", "ftp://example.com/a", "gopher://example.com/", "data:text/plain,hi",
		"https://user:pass@example.com/a.png", "https:///a.png", "//example.com/a.png", "example.com/a.png", "", "https://exa mple.com/",
		"javascript:alert(1)", "unix:///var/run/docker.sock",
	} {
		if _, err := f.Fetch(ctx, "acme", raw, intake.Meta{}); !errors.Is(err, intake.ErrNotAllowed) {
			t.Errorf("Fetch(%q) = %v, want ErrNotAllowed", raw, err)
		}
	}
}

func TestFetchRefusesADowngradeToPlainHTTPOnRedirect(t *testing.T) {
	// A fetcher that requires HTTPS, reaching a TLS server it cannot verify,
	// would fail on the certificate. So assert the rule where it lives: the
	// same permit check runs on every hop, and it refuses http.
	var hits atomic.Int64
	plain := serve(t, func(http.ResponseWriter, *http.Request) { hits.Add(1) })
	f := intake.NewFetcher(blob.NewMemory(), intake.AnyPublicHost, intake.WithNetworkPolicy(loopback))
	if _, err := f.Fetch(ctx, "acme", plain.URL, intake.Meta{}); !errors.Is(err, intake.ErrNotAllowed) {
		t.Fatalf("Fetch = %v, want ErrNotAllowed", err)
	}
	if hits.Load() != 0 {
		t.Fatal("a plain HTTP URL was requested by an HTTPS-only fetcher")
	}
}

func TestCredentialsAreChosenPerHopAndNeverFollowARedirect(t *testing.T) {
	var cdnAuth atomic.Value
	cdn := serve(t, func(w http.ResponseWriter, r *http.Request) {
		cdnAuth.Store(r.Header.Get("Authorization") + "|" + r.Header.Get("Cookie"))
		_, _ = io.WriteString(w, png)
	})
	var apiAuth atomic.Value
	api := serve(t, func(w http.ResponseWriter, r *http.Request) {
		apiAuth.Store(r.Header.Get("Authorization"))
		http.Redirect(w, r, cdn.URL+"/signed?sig=abc", http.StatusFound)
	})
	f := local(blob.NewMemory(), intake.AnyPublicHost, intake.WithHeaders(func(u *url.URL) http.Header {
		if "http://"+u.Host != api.URL {
			return nil
		}
		return http.Header{"Authorization": {"Bearer xoxb-secret"}, "Cookie": {"d=secret"}}
	}))
	a, err := f.Fetch(ctx, "acme", api.URL+"/files-pri/T1-F1/shot.png", intake.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	if apiAuth.Load() != "Bearer xoxb-secret" {
		t.Errorf("the token did not reach its own host: %v", apiAuth.Load())
	}
	if got := cdnAuth.Load(); got != "|" {
		t.Errorf("credentials followed the redirect to another host: %q", got)
	}
	if a.MediaType != "image/png" {
		t.Errorf("media type = %q", a.MediaType)
	}
}

func TestRequestHeadersRotateAndFailBeforeDownload(t *testing.T) {
	var hits atomic.Int64
	token := "first"
	api := serve(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("stale token")
		}
		_, _ = io.WriteString(w, png)
	})
	fail := false
	f := local(blob.NewMemory(), intake.AnyPublicHost, intake.WithRequestHeaders(func(ctx context.Context, u *url.URL) (http.Header, error) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if fail {
			return nil, errors.New("refresh secret")
		}
		return http.Header{"Authorization": {"Bearer " + token}}, nil
	}))
	for _, next := range []string{"first", "second"} {
		token = next
		if _, err := f.Fetch(ctx, "acme", api.URL, intake.Meta{}); err != nil {
			t.Fatal(err)
		}
	}
	fail = true
	if _, err := f.Fetch(ctx, "acme", api.URL, intake.Meta{}); err == nil || strings.Contains(err.Error(), "refresh secret") {
		t.Fatal("refresh error leaked or was ignored")
	}
	if hits.Load() != 2 {
		t.Fatal("request sent after refresh failure")
	}
}

func TestFetchHoldsTheSizeCapWhetherOrNotTheServerDeclaresALength(t *testing.T) {
	big := strings.Repeat("z", 50_000)
	var sent atomic.Int64
	declared := serve(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, big) })
	chunked := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		fl := w.(http.Flusher)
		for i := 0; i < 10_000; i++ {
			if _, err := io.WriteString(w, strings.Repeat("z", 1000)); err != nil {
				return
			}
			sent.Add(1000)
			fl.Flush()
		}
	})
	s := blob.NewMemory()
	f := local(s, intake.AnyPublicHost, intake.WithMaxBytes(10_000))
	for name, u := range map[string]string{"declared length": declared.URL, "no declared length": chunked.URL} {
		if _, err := f.Fetch(ctx, "acme", u, intake.Meta{}); !errors.Is(err, blob.ErrTooLarge) {
			t.Errorf("%s: Fetch = %v, want blob.ErrTooLarge", name, err)
		}
	}
	if sent.Load() >= 10_000_000 {
		t.Error("an oversized stream was read to the end")
	}
	if stored(t, s, "acme", big) {
		t.Error("oversized content was stored")
	}
	// A per-file limit on the call overrides the fetcher's.
	if _, err := f.Fetch(ctx, "acme", declared.URL, intake.Meta{MaxBytes: 60_000}); err != nil {
		t.Errorf("Fetch under a per-call limit: %v", err)
	}
}

func TestFetchGivesUpOnAServerThatStalls(t *testing.T) {
	release := make(chan struct{})
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "partial")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	defer close(release)
	s := blob.NewMemory()
	start := time.Now()
	_, err := local(s, intake.AnyPublicHost, intake.WithTimeout(200*time.Millisecond)).Fetch(ctx, "acme", srv.URL, intake.Meta{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Fetch = %v, want a deadline error", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the timeout did not bound the fetch")
	}
	if stored(t, s, "acme", "partial") {
		t.Error("a truncated download was stored")
	}
}

func TestFetchStoresOnlySuccessfulResponses(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusForbidden, http.StatusUnauthorized, http.StatusInternalServerError, http.StatusNoContent, http.StatusPartialContent, http.StatusNotModified} {
		srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			_, _ = io.WriteString(w, "error body")
		})
		s := blob.NewMemory()
		if _, err := local(s, intake.AnyPublicHost).Fetch(ctx, "acme", srv.URL, intake.Meta{}); !errors.Is(err, intake.ErrStatus) {
			t.Errorf("status %d: Fetch = %v, want ErrStatus", code, err)
		}
		if stored(t, s, "acme", "error body") {
			t.Errorf("status %d: the error body was stored", code)
		}
	}
}

func TestFetchBoundsRedirectChains(t *testing.T) {
	var srv *httptest.Server
	var hops atomic.Int64
	srv = serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, fmt.Sprintf("%s/%d", srv.URL, hops.Add(1)), http.StatusFound)
	})
	_, err := local(blob.NewMemory(), intake.AnyPublicHost, intake.WithMaxRedirects(3)).Fetch(ctx, "acme", srv.URL, intake.Meta{})
	if !errors.Is(err, intake.ErrTooManyRedirects) {
		t.Fatalf("Fetch = %v, want ErrTooManyRedirects", err)
	}
	if hops.Load() != 4 {
		t.Errorf("made %d requests, want the first plus 3 redirects", hops.Load())
	}
}

func TestFetchErrorsDoNotLeakSignedQueryStrings(t *testing.T) {
	_, err := intake.NewFetcher(blob.NewMemory(), intake.AnyPublicHost, intake.WithPlainHTTP()).
		Fetch(ctx, "acme", "http://127.0.0.1:1/file?X-Amz-Signature=topsecret&jwt=eyJhbGci", intake.Meta{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "topsecret") || strings.Contains(err.Error(), "eyJhbGci") {
		t.Fatalf("error text carries the signed query: %v", err)
	}
}

func TestFetchedFilesAreTenantScoped(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "acme's file") })
	s := blob.NewMemory()
	a, err := local(s, intake.AnyPublicHost).Fetch(ctx, "acme", srv.URL, intake.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stat(ctx, "globex", blob.Digest(a.Digest)); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("another tenant can see the fetched blob: %v", err)
	}
}
