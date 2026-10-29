# 0047. Never take a vendor file ID from an untrusted client

- **Status:** Accepted
- **Date:** 2026-10-10
- **Source:** PR #98 (commit 92516f5), recorded as D-47 in `DESIGN_DECISIONS.md`

## Decision

A vendor's file store belongs to an account, not to a user. Anthropic files are visible to the whole workspace, a Gemini Files API name to the whole project, and Vertex AI reads any `gs://` object its service account can. A locator a client supplies is therefore resolved with the host's credentials, and could name another tenant's file.
An untrusted client, such as the body of a request to a server, may send inline bytes, a `saige-artifact://` digest reference (which resolves only in its own session's store) and an https URL to public content. It may not send a vendor file, another URI scheme (`http`, `gs`, `s3`, `file`), a bare name such as `files/abc`, any other reference, or a URL on a vendor's file store. `types.CheckClientSource` and `types.CheckClientParts` apply the rule, and a host calls them where untrusted parts enter. Vendor files are only ever recorded by the host's own uploads, with the endpoint they belong to.
Derived results follow the same line. A conversion keyed by a digest is a content address: whoever sends the same bytes gets the same output, so it is shared without a scope. A part reached only by a URI or a vendor file is a name, so its conversion is memoized only under a named tenant scope. The response cache keys media by digest first and is scoped by tenant (`ScopeKey`), so a cached answer about one tenant's file never reaches another.
Behind a privacy boundary the conversion runs inside it (D-31): its text output is tokenized before it is sent, and with `require_text` media leaves only as text and a converter sends media only to an endpoint cleared for personal data.
