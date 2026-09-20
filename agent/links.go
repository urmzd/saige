package agent

// LinkPolicy resolves the directed handoff graph at construction. Delegation
// has a fixed call/return link and does not use this graph. Policies must not
// invent agent names. The group validates every returned endpoint.
type LinkPolicy interface {
	Resolve(map[string][]string) (map[string][]string, error)
}

// DirectReturnLinks adds the reverse of each declared handoff link. This is the
// default: a specialist can send control back to any agent that can send work
// to it, including when its declared outbound links were restricted.
type DirectReturnLinks struct{}

func (DirectReturnLinks) Resolve(links map[string][]string) (map[string][]string, error) {
	out := cloneLinks(links)
	for from, targets := range links {
		for _, to := range targets {
			found := false
			for _, target := range out[to] {
				if target == from {
					found = true
					break
				}
			}
			if !found {
				out[to] = append(out[to], from)
			}
		}
	}
	return out, nil
}

// DirectedLinks preserves exactly the declared graph. Use it when a return
// edge would violate the deployment's control or trust boundary.
type DirectedLinks struct{}

func (DirectedLinks) Resolve(links map[string][]string) (map[string][]string, error) {
	return cloneLinks(links), nil
}

func cloneLinks(links map[string][]string) map[string][]string {
	out := make(map[string][]string, len(links))
	for name, targets := range links {
		out[name] = append([]string(nil), targets...)
	}
	return out
}

func WithLinkPolicy(policy LinkPolicy) AgentOption {
	return func(cfg *AgentConfig) { cfg.LinkPolicy = policy }
}
