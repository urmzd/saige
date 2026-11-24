# GitHub connector

`github.com/urmzd/saige/flow/connectors/github` owns signature verification, normalized mentions and typed webhook
events, API context and file reads, comments and review-thread replies,
idempotent delivery, commit comparison, workflow API operations, and check-run
transport.

Application policy is supplied through `Config.Automation`, `Config.Prepare`,
`Config.Render`, and `Config.Conclusion`. These hooks do not bypass webhook
verification or repository confinement. `Prepare` receives the
`*event.Envelope` before it is published. With no hooks, the connector answers
with ordinary Markdown and exposes no `/fix` or `/check` tools.

Review policy, PR guides, signed mutation grants, check-run and summary
presentation, agent definitions and deployment configuration belong to the
host. `github.New(github.Config{...})` takes transport configuration: the API
client, the webhook verifier, owners, bot mappings, read repositories and
read hooks (see [what a read may reach](#what-a-read-may-reach)), whether to
publish checks, and the app's own login and numeric app ID. For the API client's token, see
[credentials](#credentials).

**The model never approves; a host hook may.** A review the model submits
through `github_create_review` is `COMMENT` or `REQUEST_CHANGES`: the tool
offers only the two, and `PrepareReview` refuses `APPROVE` with
`ErrNoApproval` whatever the host allows. The HTTP API and the fake refuse
`APPROVE` too, unless the call carries a grant that only the review sink can
make, for that one pull request and commit.

An approval comes only from the host, in two steps:

1. `Config.Review` plans the review, with `APPROVE` as its event.
2. `Config.MayApprove(ctx, req)` decides. `req` is an `ApprovalRequest`: the
   owner, repository, number, reviewed commit, author login and numeric
   `AuthorID` and draft flag of the pull request, the run ID, the
   connector's app slug (`App`) and numeric ID (`AppID`, from
   `Config.AppID`), the app's outstanding reviews on the pull request
   (`AppReviews`), and the review the sink would post. The sink asks once.

| Case | What the sink posts |
|------|---------------------|
| `MayApprove` is nil (the default) | Nothing: the delivery fails with `ErrNoApproval`, as before the hook |
| The pull request is closed or a draft | The same review as `COMMENT`, without asking the hook |
| The hook answers no | The same review as `COMMENT` |
| The hook returns an error | Nothing: the delivery is retried, and the hook is asked again |
| The hook answers yes | `APPROVE`, after it reads the pull request again and finds it open, not a draft, and on the reviewed commit; otherwise `COMMENT` |
| `Config.Login` names no app | The same review as `COMMENT`, without asking the hook |

A head that moved off the reviewed commit, before or while the hook decides,
is refused as `ErrUndeliverable`. The sink posts the approval under a grant
that the HTTP API and the fake spend on one post for that pull request and
commit. Recovery reads back an approval the host posted, so a retried
delivery matches it instead of posting another. Recovery never asks the hook:
it posts nothing.

After an approval is posted, the sink reads the pull request and the app's
reviews once more, and dismisses that approval when:

- the head is no longer the approved commit, because a push landed between
  the last read and the post;
- a change request of the app's account is outstanding that the hook was not
  shown, such as another agent's posted while the hook decided, which the
  approval would otherwise replace as the account's latest review; or
- either read fails.

These reads and the dismissal run without the delivery's cancellation, each
under its own time bound, so a delivery cancelled right after the post still
confirms it and a read that stalls still leaves the dismissal its time. The
sink keeps the IDs of the reviews the hook was shown with the delivery's own
state record, in the same write as the delivery, also when the post fails. When the post's response is lost and a retry
recovers the approval, the sink confirms it there against the same set; if
that set was not kept, any outstanding change request other than the
approval dismisses it. A failed dismissal is logged to `Config.Log` (or
`slog.Default()`), and the delivery still completes. `New` refuses a `Config` that sets `MayApprove`
with an API that does not implement `ReviewDismissAPI`. A push after these
reads is the host's to handle on `synchronize` (for example with
`Connector.DismissReview`), or GitHub's, with the branch rule that dismisses
stale approvals when new commits are pushed.

A host that requires passing checks reads them with `CommitChecks(ctx, owner,
repo, sha)`. It returns every check run (the latest of each), every check
suite, and every commit status (the latest of each context) on the commit,
as a `CommitCheck` with
its kind, name, status, conclusion, and for a check run the slug of the app
that published it and its external ID. It follows pages of 100 until one is
short, and fails when a kind has `MaxCommitChecks` (1000) or more, or when
GitHub answers for another commit. The model has no tool for this read.

Each check run and check suite also carries `AppID`, the numeric ID of the
app that published it (0 when GitHub's response has none), each check suite
`Runs`, its number of check runs, and each commit status `Creator`, the
login that set it. A check suite is read so that one queued before any of its
runs registered counts as pending: a suite that has not completed blocks,
with or without runs, and a suite never counts as a pass on its own. An app
that leaves its suite queued without ever reporting keeps it pending.

- `CommitCheck.Passed` is true for a completed check that concluded
  `success`. `CommitCheck.Blocks` is true for a check that has not completed,
  or that completed with any other conclusion, except that a check run that
  concluded `neutral` or `skipped` does not block. A commit status has no
  neutral state: it blocks unless it passed.
- `ApprovalRequest.OwnCheck(check)` is true for every check of the
  connector's own app, from any run and under any name: a check run whose
  `AppID` is `ApprovalRequest.AppID`, or whose app slug is
  `ApprovalRequest.App` (a slug names one app, and it is how a check run
  without an app ID in the response is recognized), and a commit status set
  by the app's bot account `<slug>[bot]`. The app's own result never counts
  toward an approval, so a finished run's `neutral` check, or an earlier
  run's, does not make a commit approvable.
- `ApprovalRequest.ChecksAllow(checks)` is the call a host makes. It is true
  only when the request names the app (`AppID` or `App`), at least one check
  that is not own passed with `success`, none blocks, no check run's app ID
  and slug disagree about being own, and the list is shorter than
  `MaxCommitChecks`. A check run of another app that concluded `neutral` or
  `skipped` neither satisfies the rule nor fails it. An empty list is not a
  pass: checks that have not registered yet are not a pass.
- `ApprovalRequest.ChecksAllowRequiring(checks, required)` also requires
  each `RequiredCheck` (at most `MaxRequiredChecks`, 100) to be present from
  its source: a check run of that name from the app `AppID`, or a commit
  status of that context set by the login `Creator`, each such check
  concluded `success`. A requirement names exactly one source and never the
  connector's own app. There is no requirement by name alone, since anyone
  who can write a commit status or run a workflow can report a check of any
  name. For a check a workflow defines, the app is GitHub Actions and the
  pull request's own workflow files may define it: branch protection, or a
  workflow run from the base branch (`pull_request_target`), is what ties
  such a check to the base branch's definition.
- `ApprovalRequest.AuthoredByApp()` reports whether the app's bot account
  opened the pull request; with no app named it reports true.
- `AllPassed(checks, own)` is the same rule with the own check given by the
  caller and without the app requirement; `AllPassed(checks, req.OwnCheck)`
  differs from `ChecksAllow` only there.

Every agent posts reviews as the same bot account, and GitHub counts only a
reviewer's latest approval or change request. An approval from one agent
therefore replaces a change request another agent left outstanding.
`ApprovalRequest.AppReviews` lists the bot account's outstanding approvals and
change requests on the pull request (`ID`, `State`, `CommitID`, and `Marker`,
the delivery marker of the run whose review the sink posted, empty for any
other); `ChangesRequested()` keeps the change requests. `ReviewMarker(tenant,
run, commit)` computes a run's marker, so a host matches reviews to the runs,
and so the agents, it keeps records of. A host refuses an approval while any
change request remains that it cannot attribute to the agent approving. The
model's `github_create_review` refuses a body or comment that carries one of
the sink's markers.

A host clears its own earlier verdict with `Connector.DismissReview(ctx,
owner, repo, number, reviewID, message)`, for example an approval left on an
older head after a push, or a change request that a later clean review
replaces. It reads the review first and refuses with `ErrNotOwnReview` unless
the app's bot account wrote it and it is an approval or a change request. The
bot account is `<slug>[bot]`, where the slug is `Config.Login` without
`[bot]`, compared ignoring case: with `Login` `humagen` or `humagen[bot]`, a
review by `humagen[bot]` is own and one by a person named `humagen` is not.
An empty `Login` owns nothing. A comment, a dismissed review, another login's
review and a review GitHub reports on another pull request are refused before
any dismissal is sent. `Connector.OwnReviews(ctx, owner, repo, number)` lists
the app's own reviews on a pull request with their ID, state, commit and
author; it fails when the pull request has 1000 or more reviews. No model
tool offers either. Both know the app, not the agent: a host attributes a
review to an agent from its own records before it dismisses one.

A host that refuses an approval asked for by someone who wrote the change
reads the pull request's commits with `PullRequestCommits(ctx, owner, repo,
number)` on the HTTP API or the fake. Each `PRCommit` has the SHA; the
numeric ID and login of the GitHub accounts linked to its author and
committer; the git author and committer emails (`AuthorEmail`,
`CommitterEmail`); `CoAuthors`, every `Co-authored-by: Name <email>` line
of the message, matched ignoring case anywhere in it, with space allowed
before the colon and lines ending at LF, CRLF or a lone CR. A line names one
co-author for each part between commas outside angle brackets, so
`Doe, Jo <jo@example.com>` names `Doe` without an email and `Jo`. A part's
`Email` is its first address in angle brackets that holds no other bracket,
whatever follows it (the innermost, so `Name<<a@example.com>>` gives
`a@example.com`), and empty when the part names no address; and `Verification`, GitHub's verdict on the commit's
signature (`Verified` and `Reason`; the REST response names no signer). A
commit that names more than `MaxCoAuthors` (50) fails the read. The git
author, committer and co-authors are whatever the commit's maker wrote: a
host that needs proof of who wrote a change requires every commit to be
verified. An ID of 0 is unknown and matches nobody. A host denies a
requested approval when any commit has an author or committer it cannot map
to a person (an ID of 0, or an ID, email or co-author it does not know), and
when the asker is any of them. GitHub commits merges, squashes and web edits
as its `web-flow` account (`WebFlowLogin`, `WebFlowID`);
`PRCommit.CommittedByWebFlow` tells that committer apart by ID and login, so a
host may ignore it as committer while it still checks the author. GitHub
lists at most 250 commits of a pull request, so the read fails at
`MaxPullRequestCommits` (250) rather than return a list that may be cut
short. The model has no tool for this read.

## What a webhook publishes

Each verified delivery becomes one typed event on the issue or pull request's
channel, `github://<owner>/<repo>/issues/<number>` or
`github://<owner>/<repo>/pull/<number>`. The type follows
`github.<event>.<action>.v1` with GitHub's own names:

| Type | Raised by |
|------|-----------|
| `github.issue_comment.created.v1` | A comment on an issue or pull request |
| `github.pull_request_review_comment.created.v1` | A review comment on a diff line |
| `github.pull_request_review.submitted.v1` | A submitted review |
| `github.issues.opened.v1`, `github.issues.labeled.v1` | A new or labeled issue |
| `github.pull_request.opened.v1`, `.ready_for_review.v1`, `.labeled.v1` | A pull request opened, marked ready, or labeled |
| `github.pull_request.review_requested.v1` | A review requested from an agent's login |
| `github.pull_request.synchronize.v1` | New commits pushed to a pull request |

Each body carries the logins and the numeric user IDs of the sender
(`Sender`, `SenderID`) and of the issue or pull request's author (`Author`,
`AuthorID`), and a review request also those of the requested reviewer
(`RequestedReviewer`, `RequestedReviewerID`). A login can be renamed and
reused; an ID cannot, so a host compares people by ID. An ID is 0, and left
out of the trigger context, when the payload has none or one below 1: 0 is
the only value for unknown, and an ID of 0 matches nobody. `ContextSubject.User`
carries the author's login and ID on a context read.

When checks are on, the head commit (`head_sha`) is pinned on the body before
it is published. A bot's delivery publishes nothing, except `synchronize`,
whose id is `github:commit:<repo>:<number>:<sha>` so a redelivery joins its run.

## Who a comment asks

A comment, a review, a review comment, or a new issue or pull request that
names an agent is published with every agent it names in `Mentions`. One that
names no agent but mentions the app's own login (`Config.Login`, such as
`humagen[bot]`, mentioned as `@humagen`) is addressed to the app, the way
Slack delivers only messages that mention the app, and is published naming no
agent. The app's mention is taken out of the text in both cases. Anything else
publishes nothing, so a busy repository wakes no agent on every comment, and a
bot's comment publishes nothing whatever it mentions.

The connector chooses no agent. The channel's members (`flow/membership`) and
the host decide who answers. A login in `Bots` names its agent wherever
it is mentioned, the app's own too, so when `Bots` maps the app's login,
`@humagen what changed?` mentions that agent.

## What a comment refers to

Besides the agents in `Mentions`, a person's text (a comment, a review, a
review comment, a new issue or pull request, or the description a review
request reads) is published with what it refers to in `Request.References`,
in the order written and once each:

| Written | Reference |
|---------|-----------|
| `@docs-bot`, a login `Bots` or `Agents` names | `agent`, id the login as written |
| `@ada`, any other login | `person`, id `ada` |
| `@humagen`, the app's own login | none |
| `#12` | `place`, address `github://<owner>/<repo>/issues/12` in this repository |
| `acme/web#12` | `place`, address `github://acme/web/issues/12` |
| `https://github.com/acme/web/pull/12/files#r5` | `link`, the URL, address `github://acme/web/pull/12` |
| a file or commit permalink | `link`, the URL with its line range, no address |

GitHub serves a pull request at its issue number too, and a short reference
does not say which it is, so `#12` is addressed as an issue. A team mention
(`@acme/reviewers`), an email address and anything inside code is not a
reference. A pasted Slack permalink is addressed as its thread when
`Config.Links.SlackTeams` names its domain, and `Config.Links.Address` reads
the host's own URLs. Automation text is the host's and refers to nothing.

## What a read may reach

The reads that take a reference (`github_read_context`, `github_read_file`,
`github_workflow_status`, `github_fix_base` and `Connector.ReadComments`)
parse it with `ParseReadReference` into an owner, a repository and a number,
and request that repository on the API's own base: a URL is never fetched.
A reference whose repository still ends in `.git` after one clone suffix is
taken off, or whose owner is longer than 39 characters or repository longer
than 100, is refused.

A run reads its own repository: the one its address names, whether the
address is the repository's or one of its issues or pull requests. Any other
repository is a cross-source read, decided in this order before any request
is made:

1. `Config.Owners`: an owner the connector does not serve is refused.
2. `Config.ReadRefused(ctx, owner, repo)`: true refuses the read whatever
   grants it, which keeps a repository to runs on its own issues and pull
   requests.
3. `Config.ReadRepositories`: an exact `owner/repo`, in any case, grants it.
4. `Config.MayRead(ctx, owner, repo)`: true grants it, for a host whose
   grants are not a fixed list, such as the repositories a GitHub App
   installation covers.

Otherwise the read is refused with a `*ReadError` (`errors.Is` reports
`connector.ErrUndeliverable`; `Kept` says `ReadRefused` decided). Each hook
is asked at most once for each repository a read reaches, with the owner and
repository that are requested. GitHub names are case-insensitive, so the
hooks receive them in lower case however the reference spells them, and a
host can compare them exactly. A host that sets
`MayRead` grants all five reads together, `github_fix_base` included; the
hooks are not told which read is asking.

A cross-source read is answered by the repository the grant was decided for
or not at all. GitHub answers a renamed or transferred repository with a
redirect; `HTTPAPI` does not follow one for these reads and fails with
`ErrReadRedirected`, with or without the hooks. The connector marks the
read's context with `ReadWhereNamed`.

A run's read of its own repository follows a redirect within that
repository. A redirect to another repository, such as an issue that was
transferred, is a cross-source read of that repository: steps 1 to 4 decide
it before it is requested, and a read they do not grant fails with
`ErrReadRedirected`. A redirect to a location that names no repository on
the API's own base, such as `/repositories/{id}`, is not followed. The
location is read as it will be requested, escaped: an owner or repository
with an escape, or a later segment that unescapes to `.`, `..` or a slash,
names no repository. `HTTPAPI`
asks `FollowReadRedirect` about every redirect, and another `API`
implementation that follows redirects asks it too.

Both hooks are nil by default: nil `MayRead` grants nothing more and nil
`ReadRefused` refuses nothing, so a `Config` without them grants what
`ReadRepositories` names and nothing else. The hooks are never asked for a
write (`Config.ActionRepositories` alone grants a cross-surface write), for a
run's own repository (only for another repository a redirect of its read
names), or for an address the host bound itself (`Deliver`, `Messages`,
`PollFeedback`). `github_create_review` and
`github_create_fix_pr` read their target to validate the write, under
`ActionRepositories` and without the hooks, and return none of what they
read.

With either hook set, a read action refuses arguments with a key given
twice, two keys that differ only by case, or a key its schema does not
declare, before decoding them.

`ReadComments(ctx, source, reference, want, keep)` reads only the latest
conversation comments of an issue or pull request, through the optional
`LatestCommentsAPI`: page 1, the page its `Link` header names last, and the
page before that when the last holds fewer than `want` comments `keep`
accepts. `HTTPAPI` makes at most three requests for it, each to the API's
own base for the repository named, and follows no redirect
(`ErrReadRedirected`), for a run's own repository too. It returns at most two
pages of 30. Only a page number is read from the `Link` header (up to
10000), and one answer is read up to 4 MiB. Comments deleted between the
requests are `ErrCommentsChanged`.

## Where the answer lands

The GitHub sink subscribes to `github://` channels with the run types, so a run
started by a comment is answered on the same issue or pull request: one comment
per run, edited as it streams, and a check run when checks are on. A run
started elsewhere reaches GitHub only through a subscriber that names its
channel.

## Credentials

The connectors take an `*http.Client` for their API calls. `flow/credential`
builds one that asks a `credential.Source` for the token on every request,
for a connector and a subject (`credential.App(installation)` or
`credential.User(person)`):

```go
api := github.NewHTTPAPI("", credential.Client(src, "github", credential.App(installation), nil))
```

The host's store is the `Source`; a hosted credential service can be another
without changing the connector. A subject with no credential gets
`credential.ErrNoCredential` and the request is never sent: a person's call
never falls back to the app's token.
