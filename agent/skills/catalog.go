package skills

import (
	"context"
	"fmt"
	"slices"
	"sort"

	"github.com/urmzd/saige/agent/selector"
)

// SkillCatalog is the read side the skill tools use. owner is the agent
// asking; a catalog may scope its contents by it (per tenant, say), and may
// ignore it. Reachability is decided separately by a SkillPolicy.
type SkillCatalog interface {
	List(ctx context.Context, owner string) ([]SkillMeta, error)
	Load(ctx context.Context, owner, name string) (Skill, error)
	// ReadResource returns one manifest-listed file of a skill, at most max
	// bytes (max <= 0 means no limit).
	ReadResource(ctx context.Context, owner, name, path string, max int64) ([]byte, SkillResource, error)
	// Search returns up to k of the listed skills that best match query.
	Search(ctx context.Context, owner, query string, k int) ([]SkillMeta, error)
}

// Catalog is an immutable SkillCatalog built from ordered sources. When two
// sources define the same name, the first source wins and the later
// definition is recorded in Shadowed, with one exception: an untrusted
// definition never shadows a trusted one. A trusted definition from a later
// source replaces an untrusted one from an earlier source, so an untrusted
// project checkout cannot swap the content of a trusted skill an allow list
// admits by name. Rebuild a catalog to pick up changes on disk.
type Catalog struct {
	skills   []Skill // sorted by name
	shadowed []SkillMeta
	problems []error
	search   selector.Selector[SkillMeta]
}

// CatalogOption configures NewCatalog.
type CatalogOption func(*Catalog)

// WithSelector replaces the BM25 ranking used by Search.
func WithSelector(sel selector.Selector[SkillMeta]) CatalogOption {
	return func(c *Catalog) { c.search = sel }
}

// SkillMetaText is the searchable text BM25 uses for a skill by default.
func SkillMetaText(m SkillMeta) string { return m.Name + " " + m.Name + " " + m.Description }

// NewCatalog lists every source in order and snapshots the result.
//
// A source that fails, or a package that does not validate, is recorded in
// Problems and skipped; the rest of the catalog still loads. The returned
// error is non-nil only when ctx ends.
func NewCatalog(ctx context.Context, sources []SkillSource, opts ...CatalogOption) (*Catalog, error) {
	c := &Catalog{search: selector.NewBM25(SkillMetaText)}
	for _, o := range opts {
		o(c)
	}
	seen := map[string]int{} // name to index in c.skills
	for _, src := range sources {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		list, err := src.List(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			c.problems = append(c.problems, err)
		}
		for _, s := range list {
			i, ok := seen[s.Name]
			if !ok {
				seen[s.Name] = len(c.skills)
				c.skills = append(c.skills, s)
				continue
			}
			if s.Trusted && !c.skills[i].Trusted {
				c.shadowed = append(c.shadowed, c.skills[i].SkillMeta)
				c.skills[i] = s
				continue
			}
			c.shadowed = append(c.shadowed, s.SkillMeta)
		}
	}
	sort.Slice(c.skills, func(i, j int) bool { return c.skills[i].Name < c.skills[j].Name })
	return c, nil
}

// Problems returns the errors met while building the catalog.
func (c *Catalog) Problems() []error { return slices.Clone(c.problems) }

// Shadowed returns definitions hidden by another source's skill of the same
// name.
func (c *Catalog) Shadowed() []SkillMeta { return slices.Clone(c.shadowed) }

// List implements SkillCatalog. It returns every skill, sorted by name.
func (c *Catalog) List(context.Context, string) ([]SkillMeta, error) {
	out := make([]SkillMeta, len(c.skills))
	for i, s := range c.skills {
		out[i] = cloneMeta(s.SkillMeta)
	}
	return out, nil
}

// Load implements SkillCatalog.
func (c *Catalog) Load(_ context.Context, _ string, name string) (Skill, error) {
	s, ok := c.find(name)
	if !ok {
		return Skill{}, fmt.Errorf("%w: %s", ErrSkillNotFound, name)
	}
	s.SkillMeta = cloneMeta(s.SkillMeta)
	s.Resources = slices.Clone(s.Resources)
	return s, nil
}

// ReadResource implements SkillCatalog.
func (c *Catalog) ReadResource(_ context.Context, _ string, name, path string, max int64) ([]byte, SkillResource, error) {
	s, ok := c.find(name)
	if !ok {
		return nil, SkillResource{}, fmt.Errorf("%w: %s", ErrSkillNotFound, name)
	}
	return s.ReadResource(path, max)
}

// Search implements SkillCatalog.
func (c *Catalog) Search(ctx context.Context, owner, query string, k int) ([]SkillMeta, error) {
	all, err := c.List(ctx, owner)
	if err != nil {
		return nil, err
	}
	return c.search.Select(ctx, query, all, k)
}

func (c *Catalog) find(name string) (Skill, bool) {
	i, ok := slices.BinarySearchFunc(c.skills, name, func(s Skill, n string) int {
		switch {
		case s.Name < n:
			return -1
		case s.Name > n:
			return 1
		}
		return 0
	})
	if !ok {
		return Skill{}, false
	}
	return c.skills[i], true
}

func cloneMeta(m SkillMeta) SkillMeta {
	m.AllowedTools = slices.Clone(m.AllowedTools)
	if m.Metadata != nil {
		md := make(map[string]string, len(m.Metadata))
		for k, v := range m.Metadata {
			md[k] = v
		}
		m.Metadata = md
	}
	return m
}
