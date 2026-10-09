package catalog

import (
	"bytes"
	_ "embed"
	"fmt"
	"sync"
)

// defaultJSON is the catalog this package ships: the single source of truth
// for the model rows Lookup resolves before a host installs anything else.
//
//go:embed data/default.json
var defaultJSON []byte

// schemaJSON is the JSON Schema of the catalog file format, for editors.
//
//go:embed catalog.schema.json
var schemaJSON []byte

var (
	defaultOnce sync.Once
	defaultCat  *Catalog
)

// mustDefault parses and validates the embedded catalog once. An invalid
// embedded catalog is a build defect, so it panics; the golden tests catch
// it first.
func mustDefault() *Catalog {
	defaultOnce.Do(func() {
		c, err := load(bytes.NewReader(defaultJSON), "catalog/default.json")
		if err == nil {
			err = c.Validate()
		}
		if err != nil {
			panic(fmt.Sprintf("catalog: embedded default catalog is invalid: %v", err))
		}
		defaultCat = c
	})
	return defaultCat
}

// Default returns a deep copy of the embedded, pre-validated catalog.
func Default() *Catalog { return mustDefault().clone() }

// DefaultJSON returns the embedded catalog file as shipped.
func DefaultJSON() []byte { return bytes.Clone(defaultJSON) }

// Schema returns the JSON Schema of the catalog file format.
func Schema() []byte { return bytes.Clone(schemaJSON) }
