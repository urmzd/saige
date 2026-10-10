package catalog

import (
	"fmt"
	"reflect"
	"sort"

	"github.com/urmzd/saige/agent/registry"
	"github.com/urmzd/saige/agent/types"
)

// InstallReport lists what an Install changed, by "provider/prefix" key.
type InstallReport struct {
	// Added rows had no revision before.
	Added []string
	// Changed rows received a new revision.
	Changed []string
	// Unchanged rows already resolved to the same entry.
	Unchanged []string
	// Removed rows were installed by an earlier Install and are no longer
	// declared. Each received a tombstone revision, so History still shows
	// what it said.
	Removed []string
	// Pinned rows received a revision but keep resolving to their pin.
	Pinned []string
}

var (
	active    *Catalog
	installed = map[string]bool{} // keys an Install has written
)

// Install validates c and makes its rows and baselines the ones Lookup
// resolves. A row whose resolved entry differs from its newest revision gets
// a new revision, recorded with opts; an identical row gets none, so the
// history shows only real changes. A pinned row stays pinned. Rows added
// with Register and never declared by a catalog are left in place. Presets
// are not installed: they live on the catalog value the host passes around.
func Install(c *Catalog, opts ...registry.Option) (InstallReport, error) {
	if c == nil {
		return InstallReport{}, fmt.Errorf("catalog: Install needs a catalog")
	}
	if err := c.Validate(); err != nil {
		return InstallReport{}, err
	}
	v := c.view()

	mu.Lock()
	defer mu.Unlock()
	var rep InstallReport
	declared := map[string]bool{}
	for _, e := range v.entries {
		k := key(e.Provider, e.Prefix)
		declared[k] = true
		latest, ok := models.Latest(k)
		switch {
		case !ok:
			rep.Added = append(rep.Added, k)
		case sameEntry(latest.Value, e):
			rep.Unchanged = append(rep.Unchanged, k)
			installed[k] = true
			continue
		default:
			rep.Changed = append(rep.Changed, k)
		}
		models.Register(k, cloneEntry(e), opts...)
		installed[k] = true
		if _, pinned := models.Pinned(k); pinned {
			rep.Pinned = append(rep.Pinned, k)
		}
	}
	for k := range installed {
		if declared[k] {
			continue
		}
		latest, ok := models.Latest(k)
		if !ok || latest.Value.removed {
			continue
		}
		tomb := Entry{Provider: latest.Value.Provider, Prefix: latest.Value.Prefix, removed: true}
		models.Register(k, tomb, opts...)
		rep.Removed = append(rep.Removed, k)
		delete(installed, k)
	}
	for p := range baseline {
		if _, ok := v.baselines[p]; !ok && installedBaselines[p] {
			delete(baseline, p)
			delete(installedBaselines, p)
		}
	}
	for p, b := range v.baselines {
		baseline[p] = b.ForModel(b.Model)
		installedBaselines[p] = true
	}
	for _, l := range [][]string{rep.Added, rep.Changed, rep.Unchanged, rep.Removed, rep.Pinned} {
		sort.Strings(l)
	}
	active = c.clone()
	return rep, nil
}

// installedBaselines are the baselines an Install wrote, so a later Install
// that drops one removes it while RegisterBaseline calls stay.
var installedBaselines = map[types.ProviderName]bool{}

func sameEntry(a, b Entry) bool {
	return reflect.DeepEqual(cloneEntry(a), cloneEntry(b))
}

// Active returns a copy of the catalog last installed: the embedded default
// until a host installs another.
func Active() *Catalog {
	mu.RLock()
	defer mu.RUnlock()
	if active == nil {
		return Default()
	}
	return active.clone()
}
