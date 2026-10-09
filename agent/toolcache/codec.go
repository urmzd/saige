package toolcache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// EntryCodecVersion is the Entry encoding version EncodeEntry writes.
// DecodeEntry rejects other versions, so a store shared across releases
// misses rather than replaying a value it cannot read.
const EntryCodecVersion = 1

// ErrEntryCodec reports a stored value DecodeEntry cannot read.
var ErrEntryCodec = errors.New("tool cache: cannot decode stored value")

// wireEntry is the stored form of an Entry. Blocks keep their raw bytes,
// which the JSON form of types.ToolResultBlock leaves out.
type wireEntry struct {
	V         int              `json:"v"`
	Text      string           `json:"text"`
	Blocks    []wireBlock      `json:"blocks,omitempty"`
	IsError   bool             `json:"is_error,omitempty"`
	Citations []types.Citation `json:"citations,omitempty"`
	StoredAt  time.Time        `json:"stored_at"`
	ExpiresAt time.Time        `json:"expires_at"`
	Err       string           `json:"err,omitempty"`
}

type wireBlock struct {
	types.ToolResultBlock
	Data []byte `json:"data,omitempty"`
}

// EncodeEntry serializes a cached tool result for a byte store such as Redis
// or disk.
func EncodeEntry(e Entry) ([]byte, error) {
	w := wireEntry{
		V:         EntryCodecVersion,
		Text:      e.Result.Text,
		IsError:   e.Result.IsError,
		Citations: e.Result.Citations,
		StoredAt:  e.StoredAt,
		ExpiresAt: e.ExpiresAt,
		Err:       e.Err,
	}
	for _, b := range e.Result.Blocks {
		w.Blocks = append(w.Blocks, wireBlock{ToolResultBlock: b, Data: b.Data})
	}
	return json.Marshal(w)
}

// DecodeEntry is the inverse of EncodeEntry. Numbers inside citation
// metadata decode as json.Number, matching what the in-memory path returns.
func DecodeEntry(b []byte) (Entry, error) {
	var w wireEntry
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.UseNumber()
	if err := decoder.Decode(&w); err != nil {
		return Entry{}, fmt.Errorf("%w: %v", ErrEntryCodec, err)
	}
	if w.V != EntryCodecVersion {
		return Entry{}, fmt.Errorf("%w: version %d", ErrEntryCodec, w.V)
	}
	e := Entry{
		Result:    types.ToolResult{Text: w.Text, IsError: w.IsError, Citations: w.Citations},
		StoredAt:  w.StoredAt,
		ExpiresAt: w.ExpiresAt,
		Err:       w.Err,
	}
	for _, wb := range w.Blocks {
		blk := wb.ToolResultBlock
		blk.Data = wb.Data
		e.Result.Blocks = append(e.Result.Blocks, blk)
	}
	return e, nil
}

// BytesCache adapts a byte store to the tool cache, so a shared backend
// stores only bytes. A stored value that does not decode is reported as an
// error by Get, which the tool cache logs, counts, and treats as a miss.
func BytesCache(store types.Cache[[]byte]) types.Cache[Entry] {
	return bytesCache{store: store}
}

type bytesCache struct {
	store types.Cache[[]byte]
}

func (c bytesCache) Get(ctx context.Context, key string) (Entry, bool, error) {
	raw, found, err := c.store.Get(ctx, key)
	if err != nil || !found {
		return Entry{}, false, err
	}
	e, err := DecodeEntry(raw)
	if err != nil {
		return Entry{}, false, err
	}
	return e, true, nil
}

func (c bytesCache) Set(ctx context.Context, key string, value Entry, ttl time.Duration) error {
	raw, err := EncodeEntry(value)
	if err != nil {
		return err
	}
	return c.store.Set(ctx, key, raw, ttl)
}

func (c bytesCache) Delete(ctx context.Context, key string) error {
	return c.store.Delete(ctx, key)
}
