package github

import (
	"context"
	"fmt"
	"strings"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connector"
)

// ReadError is the refusal of a cross-source read: a read of a repository
// other than the run's own that nothing grants, or that Config.ReadRefused
// keeps to its own runs. It is returned before any request is made, and
// errors.Is reports it as connector.ErrUndeliverable.
type ReadError struct {
	// Owner and Repo name the repository as the reference wrote it.
	// Config.ReadRefused and Config.MayRead are given them in lower case.
	Owner, Repo string
	// Kept is true when Config.ReadRefused refused the read, and false
	// when neither Config.ReadRepositories nor Config.MayRead granted it.
	Kept bool
}

func (e *ReadError) Error() string {
	if e.Kept {
		return fmt.Sprintf("%s: repository %s/%s is not readable from other surfaces; only runs on its own issues and pull requests read it", connector.ErrUndeliverable, e.Owner, e.Repo)
	}
	return fmt.Sprintf("%s: repository %s/%s is not granted for cross-source reads", connector.ErrUndeliverable, e.Owner, e.Repo)
}

// Unwrap makes a ReadError a connector.ErrUndeliverable.
func (e *ReadError) Unwrap() error { return connector.ErrUndeliverable }

// readTarget permits explicit GitHub references from any connector, but only
// in the run's original GitHub repository or a repository granted for
// cross-source reads (crossSourceRead). URLs are parsed into API resource
// coordinates, never fetched as arbitrary URLs. The address it returns is
// the one the caller reads, so the grant is decided for what is read, and
// the context it returns is the one to read with: for a repository other
// than the run's own it is marked ReadWhereNamed, so the read is answered
// by the repository the grant was decided for or not at all. For the run's
// own repository it is marked by ownRead.
func (c *Connector) readTarget(ctx context.Context, source address.Address, reference string) (context.Context, address.Address, error) {
	return c.readTargetIn(ctx, source, reference, false)
}

// readTargetIn is readTarget; granted says the reference's repository has
// already passed repositoryTarget in this call, so the grants are not asked
// a second time for it.
func (c *Connector) readTargetIn(ctx context.Context, source address.Address, reference string, granted bool) (context.Context, address.Address, error) {
	if reference == "" {
		own, err := c.ref(source)
		if err != nil {
			return ctx, address.Address{}, err
		}
		return c.ownRead(ctx, own.Owner, own.Repo), source, nil
	}
	target, err := ParseReadReference(reference)
	if err != nil {
		return ctx, address.Address{}, err
	}
	if isRepository(target) {
		return ctx, address.Address{}, fmt.Errorf("%w: %s names a repository; this action needs an issue or pull request, such as github://%s/%s/pull/12", connector.ErrUndeliverable, reference, target.Workspace, target.Path[0])
	}
	ref, err := c.ref(target)
	if err != nil {
		return ctx, address.Address{}, err
	}
	// Positioning and mutation grants never cross a supplied reference. Only an
	// exact match to the bound source retains the original pinned head.
	clean := refAddress(ref, target.Path[1])
	if ownRepository(source, ref.Owner, ref.Repo) {
		if original, err := c.ref(source); err == nil && original.Number == ref.Number && source.Params["head_sha"] != "" {
			clean = clean.WithParam("head_sha", source.Params["head_sha"])
		}
		return c.ownRead(ctx, ref.Owner, ref.Repo), clean, nil
	}
	if !granted {
		if err := c.crossSourceRead(ctx, ref.Owner, ref.Repo); err != nil {
			return ctx, address.Address{}, err
		}
	}
	return ReadWhereNamed(ctx), clean, nil
}

// ownRepository says whether owner/repo is the repository the run was
// raised on: source is a github address of the repository itself or of one
// of its issues or pull requests. Every read decides "the run's own" here.
func ownRepository(source address.Address, owner, repo string) bool {
	return source.Connector == Name && len(source.Path) > 0 && strings.EqualFold(source.Workspace, owner) && strings.EqualFold(source.Path[0], repo)
}

// crossSourceRead decides a read of owner/repo by a run raised anywhere
// else: Config.ReadRefused refuses it first, whatever grants it; then
// Config.ReadRepositories grants it by name; then Config.MayRead is asked.
// GitHub names are case-insensitive, so both hooks are given the owner and
// repository in lower case. Only the reads that take a reference call it
// (the read actions and ReadComments), once for each, and a read of the
// run's own repository calls it for a repository a redirect names
// (redirectGrant), so neither hook ever reaches a write. A refusal is a
// *ReadError.
func (c *Connector) crossSourceRead(ctx context.Context, owner, repo string) error {
	lowerOwner, lowerRepo := strings.ToLower(owner), strings.ToLower(repo)
	if c.cfg.ReadRefused != nil && c.cfg.ReadRefused(ctx, lowerOwner, lowerRepo) {
		return &ReadError{Owner: owner, Repo: repo, Kept: true}
	}
	if c.readRepos[lowerOwner+"/"+lowerRepo] {
		return nil
	}
	if c.cfg.MayRead != nil && c.cfg.MayRead(ctx, lowerOwner, lowerRepo) {
		return nil
	}
	return &ReadError{Owner: owner, Repo: repo}
}

// ownRead marks ctx for a read of the run's own repository owner/repo. A
// redirect within it is followed; a redirect to another repository is
// followed only when redirectGrant allows that repository, which is asked
// once for each repository in the read (FollowReadRedirect).
func (c *Connector) ownRead(ctx context.Context, owner, repo string) context.Context {
	return context.WithValue(ctx, ownReadKey{}, ownRead{owner: owner, repo: repo, grant: c.redirectGrant, decided: &redirectDecisions{}})
}

// redirectGrant decides a redirect of a read of the run's own repository to
// owner/repo as a cross-source read of owner/repo: the connector serves the
// owner, the names are valid, and crossSourceRead grants it.
func (c *Connector) redirectGrant(ctx context.Context, owner, repo string) error {
	if c.owners != nil && !c.owners[strings.ToLower(owner)] {
		return fmt.Errorf("%w: owner %q is not served", connector.ErrUndeliverable, owner)
	}
	if address.ValidWorkspace(owner) != nil || !validRepository(repo) {
		return fmt.Errorf("%w: invalid GitHub repository", connector.ErrUndeliverable)
	}
	return c.crossSourceRead(ctx, owner, repo)
}
