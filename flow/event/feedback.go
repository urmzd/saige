package event

import "slices"

// TypeFeedback records an assessment or verified evidence for a delivered
// answer. It is an authenticated observation, not execution output. Hosts
// append it through their feedback writer; connectors must not assign run
// sequences. System evidence must remain distinct from personal ratings.
const TypeFeedback = "run.feedback.v1"

// Feedback carries identical semantics on every surface. RunID names the
// assessed run independently of the feedback event's own delivery sequence.
type Feedback struct {
	RunID     string   `json:"run"`
	SessionID string   `json:"session,omitempty"`
	MessageID string   `json:"message,omitempty"`
	FindingID string   `json:"finding,omitempty"`
	Actor     Actor    `json:"actor"`
	Rating    string   `json:"rating"`
	Reasons   []string `json:"reasons,omitempty"`
	Comment   string   `json:"comment,omitempty"`
	Surface   string   `json:"surface"`
	Removed   bool     `json:"removed,omitempty"`
}

func (Feedback) Type() string { return TypeFeedback }
func (Feedback) isBody()      {}
func init() {
	Register(Entry{Type: TypeFeedback, Sensitive: true, OptIn: true, New: func() Body { return new(Feedback) }, Render: render(func(_ Envelope, b Feedback) string {
		if b.Actor.Bot && slices.Contains(b.Reasons, "suggestion_committed") {
			return line("verified suggestion commit on run", b.RunID, b.Comment)
		}
		return line("feedback on run", b.RunID, b.Rating, b.Comment)
	})})
}
