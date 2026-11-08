# Slack connector

`github.com/urmzd/saige/flow/connectors/slack` owns signature verification, mention and slash-command translation,
threaded message delivery, reaction APIs, and the optional `AgentAPI` methods
for session status and command registration. Ordinary messaging clients need
only implement `API`; agent-session features are a separate capability.

`Config.Prepare` lets an application check a verified event (`*event.Envelope`)
before it is published. The configured team boundary applies to mentions,
direct messages, stop events, and slash commands after signature verification.
A delivery from another team is acknowledged without invoking application
policy or publishing anything.

Session lifecycle policy, command discovery, refusal logging, app manifests
and installation configuration belong to the host. `slack.New(slack.Config{...})`
takes the API client, signing secret, team, app id and bot mapping; it enables
no product session or command policy.

## What Slack publishes

| Type | Raised by |
|------|-----------|
| `message.posted.v1` | A mention of the app or a direct message, with the agents it names (through `Bots`) in `Mentions` and everything it refers to in `References` |
| `slack.command.invoked.v1` | A slash command |
| `slack.session.stopped.v1` | A person pressing stop on an agent session |

Each is published on the conversation's channel,
`slack://<team>/<channel>[?thread=<ts>]`. The connector chooses no agent: the
channel's members and the host do, and the Slack sink answers on the
same channel, editing one message per run.

## What a message refers to

Slack writes a reference as markup, and the connector reduces each to what a
person saw, so the model reads `@ada`, `#general` and a link's label rather
than ids and angle brackets. What the reduction drops is kept in
`Request.References`, in the order written and once each:

| Markup | Text | Reference |
|--------|------|-----------|
| `<@U08BOT>`, a user in `Bots` | `@docs-bot`, then stripped | `agent`, name `docs-bot`, id `U08BOT` |
| `<@U07\|ada>`, any other user | `@ada` (`@U07` without a label) | `person`, name `@ada`, id `U07` |
| `<#C02\|general>` | `#general` | `place`, name `#general`, id `C02`, address `slack://<team>/C02` |
| `<https://...\|label>` | `label` (the URL without one) | `link`, the URL, its label, and its address when it has one |
| `<!here>`, `<!subteam^S1>` | `@here`, the label | none |

The app's own bot user, which every `app_mention` tags, is the addressee and
no reference; the connector knows it from the delivery's `authorizations`.
A link to a GitHub pull request or issue is addressed
`github://<owner>/<repo>/{pull|issues}/<n>`, whatever tail it has. A message
permalink is addressed as its thread,
`slack://<team>/<channel>?thread=<ts>`, only when `Config.Links.SlackTeams`
names the domain it was copied from; otherwise it keeps only its URL.
`Config.Links.Address` reads the host's own URLs, such as a link to a
session. The same rules hold for a slash command's text.

## Stopping a session

Slack sends `agent_session_stopped` when a person presses stop, and the
connector publishes it as `slack.session.stopped.v1` on the session's channel.
It starts nothing. The host maps it to a stop request (`Runtime.Stop`) for
each run it started on that channel, with the person who pressed stop as the
asker.

The runtime records a stop before it announces it when built with
`runtime.WithStopLedger`. The run then ends with
`run.stopped.v1`, and the sink edits the session's message to say so under
what the agent had written. The run id to stop is `Acceptance.RunID`, reported
and every event of the run carries, `runtime.RunKey(tenant, event id, agent)`.
It is not the Slack event id.

## Feedback ownership

Signed reaction additions and removals use Slack's `event_ts` to order changes
for one native message, actor and rating. `DeliveryState` retains the addition's
event ID and a removal watermark across replica changes. Older or ambiguous
equal-time deliveries cannot overwrite a newer reaction; a removal received
before its addition prevents that late addition from resurrecting feedback.
Missing or malformed event timestamps are ignored for ratings.

Removal calls optional `Feedback.Withdraw` with the original addition identity,
never generic `Record(Removed=true)`. The host must remove only the assessment
still owned by that addition, preserving newer console or native assessments
even when their rating matches. Without `Withdraw`, removal does nothing.
The ordering field follows Slack's [Events API contract](https://docs.slack.dev/apis/events-api/).
