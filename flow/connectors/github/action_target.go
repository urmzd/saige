package github

import (
	"fmt"
	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connector"
	"net/url"
	"strings"
)

// actionTarget grants explicit writes across surfaces independently of read
// grants. The original repository remains usable; every other repository must
// be named by the operator, even if the installation token can access it.
func (c *Connector) actionTarget(source address.Address, reference string) (address.Address, error) {
	if reference == "" {
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
			parts := strings.Split(strings.Trim(u.Path, "/"), "/")
			if u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || len(parts) != 4 {
				return address.Address{}, fmt.Errorf("%w: expected a GitHub issue or pull request address", connector.ErrUndeliverable)
			}
			target = address.Address{Connector: Name, Workspace: parts[0], Path: parts[1:]}
		}
	}
	if err != nil {
		return address.Address{}, err
	}
	ref, err := c.ref(target)
	if err != nil {
		return address.Address{}, err
	}
	if current, err := c.ref(source); err == nil && strings.EqualFold(current.Owner, ref.Owner) && strings.EqualFold(current.Repo, ref.Repo) {
		return refAddress(ref, target.Path[1]), nil
	}
	for _, grant := range c.cfg.ActionRepositories {
		if strings.EqualFold(grant, ref.Owner+"/"+ref.Repo) {
			return refAddress(ref, target.Path[1]), nil
		}
	}
	return address.Address{}, fmt.Errorf("%w: repository is not granted for cross-surface actions", connector.ErrUndeliverable)
}
