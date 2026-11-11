package github

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connector"
)

// readTarget permits explicit GitHub references from any connector, but only
// in operator-granted repositories or the run's original GitHub repository.
// URLs are parsed into API resource coordinates, never fetched as arbitrary URLs.
func (c *Connector) readTarget(source address.Address, reference string) (address.Address, error) {
	if reference == "" {
		if _, err := c.ref(source); err != nil {
			return address.Address{}, err
		}
		return source, nil
	}
	var target address.Address
	var err error
	if strings.HasPrefix(reference, "github://") {
		target, err = address.Parse(reference)
	} else {
		var u *url.URL
		u, err = url.Parse(reference)
		if err == nil {
			if u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" {
				return address.Address{}, fmt.Errorf("%w: expected a github:// address or github.com issue/PR URL", connector.ErrUndeliverable)
			}
			parts := strings.Split(strings.Trim(u.Path, "/"), "/")
			if len(parts) != 4 {
				return address.Address{}, fmt.Errorf("%w: expected a GitHub issue or PR reference", connector.ErrUndeliverable)
			}
			target = address.Address{Connector: Name, Workspace: parts[0], Path: parts[1:]}
		}
	}
	if err != nil {
		return address.Address{}, fmt.Errorf("github: read reference: %w", err)
	}
	ref, err := c.ref(target)
	if err != nil {
		return address.Address{}, err
	}
	// Positioning and mutation grants never cross a supplied reference. Only an
	// exact match to the bound source retains the original pinned head.
	clean := refAddress(ref, target.Path[1])
	if original, err := c.ref(source); err == nil && strings.EqualFold(original.Owner, ref.Owner) && strings.EqualFold(original.Repo, ref.Repo) {
		if original.Number == ref.Number && source.Params["head_sha"] != "" {
			clean = clean.WithParam("head_sha", source.Params["head_sha"])
		}
		return clean, nil
	}
	if !c.readRepos[strings.ToLower(ref.Owner+"/"+ref.Repo)] {
		return address.Address{}, fmt.Errorf("%w: repository %s/%s is not granted for cross-source reads", connector.ErrUndeliverable, ref.Owner, ref.Repo)
	}
	return clean, nil
}
