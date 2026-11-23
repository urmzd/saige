package github

import (
	"fmt"
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
	target, err := ParseReadReference(reference)
	if err != nil {
		return address.Address{}, err
	}
	if isRepository(target) {
		return address.Address{}, fmt.Errorf("%w: %s names a repository; this action needs an issue or pull request, such as github://%s/%s/pull/12", connector.ErrUndeliverable, reference, target.Workspace, target.Path[0])
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
