package netguard_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/urmzd/saige/netguard"
)

func TestPublicOnlyClassifiesAddresses(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.255.255.254", "::1", "0.0.0.0", "::", "10.1.2.3", "172.31.255.255", "192.168.0.1",
		"169.254.169.254", "fe80::1", "fc00::1", "fd12:3456::1", "fd00:ec2::254", "100.64.0.1", "100.100.100.200",
		"100.127.255.255", "224.0.0.1", "ff02::1", "255.255.255.255", "240.0.0.1", "0.1.2.3", "192.0.0.1",
		"198.18.0.1", "::ffff:10.0.0.1", "::ffff:127.0.0.1", "::ffff:169.254.169.254", "64:ff9b::a00:1",
		"64:ff9b:1::1", "2002:a00:1::", "2001:0:a00:1::", "192.0.2.1", "198.51.100.7", "203.0.113.9", "2001:db8::1",
		"::7f00:1", "::a00:1", "fec0::1",
	}
	for _, s := range blocked {
		err := netguard.PublicOnly(netip.MustParseAddr(s))
		if !errors.Is(err, netguard.ErrBlocked) {
			t.Errorf("PublicOnly(%s) = %v, want ErrBlocked", s, err)
		}
		var be *netguard.BlockedError
		if !errors.As(err, &be) || be.Address == "" || be.Reason == "" {
			t.Errorf("PublicOnly(%s) = %#v, want a *BlockedError naming the address and reason", s, err)
		}
	}
	for _, s := range []string{
		"140.82.112.3", "8.8.8.8", "93.184.216.34", "2606:50c0:8000::154", "2606:4700::1111",
		"172.32.0.1", "100.128.0.1", "11.0.0.1", "::ffff:8.8.8.8",
	} {
		if err := netguard.PublicOnly(netip.MustParseAddr(s)); err != nil {
			t.Errorf("PublicOnly(%s) = %v, want allowed", s, err)
		}
	}
	if err := netguard.PublicOnly(netip.Addr{}); !errors.Is(err, netguard.ErrBlocked) {
		t.Error("PublicOnly must refuse the zero address")
	}
}

func TestNoMetadataRefusesOnlyMetadataAndLinkLocal(t *testing.T) {
	for _, s := range []string{
		"169.254.169.254", "169.254.0.1", "::ffff:169.254.169.254", "fd00:ec2::254", "fe80::1", "100.100.100.200",
	} {
		if err := netguard.NoMetadata(netip.MustParseAddr(s)); !errors.Is(err, netguard.ErrBlocked) {
			t.Errorf("NoMetadata(%s) = %v, want ErrBlocked", s, err)
		}
	}
	for _, s := range []string{
		"127.0.0.1", "::1", "10.0.0.5", "192.168.1.1", "172.16.0.1", "100.64.0.1", "fc00::1", "fd00:ec2::253",
		"93.184.216.34", "2606:4700::1111",
	} {
		if err := netguard.NoMetadata(netip.MustParseAddr(s)); err != nil {
			t.Errorf("NoMetadata(%s) = %v, want allowed", s, err)
		}
	}
	if err := netguard.NoMetadata(netip.Addr{}); !errors.Is(err, netguard.ErrBlocked) {
		t.Error("NoMetadata must refuse the zero address")
	}
}

func TestExceptAdmitsTheListedPrefixesAndDefersTheRest(t *testing.T) {
	p := netguard.Except(netguard.PublicOnly, netip.MustParsePrefix("127.0.0.1/32"))
	for _, s := range []string{"127.0.0.1", "::ffff:127.0.0.1", "8.8.8.8"} {
		if err := p(netip.MustParseAddr(s)); err != nil {
			t.Errorf("%s = %v, want admitted", s, err)
		}
	}
	for _, s := range []string{"127.0.0.2", "::1", "10.0.0.1", "169.254.169.254"} {
		if err := p(netip.MustParseAddr(s)); !errors.Is(err, netguard.ErrBlocked) {
			t.Errorf("%s = %v, want refused by PublicOnly", s, err)
		}
	}
	if err := netguard.Except(nil)(netip.MustParseAddr("10.0.0.1")); !errors.Is(err, netguard.ErrBlocked) {
		t.Error("a nil policy must mean PublicOnly")
	}
}

var errOwn = errors.New("pkg: destination address is blocked")

func TestTagMatchesBothSentinelsAndReadsAsTheOwnOne(t *testing.T) {
	p := netguard.Tag(netguard.PublicOnly, errOwn)
	err := p(netip.MustParseAddr("10.0.0.1"))
	if !errors.Is(err, errOwn) || !errors.Is(err, netguard.ErrBlocked) {
		t.Fatalf("err = %v, want it to match both sentinels", err)
	}
	if want := "pkg: destination address is blocked: 10.0.0.1 is not a public address"; err.Error() != want {
		t.Errorf("message %q, want %q", err, want)
	}
	var be *netguard.BlockedError
	if !errors.As(err, &be) || be.Address != "10.0.0.1" {
		t.Errorf("the BlockedError is not reachable through the tag: %#v", err)
	}
	if err := p(netip.MustParseAddr("8.8.8.8")); err != nil {
		t.Errorf("a public address was refused: %v", err)
	}

	custom := netguard.Tag(func(netip.Addr) error { return errors.New("nope") }, errOwn)
	if err := custom(netip.MustParseAddr("8.8.8.8")); err.Error() != "pkg: destination address is blocked: nope" || !errors.Is(err, errOwn) {
		t.Errorf("custom policy error = %v", err)
	}
	already := netguard.Tag(func(netip.Addr) error { return errOwn }, errOwn)
	if err := already(netip.MustParseAddr("8.8.8.8")); err != errOwn {
		t.Errorf("an error that already matches the sentinel was rewrapped: %v", err)
	}
	if netguard.Tag(netguard.NoMetadata, nil)(netip.MustParseAddr("169.254.169.254")).Error() !=
		netguard.NoMetadata(netip.MustParseAddr("169.254.169.254")).Error() {
		t.Error("a nil sentinel must leave the policy unchanged")
	}
}

func TestControlRefusesWhatThePolicyRefuses(t *testing.T) {
	control := netguard.Control(netguard.PublicOnly)
	for addr, blocked := range map[string]bool{
		"93.184.216.34:443":           false,
		"[2606:4700::1111]:443":       false,
		"127.0.0.1:80":                true,
		"[::ffff:169.254.169.254]:80": true,
		"example.com:80":              true,
		"10.0.0.5":                    true,
	} {
		err := control("tcp", addr, nil)
		if got := errors.Is(err, netguard.ErrBlocked); got != blocked {
			t.Errorf("Control(%q) = %v, blocked want %v", addr, err, blocked)
		}
	}
	if err := netguard.Control(nil)("tcp", "10.0.0.1:80", nil); !errors.Is(err, netguard.ErrBlocked) {
		t.Error("a nil policy must mean PublicOnly")
	}
}

func serve(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, c *http.Client, url string) (*http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err == nil {
		t.Cleanup(func() { _ = resp.Body.Close() })
	}
	return resp, err
}

// refused reports a connection the guard refused. A host without IPv6
// fails an IPv6 dial before the guard is asked, which is a refusal too.
func refused(err error) bool {
	return errors.Is(err, netguard.ErrBlocked) || errors.Is(err, syscall.EAFNOSUPPORT)
}

// loopback admits the test server and nothing else private.
var loopback = netguard.Except(netguard.PublicOnly, netip.MustParsePrefix("127.0.0.1/32"))

func TestClientRefusesLoopbackAndPrivateAddressesAtDialTime(t *testing.T) {
	var hits atomic.Int64
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, "internal admin page")
	})
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	c := netguard.Client(netguard.PublicOnly)
	for _, target := range []string{
		srv.URL + "/",                             // 127.0.0.1 literal
		"http://localhost:" + port + "/",          // a name that resolves to loopback
		"http://[::1]:" + port + "/",              // IPv6 loopback
		"http://[::ffff:127.0.0.1]:" + port + "/", // IPv4-mapped IPv6
		"http://0.0.0.0:" + port + "/",            // unspecified
		"http://127.1:" + port + "/",              // short-form loopback, resolved by the OS
		"http://2130706433:" + port + "/",         // decimal loopback
	} {
		_, err := get(t, c, target)
		if err == nil {
			t.Errorf("%s: fetched an address that is not public", target)
			continue
		}
		if !strings.Contains(target, "127.1") && !strings.Contains(target, "2130706433") && !refused(err) {
			t.Errorf("%s: err = %v, want ErrBlocked", target, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("the loopback server was reached %d times", hits.Load())
	}

	resp, err := get(t, netguard.Client(loopback), srv.URL+"/")
	if err != nil {
		t.Fatalf("an admitted address was refused: %v", err)
	}
	if body, _ := io.ReadAll(resp.Body); string(body) != "internal admin page" {
		t.Errorf("body %q", body)
	}
	resp, err = get(t, netguard.Client(netguard.NoMetadata), srv.URL+"/")
	if err != nil {
		t.Fatalf("NoMetadata refused loopback: %v", err)
	}
	_ = resp.Body.Close()
}

func TestClientRechecksEveryRedirectHop(t *testing.T) {
	var internalHits atomic.Int64
	internal := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		internalHits.Add(1)
		_, _ = io.WriteString(w, "secret")
	})
	port := internal.URL[strings.LastIndex(internal.URL, ":")+1:]
	for _, loc := range []string{
		"http://[::1]:" + port + "/",            // a literal the policy refuses
		"http://169.254.169.254/latest/",        // the metadata service
		"http://[::ffff:169.254.169.254]/",      // the metadata service, IPv4-mapped
		"http://localhost.localdomain.invalid/", // does not resolve; must not be reached either
	} {
		public := serve(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, loc, http.StatusFound) })
		// 127.0.0.1 is admitted so the first hop succeeds; the hop it
		// redirects to is what has to be refused.
		_, err := get(t, netguard.Client(loopback), public.URL)
		if err == nil {
			t.Errorf("redirect to %s was followed", loc)
			continue
		}
		if !strings.Contains(loc, ".invalid") && !refused(err) {
			t.Errorf("redirect to %s: err = %v, want ErrBlocked", loc, err)
		}
	}
	if internalHits.Load() != 0 {
		t.Fatal("a redirect reached the internal server")
	}
}

func TestClientRedirectBounds(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/page", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "page") })
	mux.HandleFunc("/to-page", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/page", http.StatusFound) })
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/loop", http.StatusFound) })
	mux.HandleFunc("/to-ftp", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "ftp://example.com/file", http.StatusFound)
	})
	srv := serve(t, mux.ServeHTTP)

	resp, err := get(t, netguard.Client(loopback), srv.URL+"/to-page")
	if err != nil {
		t.Fatal(err)
	}
	if body, _ := io.ReadAll(resp.Body); string(body) != "page" {
		t.Errorf("redirect not followed: %q", body)
	}

	if _, err := get(t, netguard.Client(loopback, netguard.WithMaxRedirects(3)), srv.URL+"/loop"); err == nil ||
		!strings.Contains(err.Error(), "stopped after 3 redirects") {
		t.Errorf("err = %v, want the chain stopped after 3", err)
	}
	if _, err := get(t, netguard.Client(loopback), srv.URL+"/loop"); err == nil ||
		!strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Errorf("err = %v, want the default bound of 10", err)
	}
	if _, err := get(t, netguard.Client(loopback), srv.URL+"/to-ftp"); err == nil ||
		!strings.Contains(err.Error(), `unsupported scheme "ftp"`) {
		t.Errorf("err = %v, want the ftp redirect refused", err)
	}

	resp, err = get(t, netguard.Client(loopback, netguard.WithMaxRedirects(0)), srv.URL+"/to-page")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/page" {
		t.Errorf("WithMaxRedirects(0) = %d to %q, want the redirect itself", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestTransportIgnoresProxyEnvironment(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	if tr := netguard.Transport(netguard.PublicOnly); tr.Proxy != nil {
		t.Fatal("the transport must not use a proxy")
	}
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "direct") })
	resp, err := get(t, netguard.Client(loopback), srv.URL)
	if err != nil {
		t.Fatalf("request went through the proxy: %v", err)
	}
	if body, _ := io.ReadAll(resp.Body); string(body) != "direct" {
		t.Errorf("body %q", body)
	}
}
