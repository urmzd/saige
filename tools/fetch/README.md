# tools/fetch

A `fetch` tool that retrieves an http or https URL and returns its text, through an HTTP client that refuses private, local, and cloud metadata addresses.

```go
import "github.com/urmzd/saige/tools/fetch"

tool := fetch.NewTool()                                 // private addresses blocked
local := fetch.NewTool(fetch.AllowPrivateNetworks())   // reach the local network; metadata stays blocked
```

## Behavior

- HTML becomes plain text: scripts and styles are dropped and block elements become line breaks. JSON, XML, and other text types are returned as is. Binary content types are refused.
- Bodies over 2 MB are cut (`WithMaxBytes`); returned text is capped at 20000 characters by default, which a call can change with `max_chars`.
- HTTP status 400 and above is returned as an error with the start of the body.
- URLs with embedded credentials and non-http schemes are refused. Capability: `read`.

## SafeHTTPClient

`SafeHTTPClient(blockPrivate)` checks the resolved IP at connect time, after DNS, so redirects and re-resolution cannot reach a forbidden address. Link-local and cloud metadata ranges (`169.254.0.0/16`, `fe80::/10`, `fd00:ec2::254`, `100.100.100.200`) are always refused. With `blockPrivate`, loopback, RFC 1918, IPv6 unique local, carrier-grade NAT, unspecified, and multicast addresses are refused too. Proxy variables are ignored and at most five redirects are followed. Use it for any client that dials model-chosen URLs.

## Related

- [`tools/fs`](../fs/README.md), [`tools/exec`](../exec/README.md)
- [Root README](../../README.md)
