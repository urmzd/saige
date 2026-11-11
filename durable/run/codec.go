package run

import "encoding/json"

// Codec marshals activity inputs and outputs. JSON is the default: ledger
// payloads stay greppable and queryable in the caller's own tables. The
// codec is the only payload path — encryption and claim-check land later as
// wrapping codecs, so ContentType is recorded per payload.
type Codec interface {
	Marshal(v any) ([]byte, error)
	Unmarshal(data []byte, v any) error
	ContentType() string
}

// JSONCodec is the default Codec.
type JSONCodec struct{}

// Marshal encodes v as JSON.
func (JSONCodec) Marshal(v any) ([]byte, error) { return json.Marshal(v) }

// Unmarshal decodes JSON b into v.
func (JSONCodec) Unmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// ContentType is "application/json", stored with every record it produces.
func (JSONCodec) ContentType() string { return "application/json" }
