package types

// CompactionTrigger names what started a compaction.
type CompactionTrigger string

const (
	// CompactionTriggerRule: the strategy's own rule, such as a message
	// count or a window size, fired before a turn.
	CompactionTriggerRule CompactionTrigger = "rule"
	// CompactionTriggerInputPressure: the next turn's input exceeded
	// CompactConfig.MaxInputTokens.
	CompactionTriggerInputPressure CompactionTrigger = "input_pressure"
	// CompactionTriggerRequested: a ConfigContent asked for CompactNow.
	CompactionTriggerRequested CompactionTrigger = "requested"
	// CompactionTriggerContextLength: the provider rejected the turn as
	// longer than its context window.
	CompactionTriggerContextLength CompactionTrigger = "context_length"
)

// CompactionContent records one compaction on the branch it created: the
// strategy that ran, the input size before and after, and what happened to
// each message of the branch it compacted, by node ID on that branch. It is
// metadata: stripped before the provider call.
type CompactionContent struct {
	// Strategy is the configured strategy, for example
	// "chain(clear_tool_results,summary)". Steps names the ones that changed
	// the history, in order.
	Strategy string            `json:"strategy"`
	Steps    []string          `json:"steps,omitempty"`
	Trigger  CompactionTrigger `json:"trigger"`
	// TokensBefore and TokensAfter measure the model-visible history with
	// the agent's tokenizer.
	TokensBefore int `json:"tokens_before"`
	TokensAfter  int `json:"tokens_after"`
	// FromBranch is the branch that was compacted; it keeps every message.
	FromBranch BranchID `json:"from_branch,omitempty"`
	// Kept messages were carried over verbatim, Selected ones were older
	// messages kept because they were relevant, and Cleared ones had tool
	// results replaced by stubs. Summarized messages are covered by the
	// summary; Dropped ones were removed without one.
	Kept       []NodeID `json:"kept,omitempty"`
	Selected   []NodeID `json:"selected,omitempty"`
	Cleared    []NodeID `json:"cleared,omitempty"`
	Summarized []NodeID `json:"summarized,omitempty"`
	Dropped    []NodeID `json:"dropped,omitempty"`
	// SummaryNode is the node on the new branch that holds the summary.
	SummaryNode NodeID `json:"summary_node,omitempty"`
}

func (CompactionContent) isSystemContent() {}

// CompactionDelta reports a compaction. The run continues on Branch, and
// NodeID is the node holding the CompactionContent record.
type CompactionDelta struct {
	Branch BranchID
	NodeID string
	Record CompactionContent
}

func (CompactionDelta) isDelta() {}
