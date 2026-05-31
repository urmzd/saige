package types

import (
	"context"
	"time"
)

// Cache is a generic key/value cache with optional per-entry TTL.
// Implementations must be safe for concurrent use. The zero value of V is
// returned together with found=false on a miss.
//
// Keys are opaque strings; callers are responsible for producing a
// deterministic, collision-resistant key (see agent/provider/cache.Key).
type Cache[V any] interface {
	// Get returns the cached value for key. found is false on a miss or when
	// the entry has expired.
	Get(ctx context.Context, key string) (value V, found bool, err error)

	// Set stores value under key. A zero ttl means "use the implementation's
	// default" (which may itself be "no expiry"). Implementations that do not
	// support TTL ignore it.
	Set(ctx context.Context, key string, value V, ttl time.Duration) error

	// Delete removes key if present. Deleting a missing key is not an error.
	Delete(ctx context.Context, key string) error
}
