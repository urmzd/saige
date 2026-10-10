// Package convert fits the parts of a request to the offering that serves
// it: the modality conversion policy.
//
// A model takes some media natively and not others. Instead of dropping a
// part it cannot take, or replacing it with a placeholder, every attempt is
// planned: each media part is native, lowered (an image in a tool result
// that Apply moves to a follow-up user message, for an endpoint whose tool
// results carry text only), converted by a
// permitted action, or rejected. Reject is the default. The modality dial
// (types.Dials.Modality) permits the other actions per modality, in order
// of preference:
//
//   - convert: same modality, another format (Transcode);
//   - transcribe: audio to text through a model that hears it (Transcribe);
//   - describe: an image or video to text through a vision model (Describe);
//   - extract: a document to text (Documents, Extract);
//   - omit: drop the part and send a notice in its place.
//
// PlanConversions is pure. Plan.Apply runs the conversions on a copy of the
// messages: the conversation keeps its original parts, and only the view
// sent to the provider is converted. Conversions are memoized by the
// policy's scope, the converter's Name@Version and the part's digest, run
// as durable steps ("convert:<digest>:<converter>"), and charged to the
// attempt's budget reservation.
//
// The Provider decorator does all of this per call; provider.Build wraps
// every adapter with it, and the agent loop supplies its policy, dial
// layers, durable runner and budget through WithRuntime. A router plans
// each member through types.ConversionPlanner while it filters candidates,
// so a member that would reject leaves the request and a member that would
// convert stays.
//
// The planner also keeps reasoning a target cannot verify out of the view:
// a thinking part signed by another provider, or unsigned, is never
// replayed to the Anthropic Messages API (types.ConversionPolicy.Thinking).
package convert
