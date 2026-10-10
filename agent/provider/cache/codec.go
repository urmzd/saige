package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// ResponseCodecVersion is the CachedResponse encoding version EncodeResponse
// writes. DecodeResponse rejects other versions, so a store shared across
// releases misses rather than replaying a value it cannot read.
const ResponseCodecVersion = 2

// ErrResponseCodec reports a stored value DecodeResponse cannot read.
var ErrResponseCodec = errors.New("response cache: cannot decode stored value")

// wireResponse is the stored form of a CachedResponse. Each delta is a
// versioned wire envelope (types.MarshalDelta), so the delta encoding is the
// same one used to stream events across a process boundary.
type wireResponse struct {
	V      int               `json:"v"`
	Deltas []json.RawMessage `json:"deltas"`
	Usage  json.RawMessage   `json:"usage"`
}

// EncodeResponse serializes a recorded response for a byte store such as
// postgres.CacheStore or disk. Only the delta kinds the recorder keeps are
// accepted.
func EncodeResponse(cr CachedResponse) ([]byte, error) {
	w := wireResponse{V: ResponseCodecVersion, Deltas: make([]json.RawMessage, 0, len(cr.Deltas))}
	for _, d := range cr.Deltas {
		if !recordable(d) {
			return nil, fmt.Errorf("response cache: cannot encode %T", d)
		}
		raw, err := types.MarshalDelta(d)
		if err != nil {
			return nil, err
		}
		w.Deltas = append(w.Deltas, raw)
	}
	usage, err := types.MarshalDelta(cr.Usage)
	if err != nil {
		return nil, err
	}
	w.Usage = usage
	return json.Marshal(w)
}

// DecodeResponse is the inverse of EncodeResponse. Numbers inside tool
// arguments and citation metadata decode as json.Number, matching what an
// in-memory store returns, so replay behaves the same for both.
func DecodeResponse(b []byte) (CachedResponse, error) {
	var w wireResponse
	if err := json.Unmarshal(b, &w); err != nil {
		return CachedResponse{}, fmt.Errorf("%w: %w", ErrResponseCodec, err)
	}
	if w.V != ResponseCodecVersion {
		return CachedResponse{}, fmt.Errorf("%w: version %d", ErrResponseCodec, w.V)
	}
	cr := CachedResponse{Deltas: make([]types.Delta, 0, len(w.Deltas))}
	for _, raw := range w.Deltas {
		d, err := types.UnmarshalDelta(raw)
		if err != nil {
			return CachedResponse{}, fmt.Errorf("%w: %w", ErrResponseCodec, err)
		}
		if !recordable(d) {
			return CachedResponse{}, fmt.Errorf("%w: unexpected %T", ErrResponseCodec, d)
		}
		cr.Deltas = append(cr.Deltas, d)
	}
	if len(w.Usage) > 0 {
		d, err := types.UnmarshalDelta(w.Usage)
		if err != nil {
			return CachedResponse{}, fmt.Errorf("%w: %w", ErrResponseCodec, err)
		}
		usage, ok := d.(types.UsageDelta)
		if !ok {
			return CachedResponse{}, fmt.Errorf("%w: usage is %T", ErrResponseCodec, d)
		}
		cr.Usage = usage
	}
	return cr, nil
}

// recordable reports whether the recorder keeps d in CachedResponse.Deltas.
func recordable(d types.Delta) bool {
	switch d.(type) {
	case types.PartStart, types.PartDelta, types.PartEnd, types.CitationDelta:
		return true
	default:
		return false
	}
}

// BytesCache adapts a byte store to the response cache. A shared backend
// (postgres.CacheStore, disk) then stores only bytes and needs no knowledge
// of delta types. A stored value that does not decode is reported as an error by
// Get, which the response cache logs and treats as a miss.
func BytesCache(store types.Cache[[]byte]) types.Cache[CachedResponse] {
	return bytesCache{store: store}
}

type bytesCache struct {
	store types.Cache[[]byte]
}

func (c bytesCache) Get(ctx context.Context, key string) (CachedResponse, bool, error) {
	raw, found, err := c.store.Get(ctx, key)
	if err != nil || !found {
		return CachedResponse{}, false, err
	}
	cr, err := DecodeResponse(raw)
	if err != nil {
		return CachedResponse{}, false, err
	}
	return cr, true, nil
}

func (c bytesCache) Set(ctx context.Context, key string, value CachedResponse, ttl time.Duration) error {
	raw, err := EncodeResponse(value)
	if err != nil {
		return err
	}
	return c.store.Set(ctx, key, raw, ttl)
}

func (c bytesCache) Delete(ctx context.Context, key string) error {
	return c.store.Delete(ctx, key)
}
