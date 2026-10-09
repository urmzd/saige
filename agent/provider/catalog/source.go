package catalog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/urmzd/saige/agent/registry"
)

// Source produces one catalog layer from wherever it is kept: the embedded
// default, a file, an HTTP endpoint, an object store, or a database. Load
// returns the layer as written; LoadSource and Layered validate the result.
//
// An object store needs no SDK in this package: wrap its reader in
// ReaderSource.
type Source interface {
	Load(ctx context.Context) (*Catalog, error)
}

// sourceName names a source in errors. Sources may implement fmt.Stringer.
func sourceName(s Source) string {
	if n, ok := s.(fmt.Stringer); ok {
		return n.String()
	}
	return fmt.Sprintf("%T", s)
}

// loadFrom calls s.Load and wraps a failure with the source's name.
func loadFrom(ctx context.Context, s Source) (*Catalog, error) {
	if s == nil {
		return nil, errors.New("catalog: nil source")
	}
	c, err := s.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("catalog source %s: %w", sourceName(s), err)
	}
	if c == nil {
		return nil, fmt.Errorf("catalog source %s: returned no catalog", sourceName(s))
	}
	return c, nil
}

// LoadSource loads a source and validates the result as a complete catalog.
// A lone overlay is not complete; layer it over EmbeddedSource with Layered.
func LoadSource(ctx context.Context, src Source) (*Catalog, error) {
	c, err := loadFrom(ctx, src)
	if err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("catalog source %s: %w", sourceName(src), err)
	}
	return c, nil
}

// Use loads src, validates it and installs it, so Lookup and every adapter
// resolve against it. It is the one call a host makes to replace the
// embedded catalog.
func Use(ctx context.Context, src Source) (InstallReport, error) {
	c, err := LoadSource(ctx, src)
	if err != nil {
		return InstallReport{}, err
	}
	return Install(c, sourceOption(src)...)
}

// Refresh reloads src and installs it when it differs from the active
// catalog. changed is false when the content is the same, so a host can
// call it on a timer; an HTTPSource answers an unchanged poll with a
// conditional request.
func Refresh(ctx context.Context, src Source) (rep InstallReport, changed bool, err error) {
	c, err := LoadSource(ctx, src)
	if err != nil {
		return InstallReport{}, false, err
	}
	next, err := c.MarshalJSON()
	if err != nil {
		return InstallReport{}, false, err
	}
	cur, err := Active().MarshalJSON()
	if err != nil {
		return InstallReport{}, false, err
	}
	if bytes.Equal(next, cur) {
		return InstallReport{}, false, nil
	}
	rep, err = Install(c, sourceOption(src)...)
	return rep, err == nil, err
}

// ── Built-in sources ────────────────────────────────────────────────

type embeddedSource struct{}

// EmbeddedSource is the catalog this package ships.
func EmbeddedSource() Source { return embeddedSource{} }

func (embeddedSource) Load(context.Context) (*Catalog, error) { return Default(), nil }
func (embeddedSource) String() string                         { return defaultSource }

type staticSource struct{ c *Catalog }

// StaticSource serves a catalog value the host already holds. Each Load
// returns a copy.
func StaticSource(c *Catalog) Source { return staticSource{c: c} }

func (s staticSource) Load(context.Context) (*Catalog, error) {
	if s.c == nil {
		return nil, errors.New("no catalog")
	}
	return s.c.clone(), nil
}
func (staticSource) String() string { return "static" }

type fileSource struct{ path string }

// FileSource reads a catalog layer from a file. A "file://" URL is accepted.
func FileSource(path string) Source {
	return fileSource{path: strings.TrimPrefix(path, "file://")}
}

func (s fileSource) Load(context.Context) (*Catalog, error) { return LoadFile(s.path) }
func (s fileSource) String() string                         { return s.path }

// Path returns the file path, for tools that report layers.
func (s fileSource) Path() string { return s.path }

type readerSource struct {
	name string
	open func(context.Context) (io.ReadCloser, error)
}

// ReaderSource adapts any reader to a Source: an object-store client, a
// database blob, or a test fixture. open is called on every Load and its
// reader is closed afterwards.
func ReaderSource(open func(ctx context.Context) (io.ReadCloser, error)) Source {
	return readerSource{name: "reader", open: open}
}

// Named gives a source a name for errors and layer listings.
func Named(name string, s Source) Source { return namedSource{name: name, Source: s} }

type namedSource struct {
	name string
	Source
}

func (n namedSource) String() string { return n.name }

func (s readerSource) Load(ctx context.Context) (*Catalog, error) {
	if s.open == nil {
		return nil, errors.New("no reader")
	}
	rc, err := s.open(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return load(rc, s.name)
}
func (s readerSource) String() string { return s.name }

// HTTPOptions configures an HTTPSource.
type HTTPOptions struct {
	// Client sends the request. Nil uses a client with Timeout.
	Client *http.Client
	// Timeout bounds one fetch. Zero means 10 seconds.
	Timeout time.Duration
	// MaxBytes caps the response body. Zero means 4 MiB.
	MaxBytes int64
	// AllowInsecure permits plain http URLs. Leave it off: a catalog names
	// endpoints and credential variables, so it must not be tampered with
	// in transit.
	AllowInsecure bool
	// Header is added to every request, for example an authorization
	// header.
	Header http.Header
}

// HTTPSource fetches a catalog layer over HTTPS. It remembers the ETag and
// body of the last response and sends If-None-Match, so an unchanged catalog
// costs a 304 and is parsed again from the body it already holds.
//
// The source names itself, in errors and in installed revisions, by the URL
// with its user information and query string redacted, since either can
// carry a credential.
func HTTPSource(rawURL string, opts HTTPOptions) Source {
	return &httpSource{url: rawURL, name: RedactURL(rawURL), opts: opts}
}

type httpSource struct {
	url  string
	name string
	opts HTTPOptions

	mu   sync.Mutex
	etag string
	body []byte
}

func (s *httpSource) String() string { return s.name }

// RedactURL returns rawURL with any user information and query string
// replaced by "redacted", for errors, logs and layer listings. A reference
// that does not parse as a URL is replaced whole.
func RedactURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "<unparseable url>"
	}
	if u.User != nil {
		u.User = url.User("redacted")
	}
	if u.RawQuery != "" || u.ForceQuery {
		u.RawQuery, u.ForceQuery = "redacted", false
	}
	u.Fragment, u.RawFragment = "", ""
	return u.String()
}

// redactErr replaces the URL a *url.Error carries with its redacted form.
func (s *httpSource) redactErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return &url.Error{Op: ue.Op, URL: RedactURL(ue.URL), Err: ue.Err}
	}
	return err
}

const (
	defaultHTTPTimeout  = 10 * time.Second
	defaultHTTPMaxBytes = 4 << 20
)

func (s *httpSource) Load(ctx context.Context) (*Catalog, error) {
	u, err := url.Parse(s.url)
	if err != nil {
		return nil, errors.New("invalid catalog URL")
	}
	if u.Scheme != "https" && (u.Scheme != "http" || !s.opts.AllowInsecure) {
		return nil, fmt.Errorf("scheme %q not allowed: catalogs are fetched over https unless AllowInsecure is set", u.Scheme)
	}
	timeout := s.opts.Timeout
	if timeout <= 0 {
		timeout = defaultHTTPTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return nil, s.redactErr(err)
	}
	for k, vs := range s.opts.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	s.mu.Lock()
	etag, last := s.etag, s.body
	s.mu.Unlock()
	if etag != "" && last != nil {
		req.Header.Set("If-None-Match", etag)
	}
	client := s.opts.Client
	if client == nil {
		client = &http.Client{}
	}
	if !s.opts.AllowInsecure {
		c := *client
		c.CheckRedirect = func(r *http.Request, via []*http.Request) error {
			if r.URL.Scheme != "https" {
				return fmt.Errorf("redirect to %s refused: not https", r.URL.Scheme)
			}
			if len(via) >= 10 {
				return errors.New("too many redirects")
			}
			return nil
		}
		client = &c
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, s.redactErr(err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotModified && last != nil:
		// Parse the cached body again rather than copying a parsed value:
		// the raw rows carry the merge-patch nulls a copy would lose.
		return load(bytes.NewReader(last), s.name)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	limit := s.opts.MaxBytes
	if limit <= 0 {
		limit = defaultHTTPMaxBytes
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	c, err := load(bytes.NewReader(body), s.name)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.etag, s.body = resp.Header.Get("ETag"), body
	s.mu.Unlock()
	return c, nil
}

// Layered merges its sources in order, lowest first, with Merge semantics,
// and validates the result. The first source is usually EmbeddedSource.
func Layered(sources ...Source) Source { return layeredSource(sources) }

type layeredSource []Source

func (l layeredSource) String() string {
	names := make([]string, len(l))
	for i, s := range l {
		names[i] = sourceName(s)
	}
	return "layered(" + strings.Join(names, ", ") + ")"
}

func (l layeredSource) Load(ctx context.Context) (*Catalog, error) {
	if len(l) == 0 {
		return nil, errors.New("no layers")
	}
	layers := make([]*Catalog, 0, len(l))
	for _, s := range l {
		c, err := loadFrom(ctx, s)
		if err != nil {
			return nil, err
		}
		layers = append(layers, c)
	}
	return Merge(&Catalog{Version: SchemaVersion}, layers...)
}

// ParseSource turns a reference into a Source: an https:// or http:// URL
// (http only with allowInsecure), a file:// URL, or a file path.
func ParseSource(ref string, allowInsecure bool) (Source, error) {
	switch {
	case strings.HasPrefix(ref, "https://"), strings.HasPrefix(ref, "http://"):
		if strings.HasPrefix(ref, "http://") && !allowInsecure {
			return nil, fmt.Errorf("catalog %s: plain http is not allowed; use https", RedactURL(ref))
		}
		return HTTPSource(ref, HTTPOptions{AllowInsecure: allowInsecure}), nil
	case strings.HasPrefix(ref, "file://"), !strings.Contains(ref, "://"):
		return FileSource(ref), nil
	}
	return nil, fmt.Errorf("catalog %s: unsupported scheme; use a path, file:// or https://", RedactURL(ref))
}

// sourceOption records where installed revisions came from.
func sourceOption(src Source) []registryOption {
	return []registryOption{withSource(sourceName(src))}
}

// FileExists reports whether a file source's file exists, for discovery
// that skips optional layers.
func FileExists(path string) bool {
	_, err := os.Stat(strings.TrimPrefix(path, "file://"))
	return err == nil
}

type registryOption = registry.Option

func withSource(s string) registry.Option { return registry.WithSource(s) }
