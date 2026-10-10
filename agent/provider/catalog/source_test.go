package catalog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSource serves a fixed document and counts loads.
type fakeSource struct {
	doc   string
	err   error
	loads int
}

func (f *fakeSource) Load(context.Context) (*Catalog, error) {
	f.loads++
	if f.err != nil {
		return nil, f.err
	}
	return Load(strings.NewReader(f.doc))
}

func (f *fakeSource) String() string { return "fake" }

func TestLayeredMergeOrder(t *testing.T) {
	low := &fakeSource{doc: `{"version":1,"revision":"low","models":[{"provider":"openai","prefix":"gpt-4o","tier":"economy"}]}`}
	high := &fakeSource{doc: `{"version":1,"revision":"high","models":[{"provider":"openai","prefix":"gpt-4o","tier":"frontier"}]}`}
	c, err := LoadSource(context.Background(), Layered(EmbeddedSource(), low, high))
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := c.Describe("openai", "gpt-4o"); e.Tier != TierFrontier {
		t.Fatalf("the later layer must win: %s", e.Tier)
	}
	if !strings.HasSuffix(c.Revision, "+low+high") {
		t.Fatalf("revision %q", c.Revision)
	}
}

func TestFailingSourceNamesItself(t *testing.T) {
	boom := errors.New("bucket unreachable")
	_, err := LoadSource(context.Background(), Layered(EmbeddedSource(), &fakeSource{err: boom}))
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "catalog source fake") {
		t.Fatalf("got %v", err)
	}
	_, err = LoadSource(context.Background(), Named("s3://team/catalog.json", ReaderSource(func(context.Context) (io.ReadCloser, error) {
		return nil, boom
	})))
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "s3://team/catalog.json") {
		t.Fatalf("got %v", err)
	}
}

func TestReaderSourceClosesReader(t *testing.T) {
	var closed atomic.Bool
	src := ReaderSource(func(context.Context) (io.ReadCloser, error) {
		return closer{Reader: strings.NewReader(`{"version":1}`), closed: &closed}, nil
	})
	if _, err := LoadSource(context.Background(), Layered(EmbeddedSource(), src)); err != nil {
		t.Fatal(err)
	}
	if !closed.Load() {
		t.Fatal("reader left open")
	}
}

type closer struct {
	io.Reader
	closed *atomic.Bool
}

func (c closer) Close() error { c.closed.Store(true); return nil }

func TestHTTPSource(t *testing.T) {
	var hits, notModified atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("If-None-Match") == `"v1"` {
			notModified.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = io.WriteString(w, `{"version":1,"revision":"remote"}`)
	}))
	defer srv.Close()
	src := HTTPSource(srv.URL, HTTPOptions{Client: srv.Client()})
	for range 2 {
		c, err := src.Load(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if c.Revision != "remote" {
			t.Fatalf("revision %q", c.Revision)
		}
	}
	if hits.Load() != 2 || notModified.Load() != 1 {
		t.Fatalf("hits %d, 304s %d", hits.Load(), notModified.Load())
	}

	small := HTTPSource(srv.URL, HTTPOptions{Client: srv.Client(), MaxBytes: 8})
	if _, err := small.Load(context.Background()); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("size limit: %v", err)
	}

	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"version":1}`)
	}))
	defer plain.Close()
	if _, err := HTTPSource(plain.URL, HTTPOptions{}).Load(context.Background()); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("plain http accepted: %v", err)
	}
	if _, err := HTTPSource(plain.URL, HTTPOptions{AllowInsecure: true}).Load(context.Background()); err != nil {
		t.Fatalf("explicit insecure: %v", err)
	}
	if _, err := ParseSource(plain.URL, false); err == nil {
		t.Fatal("ParseSource accepted http")
	}
}

func TestUseAndRefresh(t *testing.T) {
	t.Cleanup(func() { _, _ = Install(Default()) })
	src := &fakeSource{doc: `{"version":1,"revision":"r1","models":[{"provider":"openai","prefix":"use-test","extends":"openai.chat"}]}`}
	layered := Layered(EmbeddedSource(), src)
	if _, err := Use(context.Background(), layered); err != nil {
		t.Fatal(err)
	}
	if _, ok := Describe("openai", "use-test"); !ok {
		t.Fatal("Use did not install")
	}
	if _, changed, err := Refresh(context.Background(), layered); err != nil || changed {
		t.Fatalf("unchanged refresh: %v %v", changed, err)
	}
	src.doc = strings.Replace(src.doc, "r1", "r2", 1)
	if _, changed, err := Refresh(context.Background(), layered); err != nil || !changed {
		t.Fatalf("changed refresh: %v %v", changed, err)
	}
	if h := History("openai", "use-test"); len(h) == 0 || h[len(h)-1].Source != "layered(catalog/default.json, fake)" {
		t.Fatalf("history %+v", h)
	}
}

func TestStaticSourceCopies(t *testing.T) {
	c := Default()
	got, err := StaticSource(c).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got.Revision = "mutated"
	if c.Revision == "mutated" {
		t.Fatal("static source aliases its catalog")
	}
}

// nullOverlay deletes a row field and a template with merge-patch nulls.
const nullOverlay = `{"version":1,"revision":"nulls","templates":{"embedding":null},
	"models":[{"provider":"openai","prefix":"gpt-4.1","tier":null}]}`

// checkNullsApplied merges overlay onto the embedded catalog and checks that
// both nulls deleted what they name.
func checkNullsApplied(t *testing.T, label string, overlay *Catalog) {
	t.Helper()
	merged, err := merge(Default(), overlay)
	if err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	if _, ok := merged.OfferingTemplates["embedding"]; ok {
		t.Fatalf("%s: the null template survived", label)
	}
	if e, _ := merged.Describe("openai", "gpt-4.1"); e.Tier != "" {
		t.Fatalf("%s: the null tier became %q", label, e.Tier)
	}
}

func TestHTTPSourceKeepsNullsOnNotModified(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = io.WriteString(w, nullOverlay)
	}))
	defer srv.Close()
	src := HTTPSource(srv.URL, HTTPOptions{Client: srv.Client()})
	for _, label := range []string{"200", "304"} {
		c, err := src.Load(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		checkNullsApplied(t, label, c)
	}
}

func TestStaticSourceAndCloneKeepNulls(t *testing.T) {
	c, err := Load(strings.NewReader(nullOverlay))
	if err != nil {
		t.Fatal(err)
	}
	got, err := StaticSource(c).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	checkNullsApplied(t, "static", got)
	checkNullsApplied(t, "clone", c.Clone())
}

func TestPatchKeepsNumberPrecision(t *testing.T) {
	// 2^53 + 1 is not representable as a float64.
	c, err := Load(strings.NewReader(`{"version":1,"models":[{"provider":"openai","prefix":"gpt-4.1","defaults":{"seed":9007199254740993}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	merged, err := merge(Default(), c)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := merged.Describe("openai", "gpt-4.1")
	if e.Defaults == nil || e.Defaults.Seed == nil || *e.Defaults.Seed != 9007199254740993 {
		t.Fatalf("seed %+v", e.Defaults)
	}
}

func TestHTTPSourceRedactsCredentials(t *testing.T) {
	ref := "https://alice:hunter2@127.0.0.1:1/catalog.json?token=s3cret"
	src := HTTPSource(ref, HTTPOptions{Timeout: time.Second})
	name := fmt.Sprint(src)
	for _, secret := range []string{"alice", "hunter2", "s3cret"} {
		if strings.Contains(name, secret) {
			t.Fatalf("name %q leaks %q", name, secret)
		}
	}
	if !strings.Contains(name, "127.0.0.1:1/catalog.json") {
		t.Fatalf("name %q lost the host and path", name)
	}
	_, err := LoadSource(context.Background(), src)
	if err == nil {
		t.Fatal("want a connection error")
	}
	for _, secret := range []string{"alice", "hunter2", "s3cret"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error %q leaks %q", err, secret)
		}
	}
	if _, err := ParseSource("ftp://bob:pw@host/c.json?key=k", false); err == nil || strings.Contains(err.Error(), "pw") || strings.Contains(err.Error(), "key=k") {
		t.Fatalf("ParseSource error %v", err)
	}
}

func TestHTTPSourceRecordsRedactedSource(t *testing.T) {
	t.Cleanup(func() { _, _ = Install(Default()) })
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"version":1,"revision":"r","models":[{"provider":"openai","prefix":"redact-test","extends":"openai.chat"}]}`)
	}))
	defer srv.Close()
	ref := strings.Replace(srv.URL, "https://", "https://user:pass@", 1) + "/c.json?sig=abc"
	if _, err := Use(context.Background(), Layered(EmbeddedSource(), HTTPSource(ref, HTTPOptions{Client: srv.Client()}))); err != nil {
		t.Fatal(err)
	}
	h := History("openai", "redact-test")
	if len(h) == 0 {
		t.Fatal("row not installed")
	}
	if src := h[len(h)-1].Source; strings.Contains(src, "pass") || strings.Contains(src, "sig=abc") {
		t.Fatalf("installed source %q leaks a credential", src)
	}
}
