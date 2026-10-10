package types

import "context"

// ResolvedFile holds the result of resolving a URI to raw bytes.
type ResolvedFile struct {
	Data      []byte
	MediaType MediaType
}

// Resolver resolves a URI to raw file data.
type Resolver interface {
	Resolve(ctx context.Context, uri string) (ResolvedFile, error)
}

// ResolverFunc adapts a plain function to the Resolver interface.
type ResolverFunc func(ctx context.Context, uri string) (ResolvedFile, error)

// Resolve calls f.
func (f ResolverFunc) Resolve(ctx context.Context, uri string) (ResolvedFile, error) {
	return f(ctx, uri)
}
