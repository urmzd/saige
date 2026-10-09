package catalog

import "github.com/urmzd/saige/agent/registry"

// The model table itself lives in data/default.json, embedded at build time.
// Rows are grouped by provider and declared by family prefix, longest match
// wins; templates hold what families share.
//
// Reading a row: a capability present means the flag is accepted and honoured.
// A knob absent from a reasoning model's row is usually absent because the API
// *rejects* it, not because nobody set it: OpenAI's reasoning models return a
// 400 for temperature and top_p, which is exactly the class of gap this table
// exists to make visible before the request is sent.
//
// # On the numbers
//
// Limits and prices are third-party facts on someone else's release schedule.
// They are declared only where they are solid, zero means undeclared rather
// than unlimited or free, and every priced row carries an as_of date so
// staleness is visible rather than assumed away. Rows whose pricing this
// catalog cannot state confidently are left unpriced on purpose: a Budget
// refuses to enforce against an unpriced model, which is the right failure.
// Correct a row in-process with Register or Install; either appends a
// revision, so History shows what changed and Rollback undoes it.

// defaultSource is the provenance recorded on revisions the embedded catalog
// installs.
const defaultSource = "catalog/default.json"

func init() {
	if _, err := Install(mustDefault(), registry.WithSource(defaultSource), registry.WithNote("embedded default catalog")); err != nil {
		panic("catalog: " + err.Error())
	}
}
