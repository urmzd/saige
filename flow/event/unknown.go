package event

import "encoding/json"

// Unknown is the body of a type this build does not know. It keeps the data
// byte for byte, so a relay passes it on unchanged. It is never guessed from
// the shape of the data.
type Unknown struct {
	// TypeName is the envelope's type.
	TypeName string
	// Raw is the data as it arrived.
	Raw json.RawMessage
}

// Type returns the type the envelope named.
func (u Unknown) Type() string { return u.TypeName }

func (Unknown) isBody() {}

// MarshalJSON writes Raw back unchanged, or null when there was no data.
func (u Unknown) MarshalJSON() ([]byte, error) {
	if len(u.Raw) == 0 {
		return []byte("null"), nil
	}
	return u.Raw, nil
}
