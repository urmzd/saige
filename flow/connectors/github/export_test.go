package github

import "github.com/urmzd/saige/flow/attachment/intake"

// ServeFilesFrom points the connector's file fetches at a local server: each
// GitHub file URL is rewritten by to, and fetched with f. The host policy the
// connector would have applied is exported as FileAllowed and tested on its
// own.
func ServeFilesFrom(c *Connector, f *intake.Fetcher, to func(string) string) {
	c.fetcher, c.redirectForTest = f, to
}

var (
	FileAllowed = fileAllowed
	NoHeaders   = noHeaders
)

// MediaIn exposes the body scanner as name, alt pairs.
func MediaIn(body string) [][2]string {
	var out [][2]string
	for _, m := range mediaIn(body) {
		out = append(out, [2]string{m.url, m.alt})
	}
	return out
}
