package native_test

import (
	"encoding/json"

	"github.com/urmzd/saige/flow/event"
)

// raw is a body as the JSON it is published as.
func raw(b event.Body) []byte {
	out, err := json.Marshal(b)
	if err != nil {
		panic(err)
	}
	return out
}

// decodeBody reads a body back as T, as a subscriber that has only its JSON
// would.
func decodeBody[T any](b event.Body) (T, error) {
	var out T
	err := json.Unmarshal(raw(b), &out)
	return out, err
}
