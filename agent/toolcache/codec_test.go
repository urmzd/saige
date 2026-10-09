package toolcache

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/cache/memcache"
	"github.com/urmzd/saige/agent/types"
)

func TestEntryCodecRoundTrip(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		name  string
		entry Entry
	}{
		{"text", Entry{Result: types.ToolResult{Text: "ok"}, StoredAt: now, ExpiresAt: now.Add(time.Minute)}},
		{"blocks keep their bytes", Entry{Result: types.ToolResult{Text: "img", Blocks: []types.ToolResultBlock{
			{Kind: types.ToolResultBlockKind("image"), MediaType: types.MediaPNG, Data: []byte{1, 2, 3}},
			{Kind: types.ToolResultBlockKind("json"), JSON: json.RawMessage(`{"a":1}`)},
		}}, StoredAt: now, ExpiresAt: now}},
		{"cached failure", Entry{Result: types.ToolResult{Text: "boom", IsError: true}, Err: "boom", StoredAt: now, ExpiresAt: now}},
		{"citation metadata", Entry{Result: types.ToolResult{Text: "c", Citations: []types.Citation{
			{URI: "https://example.com", Meta: map[string]any{"rank": json.Number("2")}},
		}}, StoredAt: now, ExpiresAt: now}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := EncodeEntry(tt.entry)
			if err != nil {
				t.Fatal(err)
			}
			got, err := DecodeEntry(raw)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.entry) {
				t.Fatalf("round trip = %#v, want %#v", got, tt.entry)
			}
		})
	}
}

func TestDecodeEntryRejects(t *testing.T) {
	for _, raw := range []string{`not json`, `{"v":99,"text":"x"}`} {
		if _, err := DecodeEntry([]byte(raw)); !errors.Is(err, ErrEntryCodec) {
			t.Errorf("DecodeEntry(%s) = %v, want ErrEntryCodec", raw, err)
		}
	}
}

func TestBytesCache(t *testing.T) {
	store := memcache.New[[]byte]()
	c := BytesCache(store)
	ctx := context.Background()
	entry := Entry{Result: types.ToolResult{Text: "v"}, StoredAt: time.Unix(1, 0).UTC(), ExpiresAt: time.Unix(2, 0).UTC()}
	if err := c.Set(ctx, "k", entry, 0); err != nil {
		t.Fatal(err)
	}
	got, found, err := c.Get(ctx, "k")
	if err != nil || !found || got.Result.Text != "v" {
		t.Fatalf("Get = %+v %v %v", got, found, err)
	}
	if err := store.Set(ctx, "bad", []byte("garbage"), 0); err != nil {
		t.Fatal(err)
	}
	if _, found, err := c.Get(ctx, "bad"); found || !errors.Is(err, ErrEntryCodec) {
		t.Fatalf("corrupt value: found %v err %v", found, err)
	}
}
