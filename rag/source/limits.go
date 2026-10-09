package source

import (
	"errors"
	"fmt"
	"io"
	"time"
)

// ErrTooLarge is returned (wrapped) when a fetched item exceeds its source's
// MaxBytes limit.
var ErrTooLarge = errors.New("source item exceeds size limit")

// DefaultMaxBytes is the per-item size limit used when a source's MaxBytes
// is zero. A negative MaxBytes disables the limit.
const DefaultMaxBytes int64 = 32 << 20

// DefaultHTTPTimeout bounds each HTTP request when HTTP.Client is nil.
const DefaultHTTPTimeout = 30 * time.Second

// effectiveLimit resolves a MaxBytes setting to a byte limit, where a
// negative result means unlimited.
func effectiveLimit(maxBytes int64) int64 {
	if maxBytes == 0 {
		return DefaultMaxBytes
	}
	return maxBytes
}

// readLimited reads r fully, failing with ErrTooLarge once more than limit
// bytes arrive. It reads at most limit+1 bytes, so an oversized or endless
// stream cannot exhaust memory. A negative limit reads without a bound.
func readLimited(r io.Reader, limit int64) ([]byte, error) {
	if limit < 0 {
		return io.ReadAll(r)
	}
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: more than %d bytes", ErrTooLarge, limit)
	}
	return data, nil
}
