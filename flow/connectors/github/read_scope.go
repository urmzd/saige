package github

import (
	"context"
	"fmt"
	"strings"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connector"
)

// readTarget permits explicit GitHub references from any connector, but only
// in the run's original GitHub repository or a repository granted for
// cross-source reads (crossSourceRead). URLs are parsed into API resource
// coordinates, never fetched as arbitrary URLs.
func (c *Connector) readTarget(ctx context.Context, source address.Address, reference string) (address.Address, error) {
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
	if err := c.crossSourceRead(ctx, ref.Owner, ref.Repo); err != nil {
		return address.Address{}, err
	}
	return clean, nil
}

// crossSourceRead decides a read of owner/repo by a run raised anywhere
// else: Config.ReadRefused refuses it first, whatever grants it; then
// Config.ReadRepositories grants it by name; then Config.MayRead is asked.
// Only the read actions call it, so neither hook ever reaches a write.
func (c *Connector) crossSourceRead(ctx context.Context, owner, repo string) error {
	if c.cfg.ReadRefused != nil && c.cfg.ReadRefused(ctx, owner, repo) {
		return fmt.Errorf("%w: repository %s/%s is not readable from other surfaces; only runs on its own issues and pull requests read it", connector.ErrUndeliverable, owner, repo)
	}
	if c.readRepos[strings.ToLower(owner+"/"+repo)] {
		return nil
	}
	if c.cfg.MayRead != nil && c.cfg.MayRead(ctx, owner, repo) {
		return nil
	}
	return fmt.Errorf("%w: repository %s/%s is not granted for cross-source reads", connector.ErrUndeliverable, owner, repo)
}
