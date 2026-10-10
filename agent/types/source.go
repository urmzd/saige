package types

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

// ArtifactScheme is the URI scheme of a workspace artifact reference.
const ArtifactScheme = "saige-artifact://"

// SourceKind names one way to reach media.
type SourceKind string

const (
	// SourceInline: the bytes are in the request.
	SourceInline SourceKind = "inline"
	// SourceURI: a location the provider can fetch, such as https or gs.
	SourceURI SourceKind = "uri"
	// SourceFile: a file uploaded to the provider's own file store.
	SourceFile SourceKind = "file"
	// SourceRef: a workspace artifact. It must be resolved before a provider
	// can use it.
	SourceRef SourceKind = "ref"
)

// Source is one logical piece of media and every way it can be reached. A
// media part may be reachable several ways at once (bytes, a URI, a
// workspace artifact, uploads to several vendors), and each adapter picks
// the locator it can use. That is what lets a request fail over between
// vendors: one vendor's file ID is useless to another, but the same Source
// can also carry bytes or a URI.
type Source struct {
	MediaType MediaType `json:"media_type"`
	Filename  string    `json:"filename,omitempty"`
	Size      int64     `json:"size,omitempty"`
	// Digest is the hex SHA-256 of the bytes, once known.
	Digest string `json:"sha256,omitempty"`

	// Inline holds the bytes. It is never persisted: stores keep the other
	// locators and the digest.
	Inline []byte `json:"-"`
	// URI is a location such as https://, gs://, s3:// or file://.
	URI string `json:"uri,omitempty"`
	// Ref is a workspace artifact reference (saige-artifact://<digest>).
	Ref string `json:"ref,omitempty"`
	// Files are uploads to vendor file stores.
	Files []VendorFile `json:"files,omitempty"`

	// Unresolved records why no locator could be resolved. A source with a
	// reason is rejected, never sent with its media silently missing.
	Unresolved string `json:"unresolved,omitempty"`
}

// VendorFile is an upload to a provider's file store. IDs are scoped to an
// endpoint (a workspace or project), so the endpoint is recorded with them.
type VendorFile struct {
	Provider  string    `json:"provider"`
	Endpoint  string    `json:"endpoint,omitempty"`
	ID        string    `json:"id"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}

// Bytes returns a source holding b, with its digest and size.
func Bytes(mt MediaType, b []byte) Source {
	sum := sha256.Sum256(b)
	return Source{MediaType: mt, Inline: b, Size: int64(len(b)), Digest: hex.EncodeToString(sum[:])}
}

// URL returns a source reachable at u. At most one media type is used.
func URL(u string, mt ...MediaType) Source {
	s := Source{URI: u}
	if len(mt) > 0 {
		s.MediaType = mt[0]
	}
	return s
}

// Artifact returns a source for a workspace artifact reference. The digest
// is taken from the reference when it has the artifact scheme.
func Artifact(ref string, mt MediaType) Source {
	s := Source{Ref: ref, MediaType: mt}
	if d, ok := strings.CutPrefix(ref, ArtifactScheme); ok {
		s.Digest = d
	}
	return s
}

// VendorFileID returns a source for a file already uploaded to a provider.
func VendorFileID(provider, endpoint, id string, mt MediaType) Source {
	return Source{MediaType: mt, Files: []VendorFile{{Provider: provider, Endpoint: endpoint, ID: id}}}
}

// With merges the locators of more into s: empty fields are filled and
// vendor files are added. When both digests are known and differ, the two
// are different media and s is returned unchanged.
func (s Source) With(more Source) Source {
	if s.Digest != "" && more.Digest != "" && s.Digest != more.Digest {
		return s
	}
	if s.MediaType == "" {
		s.MediaType = more.MediaType
	}
	if s.Filename == "" {
		s.Filename = more.Filename
	}
	if s.Size == 0 {
		s.Size = more.Size
	}
	if s.Digest == "" {
		s.Digest = more.Digest
	}
	if len(s.Inline) == 0 {
		s.Inline = more.Inline
	}
	if s.URI == "" {
		s.URI = more.URI
	}
	if s.Ref == "" {
		s.Ref = more.Ref
	}
	files := append([]VendorFile(nil), s.Files...)
	for _, f := range more.Files {
		if !hasVendorFile(files, f) {
			files = append(files, f)
		}
	}
	if len(files) > 0 {
		s.Files = files
	}
	return s
}

func hasVendorFile(fs []VendorFile, f VendorFile) bool {
	for _, g := range fs {
		if g.Provider == f.Provider && g.Endpoint == f.Endpoint && g.ID == f.ID {
			return true
		}
	}
	return false
}

// VendorFile returns the upload a provider can use: one for the provider on
// the given endpoint, or with no endpoint recorded. An empty endpoint
// matches any upload for the provider.
func (s Source) VendorFile(provider, endpoint string) (VendorFile, bool) {
	for _, f := range s.Files {
		if f.Provider != provider {
			continue
		}
		if endpoint == "" || f.Endpoint == "" || f.Endpoint == endpoint {
			return f, true
		}
	}
	return VendorFile{}, false
}

// Locator returns the way an adapter for provider on endpoint should reach
// the media: its own vendor file first, then the URI, then the inline
// bytes. A workspace reference is never returned, because a provider cannot
// read one; it must be resolved first. The bool is false when nothing
// usable is set.
func (s Source) Locator(provider, endpoint string) (SourceKind, bool) {
	if _, ok := s.VendorFile(provider, endpoint); ok {
		return SourceFile, true
	}
	if s.URI != "" {
		return SourceURI, true
	}
	if len(s.Inline) > 0 {
		return SourceInline, true
	}
	return "", false
}

// Kinds lists the locators s carries, in Locator's preference order.
func (s Source) Kinds() []SourceKind {
	var ks []SourceKind
	if len(s.Files) > 0 {
		ks = append(ks, SourceFile)
	}
	if s.URI != "" {
		ks = append(ks, SourceURI)
	}
	if len(s.Inline) > 0 {
		ks = append(ks, SourceInline)
	}
	if s.Ref != "" {
		ks = append(ks, SourceRef)
	}
	return ks
}

// Elided reports whether no locator is left: the bytes were not persisted
// and nothing else names the media. Adapters reject an elided source.
func (s Source) Elided() bool {
	return len(s.Inline) == 0 && s.URI == "" && s.Ref == "" && len(s.Files) == 0
}

// Unavailable returns s marked as unresolvable for reason.
func (s Source) Unavailable(reason string) Source {
	s.Unresolved = reason
	return s
}

// sourceJSON is the wire form of a Source, which carries the bytes.
type sourceJSON struct {
	MediaType  MediaType    `json:"media_type"`
	Filename   string       `json:"filename,omitempty"`
	Size       int64        `json:"size,omitempty"`
	Digest     string       `json:"sha256,omitempty"`
	Data       []byte       `json:"data,omitempty"`
	URI        string       `json:"uri,omitempty"`
	Ref        string       `json:"ref,omitempty"`
	Files      []VendorFile `json:"files,omitempty"`
	Unresolved string       `json:"unresolved,omitempty"`
}

// UnmarshalJSON reads a Source. Inline bytes are accepted when present, as
// in the wire form.
func (s *Source) UnmarshalJSON(b []byte) error {
	var w sourceJSON
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	*s = Source{MediaType: w.MediaType, Filename: w.Filename, Size: w.Size, Digest: w.Digest, Inline: w.Data,
		URI: w.URI, Ref: w.Ref, Files: w.Files, Unresolved: w.Unresolved}
	return nil
}

func (s Source) wireJSON() ([]byte, error) {
	return json.Marshal(sourceJSON{MediaType: s.MediaType, Filename: s.Filename, Size: s.Size, Digest: s.Digest,
		Data: s.Inline, URI: s.URI, Ref: s.Ref, Files: s.Files, Unresolved: s.Unresolved})
}
