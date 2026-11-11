// Package workspace defines the shared storage backend that every agent
// execution node mounts. All nodes in a deployment read and write the same
// workspace, so state lives in exactly one place regardless of how many
// agents are running.
//
// The interface is a minimal blob store so that backends (a local directory
// today; GCS, S3, or similar later) can slot in without touching any other
// package.
package workspace

import (
	"context"
	"errors"
	"io"
	"strings"
)

// ErrNotFound is returned when a key does not exist in the workspace.
var ErrNotFound = errors.New("workspace: key not found")

// ErrInvalidKey is returned when a key is empty, absolute, or attempts to
// escape the workspace (e.g. contains "..").
var ErrInvalidKey = errors.New("workspace: invalid key")

// Workspace is a flat, prefix-addressable blob store shared by every node in
// a deployment. Keys are slash-separated relative paths such as
// "runs/42/output.json".
type Workspace interface {
	// Read opens the blob at key. The caller must close the returned reader.
	// Returns ErrNotFound if the key does not exist.
	Read(ctx context.Context, key string) (io.ReadCloser, error)

	// Write stores the blob at key, replacing any existing value.
	Write(ctx context.Context, key string, r io.Reader) error

	// List returns all keys under prefix in lexical order. An empty prefix
	// lists every key.
	List(ctx context.Context, prefix string) ([]string, error)

	// Delete removes the blob at key. Returns ErrNotFound if the key does
	// not exist.
	Delete(ctx context.Context, key string) error
}

// ValidKey reports whether key is a well-formed workspace key: non-empty,
// relative, slash-separated, with no empty or "."/".." segments. Backends
// and decorators share this check so traversal is rejected uniformly.
func ValidKey(key string) bool {
	if key == "" || strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") {
		return false
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}
