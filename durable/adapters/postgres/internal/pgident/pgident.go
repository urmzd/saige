// Package pgident validates and quotes SQL identifiers supplied through
// adapter mappings. Mapping identifiers are the only strings that ever reach
// SQL text unparameterized, so every one of them passes through this package:
// validation happens once at construction time, and quoting goes through
// pgx.Identifier so a hostile column or table name cannot escape its quotes.
package pgident

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
)

// part matches one unqualified identifier: lower_snake_case only, the
// conservative subset of names that need no quoting games and cannot smuggle
// SQL. Lowercase is load-bearing, not taste: PostgreSQL case-folds unquoted
// identifiers to lowercase, and Validate resolves mapping names unquoted
// (to_regclass) while runtime SQL quotes them case-preserved. A mixed-case
// mapping would make the two disagree about which table exists — Validate
// could pass against one table while runtime statements hit another (or
// nothing). Restricting mappings to lowercase keeps the two views identical.
var part = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// Column validates a single unqualified column identifier. field names the
// mapping field for the error message.
func Column(field, name string) error {
	if name == "" {
		return fmt.Errorf("%s is required", field)
	}
	if !part.MatchString(name) {
		return fmt.Errorf("%s: unsafe identifier %q: anchor mappings use lower_snake_case identifiers (must match ^[a-z_][a-z0-9_]*$)", field, name)
	}
	return nil
}

// Table validates a table identifier, optionally schema-qualified as
// "schema.table". field names the mapping field for the error message.
func Table(field, name string) error {
	if name == "" {
		return fmt.Errorf("%s is required", field)
	}
	parts := strings.Split(name, ".")
	if len(parts) > 2 {
		return fmt.Errorf("%s: %q has more than one dot; want table or schema.table", field, name)
	}
	for _, p := range parts {
		if !part.MatchString(p) {
			return fmt.Errorf("%s: unsafe identifier %q: anchor mappings use lower_snake_case identifiers (each part must match ^[a-z_][a-z0-9_]*$)", field, name)
		}
	}
	return nil
}

// QuoteColumn returns the double-quoted form of a column identifier.
func QuoteColumn(name string) string {
	return pgx.Identifier{name}.Sanitize()
}

// QuoteTable returns the double-quoted form of a table identifier,
// preserving schema qualification.
func QuoteTable(name string) string {
	return pgx.Identifier(strings.Split(name, ".")).Sanitize()
}
