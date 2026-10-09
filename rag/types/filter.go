package types

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Fingerprint returns the deduplication fingerprint of data ingested under
// scope. In the default scope it is the hex SHA-256 of data. In any other
// scope the scope is hashed in front of the data, so identical bytes in two
// scopes never share a fingerprint and a fingerprint lookup cannot return
// another scope's document.
func Fingerprint(scope string, data []byte) string {
	h := sha256.New()
	if scope != "" {
		// The length prefix keeps (scope, data) pairs unambiguous.
		fmt.Fprintf(h, "scope:%d:%s\x00", len(scope), scope)
	}
	h.Write(data)
	return fmt.Sprintf("%x", h.Sum(nil))
}

// InTimeRange reports whether ts satisfies the Since and Until bounds of o.
// With no bound set every time passes, including the zero time. With a bound
// set the zero time (unknown) fails.
func (o *SearchOptions) InTimeRange(ts time.Time) bool {
	if o == nil || (o.Since.IsZero() && o.Until.IsZero()) {
		return true
	}
	if ts.IsZero() {
		return false
	}
	if !o.Since.IsZero() && ts.Before(o.Since) {
		return false
	}
	if !o.Until.IsZero() && !ts.Before(o.Until) {
		return false
	}
	return true
}

// Admits reports whether rec passes every condition of o other than the
// limit and minimum score: scope, time range, content type, and metadata
// filters on the document metadata merged with the variant metadata
// (variant keys win). Retrievers that score variants outside the store use
// it so their hits obey the same contract as a store search.
func (o *SearchOptions) Admits(rec *VariantRecord) bool {
	if o == nil {
		return rec.Scope == ""
	}
	if rec.Scope != o.Scope {
		return false
	}
	if !o.InTimeRange(rec.Timestamp) {
		return false
	}
	if len(o.ContentTypes) > 0 && !slices.Contains(o.ContentTypes, rec.Variant.ContentType) {
		return false
	}
	if len(o.MetadataFilters) > 0 {
		return MatchMetadata(MergeMetadata(rec.DocumentMetadata, rec.Variant.Metadata), o.MetadataFilters)
	}
	return true
}

// MergeMetadata overlays variant metadata on document metadata, the merge
// every store applies before evaluating metadata filters.
func MergeMetadata(docMeta, variantMeta map[string]string) map[string]string {
	merged := make(map[string]string, len(docMeta)+len(variantMeta))
	for k, v := range docMeta {
		merged[k] = v
	}
	for k, v := range variantMeta {
		merged[k] = v
	}
	return merged
}

// MatchMetadata reports whether meta satisfies every filter:
//
//   - FilterEq: the key exists and equals the value.
//   - FilterNeq: the key is absent or differs from the value.
//   - FilterContains: the key exists and contains the value.
//
// Unknown operators are ignored.
func MatchMetadata(meta map[string]string, filters []MetadataFilter) bool {
	for _, f := range filters {
		val, ok := meta[f.Key]
		switch f.Op {
		case FilterEq:
			if !ok || val != f.Value {
				return false
			}
		case FilterNeq:
			if ok && val == f.Value {
				return false
			}
		case FilterContains:
			if !ok || !strings.Contains(val, f.Value) {
				return false
			}
		}
	}
	return true
}
