package catalog

import "github.com/urmzd/saige/agent/types"

// caps is a small constructor for table rows: it turns a capability list into
// the map form and leaves Provider/Model/Family/Known for Lookup to fill.
func caps(list ...types.Capability) types.ModelCapabilities {
	m := make(map[types.Capability]bool, len(list))
	for _, c := range list {
		m[c] = true
	}
	return types.ModelCapabilities{Caps: m}
}

// media builds a ContentSupport set for a row.
func media(list ...types.MediaType) types.ContentSupport {
	m := make(map[types.MediaType]bool, len(list))
	for _, mt := range list {
		m[mt] = true
	}
	return types.ContentSupport{NativeTypes: m}
}
