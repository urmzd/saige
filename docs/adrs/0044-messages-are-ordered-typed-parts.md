# 0044. Messages are ordered typed parts

- **Status:** Accepted
- **Date:** 2026-10-09
- **Source:** PR #86 (commit 1a7e6dd), recorded as D-44 in `DESIGN_DECISIONS.md`

## Decision

A message is an ordered list of parts, and every part has a kind: text, an image, a document, a tool call, a citation, a refusal, and so on. The role seals which kinds a message may hold, so a misplaced part is a compile error rather than a runtime surprise.
Media is one part however it is reached. Its `Source` can carry bytes, a URI, a workspace reference and uploads to several vendors at once, and each adapter picks the locator it can use. One vendor's file ID is useless to another, so a request that fails over still has a way to reach the media. Bytes are never persisted; stores keep the other locators and the digest.
Model output streams as parts too. Each part has an index, its position in the final message, so parts can interleave and an aggregator, a restorer or a UI keys its state by index instead of guessing which call a fragment belongs to. A part that breaks the protocol is counted and ignored, never guessed at.
Every store, the wire and the durable journal encode a part the same way: its fields plus a `type` tag. Kinds are never renamed (D-35). A reader rejects a kind it does not know, ignores fields it does not know, and the earlier wire version stays readable through an upgrader. Output an older reader cannot represent becomes an error it can see, never a silent drop (D-12).
