// Package workspace is a scratch store for a run: notes, intermediate data,
// and tool results too large to send to the model whole.
//
// Content is addressed by its SHA-256 digest, so writing the same bytes twice
// yields the same Ref and a replayed durable step writes nothing new. Each
// artifact also has a name; writing a name again points it at the new
// content. Refs render as saige-artifact://<digest> URIs, which a model can
// pass back to scratch_read.
//
// Two backends ship: Memory for tests and short-lived runs, and Dir, which
// writes every file to a temporary name and renames it into place so a crash
// never leaves a partial artifact. View(true) gives a child agent read access
// without write access.
package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// URIScheme is the scheme of artifact URIs.
const URIScheme = "saige-artifact"

var (
	// ErrReadOnly is returned by writes through a read-only view.
	ErrReadOnly = errors.New("workspace: read-only")
	// ErrNotFound is returned when no artifact matches a Ref.
	ErrNotFound = errors.New("workspace: artifact not found")
	// ErrInvalidRef is returned for a Ref or URI that names nothing.
	ErrInvalidRef = errors.New("workspace: invalid reference")
)

// Ref identifies an artifact. ID is the hex SHA-256 of the content; Name is
// the label it was written under. Read accepts a Ref with only ID or only
// Name set; a Name resolves to the latest content written under it.
type Ref struct {
	ID      string            `json:"id"`
	Name    string            `json:"name,omitempty"`
	Size    int64             `json:"size"`
	Meta    map[string]string `json:"meta,omitempty"`
	Created time.Time         `json:"created"`
}

// URI returns the artifact's saige-artifact:// URI.
func (r Ref) URI() string { return URIScheme + "://" + r.ID }

// ParseRef accepts a saige-artifact:// URI, a bare digest, or a name.
func ParseRef(s string) (Ref, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Ref{}, ErrInvalidRef
	}
	if rest, ok := strings.CutPrefix(s, URIScheme+"://"); ok {
		if !isDigest(rest) {
			return Ref{}, fmt.Errorf("%w: %q", ErrInvalidRef, s)
		}
		return Ref{ID: rest}, nil
	}
	if isDigest(s) {
		return Ref{ID: s}, nil
	}
	return Ref{Name: s}, nil
}

// Hit is one matching line from Search.
type Hit struct {
	Ref    Ref
	Line   int    // 1-based line number
	Offset int64  // byte offset of the line, for Read
	Text   string // the line, shortened to MaxHitText bytes
}

// MaxHitText bounds the line text a Hit carries.
const MaxHitText = 240

// Workspace stores artifacts for a run.
type Workspace interface {
	// Put stores data under name and returns its Ref. Writing identical
	// content again returns the same ID and is safe to repeat.
	Put(ctx context.Context, name string, data []byte, meta map[string]string) (Ref, error)
	// Read returns up to limit bytes starting at offset. limit <= 0 reads
	// to the end. An offset at or past the end returns no bytes.
	Read(ctx context.Context, ref Ref, offset, limit int64) ([]byte, error)
	// Stat resolves ref to the stored artifact's full Ref.
	Stat(ctx context.Context, ref Ref) (Ref, error)
	// List returns the latest artifact for every name, sorted by name.
	List(ctx context.Context) ([]Ref, error)
	// Search returns up to k lines containing every term of query,
	// case-insensitively, ordered by name and then line.
	Search(ctx context.Context, query string, k int) ([]Hit, error)
	// View returns a view of the same store. A read-only view rejects Put
	// with ErrReadOnly, and a view of it stays read-only.
	View(readOnly bool) Workspace
}

// Digest returns the content ID of data.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func isDigest(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

func copyMeta(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func cloneRef(r Ref) Ref {
	r.Meta = copyMeta(r.Meta)
	return r
}

func validName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%w: empty name", ErrInvalidRef)
	}
	if len(name) > 512 {
		return fmt.Errorf("%w: name longer than 512 bytes", ErrInvalidRef)
	}
	return nil
}

func window(data []byte, offset, limit int64) []byte {
	if offset < 0 {
		offset = 0
	}
	if offset >= int64(len(data)) {
		return []byte{}
	}
	end := int64(len(data))
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	out := make([]byte, end-offset)
	copy(out, data[offset:end])
	return out
}

// searchRefs scans the latest artifact of each name, in name order.
func searchRefs(ctx context.Context, refs []Ref, load func(Ref) ([]byte, error), query string, k int) ([]Hit, error) {
	terms := strings.Fields(strings.ToLower(query))
	if len(terms) == 0 {
		return nil, fmt.Errorf("workspace: empty search query")
	}
	if k <= 0 {
		k = 10
	}
	var hits []Hit
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, err := load(ref)
		if err != nil {
			return nil, err
		}
		var offset int64
		for i, line := range strings.SplitAfter(string(data), "\n") {
			lower := strings.ToLower(line)
			match := true
			for _, t := range terms {
				if !strings.Contains(lower, t) {
					match = false
					break
				}
			}
			if match {
				text := strings.TrimRight(line, "\r\n")
				if len(text) > MaxHitText {
					text = truncateUTF8(text, MaxHitText) + "..."
				}
				hits = append(hits, Hit{Ref: cloneRef(ref), Line: i + 1, Offset: offset, Text: text})
				if len(hits) == k {
					return hits, nil
				}
			}
			offset += int64(len(line))
		}
	}
	return hits, nil
}

func sortRefs(refs []Ref) {
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
}

// truncateUTF8 cuts s to at most n bytes without splitting a character.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

type ctxKey struct{}

// NewContext returns ctx carrying ws. The agent loop attaches its workspace
// to every tool call, so the scratch tools use a sub-agent's read-only view
// rather than the store they were built with.
func NewContext(ctx context.Context, ws Workspace) context.Context {
	return context.WithValue(ctx, ctxKey{}, ws)
}

// FromContext returns the workspace attached to ctx.
func FromContext(ctx context.Context) (Workspace, bool) {
	ws, ok := ctx.Value(ctxKey{}).(Workspace)
	return ws, ok && ws != nil
}

// readOnly is a view that rejects writes.
type readOnly struct{ Workspace }

func (r readOnly) Put(context.Context, string, []byte, map[string]string) (Ref, error) {
	return Ref{}, ErrReadOnly
}

func (r readOnly) View(bool) Workspace { return r }

func view(ws Workspace, ro bool) Workspace {
	if ro {
		return readOnly{ws}
	}
	return ws
}
