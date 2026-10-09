package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// CatalogTool is one tool exactly as the server advertised it, before any
// prefixing, filtering or schema conversion.
type CatalogTool struct {
	Name        string                  `json:"name"`
	Description string                  `json:"description,omitempty"`
	InputSchema json.RawMessage         `json:"inputSchema,omitempty"`
	Annotations *mcpsdk.ToolAnnotations `json:"annotations,omitempty"`
	// Fingerprint identifies the name, description and schema together. It
	// changes when a server rewrites a description under an unchanged name,
	// which a name-only comparison misses.
	Fingerprint string `json:"fingerprint"`
}

func (t CatalogTool) readOnly() bool { return t.Annotations != nil && t.Annotations.ReadOnlyHint }

func (t CatalogTool) idempotent() bool {
	return t.Annotations != nil && t.Annotations.IdempotentHint
}

// Catalog is a server's advertised tool list at one point in time. It is the
// unfiltered list: AllowedTools applies when tools are imported, not here.
type Catalog struct {
	Server     string        `json:"server"`
	Tools      []CatalogTool `json:"tools"`
	CapturedAt time.Time     `json:"captured_at"`
}

// Fingerprints maps each tool name to its fingerprint, the compact form to
// record next to a result so a score change can be traced to a toolset change.
func (c Catalog) Fingerprints() map[string]string {
	out := make(map[string]string, len(c.Tools))
	for _, t := range c.Tools {
		out[t.Name] = t.Fingerprint
	}
	return out
}

// Fingerprint returns the SHA-256 of the canonical JSON of a tool's name,
// description and input schema. Key order and whitespace in the schema do not
// affect it.
func Fingerprint(name, description string, schema json.RawMessage) string {
	var canonical any
	if len(schema) > 0 {
		if err := json.Unmarshal(schema, &canonical); err != nil {
			canonical = string(schema)
		}
	}
	// encoding/json writes map keys in sorted order, which is what makes the
	// re-marshaled schema canonical.
	raw, _ := json.Marshal(map[string]any{
		"name":        name,
		"description": description,
		"inputSchema": canonical,
	})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Catalog returns the server's tool list. It is fetched once and cached until
// the server sends tools/list_changed or the session is replaced. The returned
// value is a copy.
func (c *Client) Catalog(ctx context.Context) (Catalog, error) {
	c.mu.RLock()
	cached, gen := c.catalog, c.catalogGen
	c.mu.RUnlock()
	if cached != nil {
		return cached.clone(), nil
	}

	ctx, session, done, err := c.useSession(ctx)
	if err != nil {
		return Catalog{}, err
	}
	defer done()
	cat := Catalog{Server: c.spec.Name, CapturedAt: time.Now()}
	// The iterator follows pagination; a single ListTools call would silently
	// drop every tool after the first page.
	for t, err := range session.Tools(ctx, nil) {
		if err != nil {
			return Catalog{}, fmt.Errorf("mcp: list tools on %q: %w", c.spec.Name, err)
		}
		cat.Tools = append(cat.Tools, catalogTool(t))
	}
	sort.Slice(cat.Tools, func(i, j int) bool { return cat.Tools[i].Name < cat.Tools[j].Name })

	c.mu.Lock()
	// Keep the result only if the session it came from is still current; a
	// reconnect or change notice during the listing makes it stale.
	if c.session == session && c.catalogGen == gen {
		c.catalog = &cat
		c.byName = make(map[string]CatalogTool, len(cat.Tools))
		for _, t := range cat.Tools {
			c.byName[t.Name] = t
			// A listing that offers a withdrawn tool again restores it.
			delete(c.withdrawn, t.Name)
		}
	}
	c.mu.Unlock()
	return cat.clone(), nil
}

// InvalidateCatalog drops the cached tool list so the next Catalog or Tools
// call re-fetches it. Use it for servers that change their tools without
// sending tools/list_changed.
func (c *Client) InvalidateCatalog() {
	c.mu.Lock()
	c.invalidateLocked()
	c.mu.Unlock()
}

// invalidateLocked drops the cached catalog. The generation makes a listing
// already in flight discard its result instead of caching a stale list.
func (c *Client) invalidateLocked() {
	c.catalog = nil
	c.catalogGen++
}

func catalogTool(t *mcpsdk.Tool) CatalogTool {
	var schema json.RawMessage
	if t.InputSchema != nil {
		if raw, ok := t.InputSchema.(json.RawMessage); ok {
			schema = append(json.RawMessage(nil), raw...)
		} else if raw, err := json.Marshal(t.InputSchema); err == nil {
			schema = raw
		}
	}
	var ann *mcpsdk.ToolAnnotations
	if t.Annotations != nil {
		a := *t.Annotations
		ann = &a
	}
	return CatalogTool{
		Name:        t.Name,
		Description: t.Description,
		InputSchema: schema,
		Annotations: ann,
		Fingerprint: Fingerprint(t.Name, t.Description, schema),
	}
}

func (c Catalog) clone() Catalog {
	out := c
	out.Tools = make([]CatalogTool, len(c.Tools))
	for i, t := range c.Tools {
		t.InputSchema = append(json.RawMessage(nil), t.InputSchema...)
		if t.Annotations != nil {
			a := *t.Annotations
			t.Annotations = &a
		}
		out.Tools[i] = t
	}
	return out
}

// CatalogDrift is the difference between two catalogs of one server.
type CatalogDrift struct {
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
	// Changed lists tools present in both whose fingerprint differs.
	Changed []string `json:"changed,omitempty"`
}

// Empty reports whether the catalogs match.
func (d CatalogDrift) Empty() bool {
	return len(d.Added) == 0 && len(d.Removed) == 0 && len(d.Changed) == 0
}

// DiffCatalogs compares a baseline catalog with a newer one. Names in each
// list are sorted.
func DiffCatalogs(old, new Catalog) CatalogDrift {
	before := old.Fingerprints()
	after := new.Fingerprints()
	var d CatalogDrift
	for name, fp := range after {
		prev, ok := before[name]
		switch {
		case !ok:
			d.Added = append(d.Added, name)
		case prev != fp:
			d.Changed = append(d.Changed, name)
		}
	}
	for name := range before {
		if _, ok := after[name]; !ok {
			d.Removed = append(d.Removed, name)
		}
	}
	sort.Strings(d.Added)
	sort.Strings(d.Removed)
	sort.Strings(d.Changed)
	return d
}

// Within reports an error when more than maxDrop tools disappeared, or when
// any tool in mustKeep disappeared or changed. It is a pre-run check: an
// evaluation against a server that lost the tools it depends on measures the
// outage, not the agent.
func (d CatalogDrift) Within(maxDrop int, mustKeep ...string) error {
	if maxDrop >= 0 && len(d.Removed) > maxDrop {
		return fmt.Errorf("mcp: %d tools removed (%v), at most %d allowed", len(d.Removed), d.Removed, maxDrop)
	}
	gone := map[string]string{}
	for _, n := range d.Removed {
		gone[n] = "removed"
	}
	for _, n := range d.Changed {
		gone[n] = "changed"
	}
	for _, n := range mustKeep {
		if why, ok := gone[n]; ok {
			return fmt.Errorf("mcp: required tool %q was %s", n, why)
		}
	}
	return nil
}
