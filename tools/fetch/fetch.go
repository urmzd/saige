package fetch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/urmzd/saige/agent/types"
)

// Defaults for the fetch tool. Each can be changed with an Option.
const (
	// DefaultMaxBytes is the most response body read per call.
	DefaultMaxBytes = 2 << 20
	// DefaultMaxChars caps the text returned to the model.
	DefaultMaxChars = 20000
	// DefaultTimeout bounds one request, including redirects.
	DefaultTimeout = 30 * time.Second
)

type config struct {
	client       *http.Client
	allowPrivate bool
	maxBytes     int64
	maxChars     int
	timeout      time.Duration
	userAgent    string
}

// Option configures NewTool.
type Option func(*config)

// AllowPrivateNetworks lets the tool reach loopback and private addresses.
// Cloud metadata and link-local addresses stay blocked. Use it only when the
// agent is meant to read services on the local network.
func AllowPrivateNetworks() Option { return func(c *config) { c.allowPrivate = true } }

// WithHTTPClient replaces the safe client. The caller then owns address
// filtering.
func WithHTTPClient(client *http.Client) Option { return func(c *config) { c.client = client } }

// WithMaxBytes sets the most response body read per call.
func WithMaxBytes(n int64) Option {
	return func(c *config) {
		if n > 0 {
			c.maxBytes = n
		}
	}
}

// WithMaxChars sets the default cap on returned text.
func WithMaxChars(n int) Option {
	return func(c *config) {
		if n > 0 {
			c.maxChars = n
		}
	}
}

// WithTimeout bounds each request.
func WithTimeout(d time.Duration) Option {
	return func(c *config) {
		if d > 0 {
			c.timeout = d
		}
	}
}

// WithUserAgent sets the User-Agent header.
func WithUserAgent(ua string) Option { return func(c *config) { c.userAgent = ua } }

// NewTool returns the fetch tool. By default it uses SafeHTTPClient with
// private addresses blocked.
func NewTool(opts ...Option) types.Tool {
	cfg := &config{
		maxBytes:  DefaultMaxBytes,
		maxChars:  DefaultMaxChars,
		timeout:   DefaultTimeout,
		userAgent: "saige-fetch/1",
	}
	for _, o := range opts {
		o(cfg)
	}
	if cfg.client == nil {
		cfg.client = SafeHTTPClient(!cfg.allowPrivate)
	}
	return &tool{cfg}
}

type tool struct{ cfg *config }

func (t *tool) Definition() types.ToolDef {
	return types.ToolDef{
		Name:       "fetch",
		Capability: types.ToolCapabilityRead,
		Description: "Fetch an http or https URL and return its content as text. HTML is converted to plain text; " +
			"JSON, XML, and other text types are returned as is. Private and local network addresses are refused.",
		Parameters: types.ParameterSchema{
			Type:     types.SchemaObject,
			Required: []string{"url"},
			Properties: map[string]types.PropertyDef{
				"url":       {Type: types.SchemaString, Description: "The absolute http or https URL to fetch."},
				"max_chars": {Type: types.SchemaInteger, Description: fmt.Sprintf("Maximum characters of text to return (default %d).", t.cfg.maxChars)},
			},
		},
	}
}

func (t *tool) Execute(ctx context.Context, args map[string]any) (string, error) {
	raw, _ := args["url"].(string)
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("fetch: url must be an absolute http or https URL, got %q", raw)
	}
	if u.User != nil {
		return "", fmt.Errorf("fetch: URLs with credentials are not allowed")
	}
	maxChars := t.cfg.maxChars
	switch v := args["max_chars"].(type) {
	case float64:
		if v >= 1 {
			maxChars = int(v)
		}
	case json.Number:
		if n, err := v.Int64(); err == nil && n >= 1 {
			maxChars = int(n)
		}
	}

	ctx, cancel := context.WithTimeout(ctx, t.cfg.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", fmt.Errorf("fetch: %w", err)
	}
	req.Header.Set("User-Agent", t.cfg.userAgent)
	req.Header.Set("Accept", "text/html, text/plain, application/json, application/xml;q=0.9, */*;q=0.1")

	resp, err := t.cfg.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if !isText(mediaType) {
		return "", fmt.Errorf("fetch: %s returned %q, which is not text", resp.Request.URL, mediaType)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, t.cfg.maxBytes+1))
	if err != nil {
		return "", fmt.Errorf("fetch: read body: %w", err)
	}
	clipped := int64(len(body)) > t.cfg.maxBytes
	if clipped {
		body = body[:t.cfg.maxBytes]
	}

	text := string(body)
	if mediaType == "text/html" || mediaType == "application/xhtml+xml" {
		text = htmlToText(text)
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("fetch: %s returned status %d: %s", resp.Request.URL, resp.StatusCode, clip(text, 500))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "URL: %s\nStatus: %d\nContent-Type: %s\n\n", resp.Request.URL, resp.StatusCode, mediaType)
	b.WriteString(clip(text, maxChars))
	if clipped {
		fmt.Fprintf(&b, "\n... (body exceeded %d bytes and was cut)", t.cfg.maxBytes)
	}
	return b.String(), nil
}

// isText reports whether a media type can be returned as text.
func isText(mediaType string) bool {
	switch {
	case mediaType == "", strings.HasPrefix(mediaType, "text/"):
		return true
	case strings.HasSuffix(mediaType, "+json"), strings.HasSuffix(mediaType, "+xml"):
		return true
	}
	switch mediaType {
	case "application/json", "application/xml", "application/javascript", "application/x-ndjson", "application/yaml", "application/x-yaml":
		return true
	}
	return false
}

// clip shortens s to at most n runes with a note.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + fmt.Sprintf("\n... (%d more characters; raise max_chars to read more)", len(r)-n)
}

// tagTitle is the HTML element whose text becomes the page title.
const tagTitle = "title"

// skipElements hold no readable text.
var skipElements = map[string]bool{"script": true, "style": true, "noscript": true, "template": true, "svg": true}

// blockElements start a new line.
var blockElements = map[string]bool{
	"p": true, "div": true, "br": true, "li": true, "tr": true, "h1": true, "h2": true, "h3": true,
	"h4": true, "h5": true, "h6": true, "pre": true, "blockquote": true, "section": true, "article": true,
	"header": true, "footer": true, "table": true, "ul": true, "ol": true, "hr": true, tagTitle: true,
}

// htmlToText extracts readable text from HTML: scripts and styles are
// dropped, block elements become line breaks, and runs of spaces collapse.
func htmlToText(doc string) string {
	z := html.NewTokenizer(strings.NewReader(doc))
	var b strings.Builder
	skip := 0
	title := ""
	inTitle := false
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			out := collapse(b.String())
			if title != "" && !strings.HasPrefix(out, title) {
				out = title + "\n\n" + out
			}
			return out
		case html.StartTagToken, html.SelfClosingTagToken:
			name, _ := z.TagName()
			tag := string(name)
			if tag == tagTitle {
				inTitle = true
			}
			if skipElements[tag] && tt == html.StartTagToken {
				skip++
			}
			if blockElements[tag] {
				b.WriteByte('\n')
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			tag := string(name)
			if tag == tagTitle {
				inTitle = false
			}
			if skipElements[tag] && skip > 0 {
				skip--
			}
			if blockElements[tag] {
				b.WriteByte('\n')
			}
		case html.TextToken:
			text := string(z.Text())
			if inTitle {
				title = strings.TrimSpace(text)
				continue
			}
			if skip == 0 {
				b.WriteString(text)
			}
		}
	}
}

// collapse trims each line, collapses inner whitespace, and keeps at most
// one blank line in a row.
func collapse(s string) string {
	var out []string
	blank := false
	for _, line := range strings.Split(s, "\n") {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			if !blank && len(out) > 0 {
				out = append(out, "")
			}
			blank = true
			continue
		}
		blank = false
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
