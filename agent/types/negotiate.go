package types

// ContentSupport declares which media types a model reads natively. It is
// the projection of an offering's input modalities onto
// ModelCapabilities.Media.
type ContentSupport struct {
	NativeTypes map[MediaType]bool
}

// Supports returns true if the given media type is natively supported.
func (cs ContentSupport) Supports(mt MediaType) bool {
	return cs.NativeTypes[mt]
}
