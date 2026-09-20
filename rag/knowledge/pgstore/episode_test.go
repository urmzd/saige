package pgstore

import (
	"maps"
	"testing"
)

func TestEpisodeMetadataRoundTrip(t *testing.T) {
	meta := map[string]string{"channel": "slack", "author": "alice"}

	got := decodeEpisodeMetadata(encodeEpisodeMetadata(meta))
	if !maps.Equal(got, meta) {
		t.Errorf("round trip = %v, want %v", got, meta)
	}
}

func TestEpisodeMetadataEmptyStaysNil(t *testing.T) {
	if b := encodeEpisodeMetadata(nil); b != nil {
		t.Errorf("encode(nil) = %q, want nil", b)
	}
	if b := encodeEpisodeMetadata(map[string]string{}); b != nil {
		t.Errorf("encode(empty) = %q, want nil", b)
	}
	if m := decodeEpisodeMetadata(nil); m != nil {
		t.Errorf("decode(nil) = %v, want nil", m)
	}
	if m := decodeEpisodeMetadata([]byte("{}")); m != nil {
		t.Errorf("decode({}) = %v, want nil", m)
	}
}

func TestEpisodeMetadataDecodeMalformed(t *testing.T) {
	if m := decodeEpisodeMetadata([]byte("not json")); m != nil {
		t.Errorf("decode(malformed) = %v, want nil", m)
	}
}
