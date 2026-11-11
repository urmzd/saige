# Native GitHub feedback

`github.Config.Feedback` resolves only native handles the host recorded after
delivery. `Record` owns durable observation deduplication. Optional `Withdraw`
receives both the removal identity and original reaction identity; it must
remove only that observation's current assessment, preserving newer feedback.

Thread resolution feedback also retains its original addition identity and
resolver. Reopening calls `Withdraw` for that assessment, even when a different
person reopens it. It never calls `Record` with a removal. GitHub does not provide
a resolution timestamp in this webhook, and delivery order is not guaranteed.
`ReviewThreadAPI` therefore verifies current resolution state, resolver and
comment membership in the exact PR through a bounded GraphQL read. Stale events
are retained as consumed identities; at most 1,024 deliveries per finding are
tracked, and exceeding that limit fails closed. Missing proof or missing optional
`Withdraw` never falls back to deleting by rating. A re-resolution not separated
by an observed reopening is not inferred as another human assessment.

Plain review-comment replies require `ContinueThread` to recognize the exact
`review_comment` root. A known PR-level conversation does not grant unrelated
review threads. Explicit mentions can still start a new thread.

`PollFeedback` advances one persisted page per call, bounded to 100 pages of
100 reactions and 10,000 tracked identities. Additions are applied in ID order
after a complete scan. A removal needs two identical complete snapshots.
Failed or partial scans never remove feedback. `DeliveryState` keeps scan
progress and known identities across connector reconstruction. A nil store
keeps this state only in process. The host controls poll scheduling and age.
Offset pagination is not a transactional snapshot of a changing collection.

Native review delivery also persists an exact suggestion's reviewed commit,
path, line range, replacement and finding identity. Its indexing runs after the
delivery transaction; a durable pending flag retries failed indexing on another
replica without posting the native review again. `SuggestionCommitAPI`
and `FileAPI` permit optional verification after a push. The accepted case is
deliberately narrow: current head has exactly one parent, that parent is the
reviewed SHA, a diff patch names the target file, and complete before/after
file contents equal exactly the suggested replacement. The current head is
rechecked before recording `suggestion_committed`. The optional `Evidence`
callback receives a system observation with the immutable commit SHA. Commit
author and committer metadata are not trusted as human identity. Hosts must
persist this separately from personal ratings and evaluation candidates.

Recovery of a remotely submitted review happens before current-head or current
approval checks for a new write. Recovery requires the configured app author,
tenant/run/commit marker, exact original comment anchors and a canonical payload
digest bound to the immutable final answer. Remote original diff hunks permit
finding mappings to be rebuilt after the PR head advances. Optional `RecoverReview`
reproduces the disposition of a review posted earlier only after the payload
digest is verified; hosts must retain their current evidence-visibility checks. Changed
payloads, missing proof and older reviews without a payload digest are refused,
without creating a duplicate. New reviews still require the pinned current head, and are never approvals.

This records exact committed text, not a claim that GitHub's suggestion UI was
used, the finding was semantically correct, or the reviewer was approved.
Merges, non-adjacent commits, extra edits in the same file, incomplete files or
diffs, and moving heads remain unverified. A later revert does not erase the
historical verified commit. Outdated/resolved comment flags are not substitutes
for commit evidence. The commit query reads only its first bounded file page;
a target omitted from that page remains unverified.

`FixPullContext` is an optional host callback supplying canonical source-session
and alert URLs after authorization. These fields are excluded from action
arguments and native API JSON. They participate in fix-request identity and are
added to the draft PR alongside the independently verified source issue/PR.

API contracts checked against GitHub's official [reaction endpoints](https://docs.github.com/en/rest/reactions/reactions),
[commit endpoint](https://docs.github.com/en/rest/commits/commits#get-a-commit),
and [review-thread fields](https://docs.github.com/en/graphql/reference/pulls#pullrequestreviewthread).
GitHub documents [out-of-order delivery](https://docs.github.com/en/webhooks/testing-and-troubleshooting-webhooks/troubleshooting-webhooks#webhooks-deliveries-are-out-of-order).

Local race coverage includes snapshot pagination/failure/restart, removal replay,
exact and rejected commit evidence, and trusted backlink injection. Live GitHub
delivery was not exercised by these tests.
