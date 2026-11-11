package definition

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	sdef "github.com/urmzd/saige/agent/definition"
	"github.com/urmzd/saige/durable"
)

// PinDomain is the hash domain of a Pin.
const PinDomain = "fiber/pin/v1"

// MaxTree bounds the definitions one resolution reads: the root and every
// sub-agent it reaches.
const MaxTree = 64

// Pin is the exact definition a run executes, fixed at accept time and
// carried in the run's durable input.
type Pin struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	// Digest is saige's resolved digest: the root's content and every
	// sub-agent it resolved to.
	Digest string `json:"digest"`
	// Subagents are the exact versions of every sub-agent the root reaches,
	// by name.
	Subagents []Version `json:"subagents,omitempty"`
	// PolicyHash is the root's Policy.Hash.
	PolicyHash string `json:"policy_hash"`
	// Hash is one hash over every field above, under PinDomain.
	Hash string `json:"hash"`
}

// Version names one exact version of one definition.
type Version struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// IsZero reports whether p pins nothing.
func (p Pin) IsZero() bool { return p.Name == "" }

// SubagentVersion returns the version p pinned for a sub-agent.
func (p Pin) SubagentVersion(name string) (string, bool) {
	for _, s := range p.Subagents {
		if s.Name == name {
			return s.Version, true
		}
	}
	return "", false
}

// hash computes Hash from the other fields.
func (p Pin) hash() string {
	body := struct {
		Name       string    `json:"name"`
		Version    string    `json:"version"`
		Digest     string    `json:"digest"`
		Subagents  []Version `json:"subagents"`
		PolicyHash string    `json:"policy_hash"`
	}{p.Name, p.Version, p.Digest, p.Subagents, p.PolicyHash}
	raw, err := json.Marshal(body)
	if err != nil {
		panic(fmt.Sprintf("definition: encode pin: %v", err))
	}
	h := sha256.New()
	h.Write([]byte(PinDomain))
	h.Write([]byte{0})
	h.Write(raw)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// Loaded is a verified pin and the agent it pins.
type Loaded struct {
	Pin   Pin
	Agent Agent
}

// Resolver turns names into pins and pins back into agents, over one
// Source. For each resolution it builds a saige Registry over exactly the
// tenant's definitions the root reaches, so saige's own reference and cycle
// checks and its resolved digest apply.
type Resolver struct {
	src    Source
	checks Checks
}

// NewResolver returns a Resolver over src. checks are the deployment facts
// every definition in a tree is validated against.
func NewResolver(src Source, checks Checks) *Resolver {
	return &Resolver{src: src, checks: checks}
}

// Source returns the source the resolver reads.
func (r *Resolver) Source() Source { return r.src }

// Resolve pins the latest version of name. A retired name is ErrRetired.
func (r *Resolver) Resolve(ctx context.Context, tenant, name string) (Pin, error) {
	root, err := r.src.Latest(ctx, tenant, NormalizeName(name))
	if err != nil {
		return Pin{}, err
	}
	return r.pin(ctx, tenant, root)
}

// ResolveAt pins one exact version of name. It is how a delegation runs the
// sub-agent version its parent pinned.
func (r *Resolver) ResolveAt(ctx context.Context, tenant, name, version string) (Pin, error) {
	root, err := r.src.Get(ctx, tenant, NormalizeName(name), version)
	if err != nil {
		return Pin{}, err
	}
	return r.pin(ctx, tenant, root)
}

func (r *Resolver) pin(ctx context.Context, tenant string, root Record) (Pin, error) {
	tree, err := r.tree(ctx, tenant, root)
	if err != nil {
		return Pin{}, err
	}
	res, err := resolve(root.Name, tree)
	if err != nil {
		return Pin{}, err
	}
	p := Pin{
		Name: root.Name, Version: root.Version, Digest: res.Digest,
		Subagents: subagentVersions(res), PolicyHash: root.Policy.Hash(),
	}
	p.Hash = p.hash()
	return p, nil
}

// tree picks the root and the highest live version of every sub-agent it
// reaches whose range admits it. One version per name: two references to
// one name that admit different versions are refused.
func (r *Resolver) tree(ctx context.Context, tenant string, root Record) ([]Agent, error) {
	picked := map[string]Record{root.Name: root}
	order := []Record{root}
	var agents []Agent
	for i := 0; i < len(order); i++ {
		rec := order[i]
		a, err := Parse(rec.Content, rec.Policy, r.checks)
		if err != nil {
			return nil, fmt.Errorf("%s@%s: %w", rec.Name, rec.Version, err)
		}
		if a.Definition.Digest != rec.Digest {
			return nil, fmt.Errorf("%w: %s@%s content hashes to %s, the record says %s", ErrPin, rec.Name, rec.Version, a.Definition.Digest, rec.Digest)
		}
		agents = append(agents, a)
		for _, s := range a.Definition.Subagents {
			ref, err := sdef.ParseRef(s.Ref)
			if err != nil {
				return nil, fmt.Errorf("%w: %s: %v", ErrInvalid, rec.Name, err)
			}
			rng, err := sdef.ParseRange(ref.Range)
			if err != nil {
				return nil, fmt.Errorf("%w: %s: %v", ErrInvalid, rec.Name, err)
			}
			if have, ok := picked[ref.Name]; ok {
				if v, err := sdef.ParseVersion(have.Version); err != nil || !rng.Matches(v) {
					return nil, fmt.Errorf("%w: %s asks for %s, but the tree already runs %s@%s", ErrInvalid, rec.Name, ref, have.Name, have.Version)
				}
				continue
			}
			if _, err := r.src.Latest(ctx, tenant, ref.Name); err != nil {
				return nil, fmt.Errorf("%s: sub-agent %s: %w", rec.Name, ref.Name, err)
			}
			versions, err := r.src.Versions(ctx, tenant, ref.Name)
			if err != nil {
				return nil, fmt.Errorf("%s: sub-agent %s: %w", rec.Name, ref.Name, err)
			}
			var pick *Record
			for j := len(versions) - 1; j >= 0; j-- {
				if v, err := sdef.ParseVersion(versions[j].Version); err == nil && rng.Matches(v) {
					pick = &versions[j]
					break
				}
			}
			if pick == nil {
				return nil, fmt.Errorf("%w: %s: no version of %s matches %q", ErrNotFound, rec.Name, ref.Name, ref.Range)
			}
			if len(order) >= MaxTree {
				return nil, fmt.Errorf("%w: %s reaches more than %d definitions", ErrInvalid, root.Name, MaxTree)
			}
			picked[ref.Name] = *pick
			order = append(order, *pick)
		}
	}
	return agents, nil
}

// resolve runs saige's registry over exactly the given definitions.
func resolve(root string, tree []Agent) (*sdef.Resolved, error) {
	defs := make([]*sdef.Definition, len(tree))
	for i, a := range tree {
		defs[i] = a.Definition
	}
	reg := sdef.NewRegistry(sdef.StaticSource(defs...), sdef.Checks{})
	if _, err := reg.Load(context.Background()); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	res, err := reg.Resolve(root)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return res, nil
}

// subagentVersions lists every sub-agent in a resolved tree, by name.
func subagentVersions(res *sdef.Resolved) []Version {
	seen := map[string]string{}
	var walk func(*sdef.Resolved)
	walk = func(n *sdef.Resolved) {
		for _, s := range n.Subagents {
			seen[s.Agent.Name] = s.Agent.Version
			walk(s.Agent)
		}
	}
	walk(res)
	out := make([]Version, 0, len(seen))
	for name, v := range seen {
		out = append(out, Version{Name: name, Version: v})
	}
	slices.SortFunc(out, func(a, b Version) int { return strings.Compare(a.Name, b.Name) })
	if len(out) == 0 {
		return nil
	}
	return out
}

// Verify reloads exactly what pin names and checks it still is what was
// pinned: the pin's own hash, the root and every sub-agent version, saige's
// resolved digest and the policy hash. A pin that does not hold, a missing
// row and an empty digest fail terminally (durable.NonRetryable). A store
// that cannot be reached fails retryably.
func (r *Resolver) Verify(ctx context.Context, tenant string, pin Pin) (Loaded, error) {
	fail := func(format string, args ...any) (Loaded, error) {
		return Loaded{}, durable.NonRetryable(fmt.Errorf("%w: %s@%s: %s", ErrPin, pin.Name, pin.Version, fmt.Sprintf(format, args...)))
	}
	switch {
	case pin.Name == "" || pin.Version == "":
		return fail("the pin names no definition")
	case pin.Digest == "":
		return fail("the pin has no digest")
	case pin.PolicyHash == "":
		return fail("the pin has no policy hash")
	case pin.Hash != pin.hash():
		return fail("the pin's hash does not match its fields")
	}
	get := func(name, version string) (Record, error) {
		rec, err := r.src.Get(ctx, tenant, name, version)
		if errors.Is(err, ErrNotFound) {
			return Record{}, durable.NonRetryable(fmt.Errorf("%w: %s@%s pinned %s@%s, which is gone: %w", ErrPin, pin.Name, pin.Version, name, version, err))
		}
		return rec, err
	}
	root, err := get(pin.Name, pin.Version)
	if err != nil {
		return Loaded{}, err
	}
	recs := []Record{root}
	for _, s := range pin.Subagents {
		rec, err := get(s.Name, s.Version)
		if err != nil {
			return Loaded{}, err
		}
		recs = append(recs, rec)
	}
	tree := make([]Agent, 0, len(recs))
	for _, rec := range recs {
		a, err := Parse(rec.Content, rec.Policy, r.checks)
		if err != nil {
			return fail("%s@%s no longer loads: %v", rec.Name, rec.Version, err)
		}
		if a.Definition.Digest != rec.Digest {
			return fail("%s@%s content changed under its version", rec.Name, rec.Version)
		}
		tree = append(tree, a)
	}
	if got := tree[0].Policy.Hash(); got != pin.PolicyHash || root.PolicyHash != pin.PolicyHash {
		return fail("the policy changed: it hashes to %s, the run pinned %s", got, pin.PolicyHash)
	}
	res, err := resolve(pin.Name, tree)
	if err != nil {
		return fail("%v", err)
	}
	if res.Digest != pin.Digest {
		return fail("the definition resolves to %s, the run pinned %s", res.Digest, pin.Digest)
	}
	if !slices.Equal(subagentVersions(res), pin.Subagents) {
		return fail("the sub-agents resolve to other versions than the run pinned")
	}
	return Loaded{Pin: pin, Agent: tree[0]}, nil
}
