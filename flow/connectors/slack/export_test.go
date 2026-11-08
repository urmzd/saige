package slack

import "github.com/urmzd/saige/flow/attachment/intake"

// UseFetcher swaps the connector's file fetcher, so a test can serve files
// from loopback. The host and credential policies it would have used are
// exported below and tested on their own.
func UseFetcher(c *Connector, f *intake.Fetcher) { c.fetcher = f }

var (
	FileHosts   = fileHosts
	FileHeaders = fileHeaders
)
