package definition

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"time"
)

// Source produces definitions from wherever they are kept. Load returns
// every definition the source holds, each parsed and validated on its own;
// references between them are checked by Registry. A source that cannot
// load one of its files fails as a whole, so a broken file is never
// silently skipped.
type Source interface {
	Load(ctx context.Context) ([]*Definition, error)
}

// Watcher is a Source that can say when it may have changed. Watch calls
// fn after each change until stop is called or ctx ends. A notification is
// a hint to load again, not a promise that anything changed.
type Watcher interface {
	Watch(ctx context.Context, fn func()) (stop func(), err error)
}

// sourceName names a source in errors. Sources may implement fmt.Stringer.
func sourceName(s Source) string {
	if n, ok := s.(fmt.Stringer); ok {
		return n.String()
	}
	return fmt.Sprintf("%T", s)
}

// MaxFileBytes bounds one definition file.
const MaxFileBytes = 1 << 20

// IsDefinitionFile reports whether a slash-separated path names a
// definition: a file ending in .agent.md anywhere, or a .md file directly
// inside a directory named agents.
func IsDefinitionFile(p string) bool {
	base := path.Base(p)
	if strings.HasSuffix(base, ".agent.md") {
		return true
	}
	return strings.HasSuffix(base, ".md") && path.Base(path.Dir(p)) == "agents"
}

// ── Directory and fs.FS ─────────────────────────────────────────────

type fsSource struct {
	name string
	fsys fs.FS
	root string
	// open, when set, opens the filesystem on each Load (a directory that
	// may not exist yet).
	open func() (fs.FS, func(), error)
}

// DirSource loads every definition file under dir, recursively (see
// IsDefinitionFile). The walk is confined to dir: a symbolic link that
// leaves it is refused. A missing directory holds no definitions.
func DirSource(dir string) Source {
	return &fsSource{name: dir, root: ".", open: func() (fs.FS, func(), error) {
		r, err := os.OpenRoot(dir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, nil, nil
			}
			return nil, nil, err
		}
		return r.FS(), func() { _ = r.Close() }, nil
	}}
}

// FSSource loads every definition file under root in fsys, for definitions
// compiled in with embed. name names the source in errors.
func FSSource(name string, fsys fs.FS, root string) Source {
	if root == "" {
		root = "."
	}
	return &fsSource{name: name, fsys: fsys, root: root}
}

func (s *fsSource) String() string { return s.name }

// Path returns the directory of a DirSource, for tools that list layers.
func (s *fsSource) Path() string { return s.name }

func (s *fsSource) Load(ctx context.Context) ([]*Definition, error) {
	fsys := s.fsys
	if s.open != nil {
		f, closeFS, err := s.open()
		if err != nil {
			return nil, err
		}
		if f == nil {
			return nil, nil
		}
		defer closeFS()
		fsys = f
	}
	// A root that is itself a directory named agents counts as one.
	rootIsAgents := path.Base(path.Clean(strings.ReplaceAll(s.name, "\\", "/"))) == "agents" && s.root == "."
	var defs []*Definition
	var errs []error
	err := fs.WalkDir(fsys, s.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		match := IsDefinitionFile(p) || rootIsAgents && path.Dir(p) == "." && strings.HasSuffix(p, ".md")
		if !match {
			return nil
		}
		if !d.Type().IsRegular() && d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		data, err := readBounded(fsys, p)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p, err))
			return nil
		}
		def, err := Parse(data, p)
		if err != nil {
			errs = append(errs, err)
			return nil
		}
		def.Source = s.name
		defs = append(defs, def)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("agent definitions %s: %w", s.name, err)
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("agent definitions %s: %w", s.name, errors.Join(errs...))
	}
	return defs, nil
}

func readBounded(fsys fs.FS, p string) ([]byte, error) {
	f, err := fsys.Open(p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxFileBytes {
		return nil, fmt.Errorf("file exceeds %d bytes", MaxFileBytes)
	}
	return data, nil
}

// ── Reader and static ───────────────────────────────────────────────

type readerSource struct {
	name string
	open func(context.Context) (io.ReadCloser, error)
}

// ReaderSource adapts any reader holding one definition file to a Source:
// an object-store client, a database blob or a test fixture. open is called
// on every Load and its reader is closed afterwards.
func ReaderSource(name string, open func(ctx context.Context) (io.ReadCloser, error)) Source {
	return readerSource{name: name, open: open}
}

func (s readerSource) String() string { return s.name }

func (s readerSource) Load(ctx context.Context) ([]*Definition, error) {
	if s.open == nil {
		return nil, errors.New("agent definitions: no reader")
	}
	rc, err := s.open(ctx)
	if err != nil {
		return nil, fmt.Errorf("agent definitions %s: %w", s.name, err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, MaxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("agent definitions %s: %w", s.name, err)
	}
	if len(data) > MaxFileBytes {
		return nil, fmt.Errorf("agent definitions %s: file exceeds %d bytes", s.name, MaxFileBytes)
	}
	def, err := Parse(data, s.name)
	if err != nil {
		return nil, err
	}
	def.Source = s.name
	return []*Definition{def}, nil
}

type staticSource struct{ defs []*Definition }

// StaticSource serves definitions the host already holds, such as ones it
// parsed itself.
func StaticSource(defs ...*Definition) Source { return staticSource{defs: defs} }

func (staticSource) String() string { return "static" }

func (s staticSource) Load(context.Context) ([]*Definition, error) { return slices.Clone(s.defs), nil }

// ── HTTP ────────────────────────────────────────────────────────────

// HTTPOptions configures an HTTPSource.
type HTTPOptions struct {
	// Client sends the request. Nil uses a client with Timeout.
	Client *http.Client
	// Timeout bounds one fetch. Zero means 10 seconds.
	Timeout time.Duration
	// AllowInsecure permits plain http URLs. Leave it off: a definition
	// names tools and approval rules, so it must not be tampered with in
	// transit.
	AllowInsecure bool
	// Header is added to every request, for example an authorization
	// header.
	Header http.Header
}

// HTTPSource fetches one definition file over HTTPS. It remembers the ETag
// and body of the last response and sends If-None-Match, so an unchanged
// definition costs a 304. The source names itself by the URL with its user
// information and query string redacted.
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
// replaced by "redacted".
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

func (s *httpSource) Load(ctx context.Context) ([]*Definition, error) {
	body, err := s.fetch(ctx)
	if err != nil {
		return nil, fmt.Errorf("agent definitions %s: %w", s.name, err)
	}
	def, err := Parse(body, s.name)
	if err != nil {
		return nil, err
	}
	def.Source = s.name
	return []*Definition{def}, nil
}

func (s *httpSource) fetch(ctx context.Context) ([]byte, error) {
	u, err := url.Parse(s.url)
	if err != nil {
		return nil, errors.New("invalid URL")
	}
	if u.Scheme != "https" && (u.Scheme != "http" || !s.opts.AllowInsecure) {
		return nil, fmt.Errorf("scheme %q not allowed: definitions are fetched over https unless AllowInsecure is set", u.Scheme)
	}
	timeout := s.opts.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return nil, errors.New("invalid request")
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
		var ue *url.Error
		if errors.As(err, &ue) {
			return nil, &url.Error{Op: ue.Op, URL: RedactURL(ue.URL), Err: ue.Err}
		}
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotModified && last != nil:
		return last, nil
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > MaxFileBytes {
		return nil, fmt.Errorf("response exceeds %d bytes", MaxFileBytes)
	}
	s.mu.Lock()
	s.etag, s.body = resp.Header.Get("ETag"), bytes.Clone(body)
	s.mu.Unlock()
	return body, nil
}

// ── Wrappers ────────────────────────────────────────────────────────

// Named gives a source a name for errors and listings.
func Named(name string, s Source) Source { return namedSource{name: name, Source: s} }

type namedSource struct {
	name string
	Source
}

func (n namedSource) String() string { return n.name }

func (n namedSource) Watch(ctx context.Context, fn func()) (func(), error) {
	return watch(ctx, n.Source, fn)
}

// Untrusted marks a source's definitions untrusted, as a project directory
// that arrived with a cloned repository should be. An untrusted definition
// may set only the fields UntrustedFields allows; any other field fails the
// load with its path. The allowlist admits nothing that connects to a
// server, loosens an approval, reads the user's memory, or runs code
// without asking.
func Untrusted(s Source) Source { return untrustedSource{Source: s} }

type untrustedSource struct{ Source }

func (u untrustedSource) String() string { return sourceName(u.Source) + " (untrusted)" }

func (u untrustedSource) Watch(ctx context.Context, fn func()) (func(), error) {
	return watch(ctx, u.Source, fn)
}

func (u untrustedSource) Load(ctx context.Context) ([]*Definition, error) {
	defs, err := u.Source.Load(ctx)
	if err != nil {
		return nil, err
	}
	var errs []error
	out := make([]*Definition, len(defs))
	for i, d := range defs {
		c := *d
		c.Trusted = false
		if found := untrustedIssues(&c); len(found) > 0 {
			errs = append(errs, found.asError(c.Location()))
		}
		out[i] = &c
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return out, nil
}

// watch forwards to s when it is a Watcher; otherwise it never fires.
func watch(ctx context.Context, s Source, fn func()) (func(), error) {
	if w, ok := s.(Watcher); ok {
		return w.Watch(ctx, fn)
	}
	return func() {}, nil
}

// Layered loads its sources in order, lowest first. A definition in a later
// source replaces one with the same name and version in an earlier one;
// other versions stay side by side, so a higher version in a later layer
// wins a range they both satisfy. Two definitions with the same name and
// version in one source are an error.
func Layered(sources ...Source) Source { return layeredSource(sources) }

type layeredSource []Source

func (l layeredSource) String() string {
	names := make([]string, len(l))
	for i, s := range l {
		names[i] = sourceName(s)
	}
	return "layered(" + strings.Join(names, ", ") + ")"
}

// Layers returns the sources, lowest first.
func (l layeredSource) Layers() []Source { return slices.Clone(l) }

func (l layeredSource) Load(ctx context.Context) ([]*Definition, error) {
	index := map[string]int{}
	var out []*Definition
	for _, s := range l {
		if s == nil {
			return nil, errors.New("agent definitions: nil source")
		}
		defs, err := s.Load(ctx)
		if err != nil {
			return nil, err
		}
		seen := map[string]*Definition{}
		for _, d := range defs {
			if prev, dup := seen[d.ID()]; dup {
				return nil, &ValidationError{Source: sourceName(s), Issues: []Issue{{Code: CodeDuplicate,
					Message: fmt.Sprintf("%s is defined twice: %s and %s", d.ID(), prev.Location(), d.Location())}}}
			}
			seen[d.ID()] = d
			if i, ok := index[d.ID()]; ok {
				out[i] = d
				continue
			}
			index[d.ID()] = len(out)
			out = append(out, d)
		}
	}
	return out, nil
}

// Watch fires when any layer that is a Watcher fires.
func (l layeredSource) Watch(ctx context.Context, fn func()) (func(), error) {
	var stops []func()
	stopAll := func() {
		for _, s := range stops {
			s()
		}
	}
	for _, s := range l {
		stop, err := watch(ctx, s, fn)
		if err != nil {
			stopAll()
			return nil, err
		}
		stops = append(stops, stop)
	}
	return stopAll, nil
}
