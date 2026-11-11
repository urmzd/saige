package runtime_test

import (
	"encoding/json"

	"github.com/urmzd/saige/flow/event"
)

// decode reads an event's body into T through its JSON, so a test can read a
// body without naming each type it might be.
func decode[T any](e event.Envelope) (T, error) {
	var out T
	b, err := json.Marshal(e.Data)
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(b, &out)
	return out, err
}

// raw is an event's body as JSON.
func raw(e event.Envelope) []byte {
	b, err := json.Marshal(e.Data)
	if err != nil {
		panic(err)
	}
	return b
}
