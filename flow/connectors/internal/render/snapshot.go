package render

import (
	"encoding/json"
	"fmt"
)

// snapshot retains the reorder window and sequence metadata as well as the
// visible document. Saving only View loses deduplication and late fragments.
type snapshot struct {
	Format                                   int
	Window                                   int
	Agent, RunID, Version, Channel, Tenant   string
	Status                                   Status
	StatusSeq                                uint64
	Error, Reason, FinalText, Text, Thinking string
	Question, Waiting                        *Question
	Answered                                 *Answered
	Pending                                  []snapshotPiece
	Mark                                     uint64
	Seen                                     map[uint64]struct{}
	Tools                                    []snapshotTool
	Cites                                    []snapshotCitation
	Actions                                  []snapshotAction
	Seq                                      uint64
	At                                       json.RawMessage
}
type snapshotPiece struct {
	Seq    uint64
	Stream stream
	Text   string
}
type snapshotTool struct {
	Tool Tool
	Seq  uint64
}
type snapshotCitation struct {
	Citation Citation
	Seq      uint64
}
type snapshotAction struct {
	Action Action
	Seq    uint64
}

// MarshalJSON encodes the full resumable rendering state.
func (d *Doc) MarshalJSON() ([]byte, error) {
	at, err := json.Marshal(d.at)
	if err != nil {
		return nil, err
	}
	s := snapshot{Format: 1, Window: d.Window, Agent: d.agent, RunID: d.runID, Version: d.version, Channel: d.channel, Tenant: d.tenant, Status: d.status, StatusSeq: d.statusSeq, Error: d.errText, Reason: d.reason, FinalText: d.finalText, Text: d.text.String(), Thinking: d.thinking.String(), Question: d.question, Waiting: d.waiting, Answered: d.answered, Mark: d.mark, Seen: d.seen, Seq: d.seq, At: at}
	for _, p := range d.pending {
		s.Pending = append(s.Pending, snapshotPiece{p.seq, p.str, p.txt})
	}
	for _, t := range d.tools {
		s.Tools = append(s.Tools, snapshotTool{t, t.seq})
	}
	for _, c := range d.cites {
		s.Cites = append(s.Cites, snapshotCitation{c, c.seq})
	}
	for _, a := range d.actions {
		s.Actions = append(s.Actions, snapshotAction{a, a.seq})
	}
	return json.Marshal(s)
}

// UnmarshalJSON restores a snapshot, refusing an unknown format.
func (d *Doc) UnmarshalJSON(data []byte) error {
	var s snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s.Format != 1 {
		return fmt.Errorf("render: unsupported snapshot format %d", s.Format)
	}
	*d = Doc{Window: s.Window, agent: s.Agent, runID: s.RunID, version: s.Version, channel: s.Channel, tenant: s.Tenant, status: s.Status, statusSeq: s.StatusSeq, errText: s.Error, reason: s.Reason, finalText: s.FinalText, question: s.Question, waiting: s.Waiting, answered: s.Answered, mark: s.Mark, seen: s.Seen, seq: s.Seq, toolAt: map[string]int{}, citeAt: map[string]int{}}
	if err := json.Unmarshal(s.At, &d.at); err != nil {
		return err
	}
	d.text.WriteString(s.Text)
	d.thinking.WriteString(s.Thinking)
	for _, p := range s.Pending {
		d.pending = append(d.pending, piece{p.Seq, p.Stream, p.Text})
	}
	for _, t := range s.Tools {
		t.Tool.seq = t.Seq
		d.tools = append(d.tools, t.Tool)
		if t.Tool.ID != "" {
			d.toolAt[t.Tool.ID] = len(d.tools) - 1
		}
	}
	for _, c := range s.Cites {
		c.Citation.seq = c.Seq
		d.cites = append(d.cites, c.Citation)
		d.citeAt[c.Citation.URI+"\x00"+c.Citation.Title] = len(d.cites) - 1
	}
	for _, a := range s.Actions {
		a.Action.seq = a.Seq
		d.actions = append(d.actions, a.Action)
	}
	return nil
}
