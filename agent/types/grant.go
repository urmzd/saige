package types

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

// GrantScope says how far an approval reaches beyond the call it answers.
type GrantScope string

const (
	// GrantOnce approves only the call that was asked about. It is the
	// meaning of a decision without a grant.
	GrantOnce GrantScope = "once"
	// GrantTool approves every later call of the same tool in the
	// conversation.
	GrantTool GrantScope = "tool"
	// GrantArgs approves later calls of the same tool whose arguments match
	// every matcher, such as a path under one directory.
	GrantArgs GrantScope = "args"
	// GrantSession approves every later call that would ask, of any tool, in
	// the conversation.
	GrantSession GrantScope = "session"
)

// ErrInvalidGrant reports a grant request that cannot be applied.
var ErrInvalidGrant = errors.New("invalid grant")

// ArgMatch matches one argument of a tool call. Set exactly one condition.
type ArgMatch struct {
	// Field names the argument. Dots step into nested objects:
	// "target.path" reads args["target"]["path"].
	Field string `json:"field"`
	// Equals matches a value whose JSON text, or a string's own text, equals
	// it: "42", "true" and "main" match 42, true and "main".
	Equals string `json:"equals,omitempty"`
	// Prefix matches a string that starts with it.
	Prefix string `json:"prefix,omitempty"`
	// PathPrefix matches a slash-separated path at or below it, after both
	// are cleaned, so "/srv/app/../etc" never matches "/srv/app".
	PathPrefix string `json:"path_prefix,omitempty"`
}

func (m ArgMatch) validate() error {
	set := 0
	for _, c := range []string{m.Equals, m.Prefix, m.PathPrefix} {
		if c != "" {
			set++
		}
	}
	if m.Field == "" || set != 1 {
		return fmt.Errorf("%w: a matcher needs a field and exactly one condition", ErrInvalidGrant)
	}
	return nil
}

// Matches reports whether args satisfy the matcher. A missing argument
// never matches.
func (m ArgMatch) Matches(args map[string]any) bool {
	v, ok := lookupArg(args, m.Field)
	if !ok || v == nil {
		return false
	}
	switch {
	case m.Equals != "":
		if s, ok := v.(string); ok {
			return s == m.Equals
		}
		raw, err := json.Marshal(v)
		return err == nil && string(raw) == m.Equals
	case m.Prefix != "":
		s, ok := v.(string)
		return ok && strings.HasPrefix(s, m.Prefix)
	case m.PathPrefix != "":
		s, ok := v.(string)
		if !ok || s == "" {
			return false
		}
		p, root := path.Clean(s), path.Clean(m.PathPrefix)
		return p == root || strings.HasPrefix(p, strings.TrimSuffix(root, "/")+"/")
	}
	return false
}

func lookupArg(args map[string]any, field string) (any, bool) {
	var cur any = args
	for part := range strings.SplitSeq(field, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = obj[part]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// GrantRequest is the scope a person attaches to an approval. Only the host
// can supply it, on ApprovalDecision or agent.Resolution: nothing the model
// writes can create one.
type GrantRequest struct {
	Scope GrantScope `json:"scope"`
	// Match lists the matchers of a GrantArgs grant. All must match.
	Match []ArgMatch `json:"match,omitempty"`
	// ExpiresAt ends the grant. Zero lasts for the conversation.
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}

// Validate checks the request: a known scope, and matchers exactly when the
// scope is GrantArgs.
func (g GrantRequest) Validate() error {
	switch g.Scope {
	case GrantOnce, GrantTool, GrantSession:
		if len(g.Match) > 0 {
			return fmt.Errorf("%w: only an %q grant takes matchers", ErrInvalidGrant, GrantArgs)
		}
	case GrantArgs:
		if len(g.Match) == 0 {
			return fmt.Errorf("%w: an %q grant needs at least one matcher", ErrInvalidGrant, GrantArgs)
		}
		for _, m := range g.Match {
			if err := m.validate(); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("%w: unknown scope %q", ErrInvalidGrant, g.Scope)
	}
	return nil
}

// Grant is a standing approval recorded in the conversation.
type Grant struct {
	// ID is derived from the call whose approval created the grant, so a
	// replay records the same ID.
	ID    string     `json:"id"`
	Scope GrantScope `json:"scope"`
	// Tool is the tool the grant covers, empty for GrantSession.
	Tool      string     `json:"tool,omitempty"`
	Match     []ArgMatch `json:"match,omitempty"`
	ExpiresAt time.Time  `json:"expires_at,omitzero"`
	// GrantedBy is the approver of the decision that created it.
	GrantedBy string `json:"granted_by,omitempty"`
	// ToolCallID is the call whose approval created it.
	ToolCallID string `json:"tool_call_id"`
}

// NewGrant builds the grant a decision on the call callID of tool creates.
// It reports false for GrantOnce, which creates nothing lasting.
func NewGrant(req GrantRequest, tool, callID, approver string) (Grant, bool) {
	if req.Scope == GrantOnce || req.Scope == "" {
		return Grant{}, false
	}
	sum := sha256.Sum256([]byte(callID + "\x00" + tool + "\x00" + string(req.Scope)))
	g := Grant{
		ID:         "grant_" + hex.EncodeToString(sum[:8]),
		Scope:      req.Scope,
		Match:      append([]ArgMatch(nil), req.Match...),
		ExpiresAt:  req.ExpiresAt,
		GrantedBy:  approver,
		ToolCallID: callID,
	}
	if req.Scope != GrantSession {
		g.Tool = tool
	}
	return g, true
}

// Covers reports whether the grant approves a call of tool with args at now.
func (g Grant) Covers(tool string, args map[string]any, now time.Time) bool {
	if !g.ExpiresAt.IsZero() && !now.Before(g.ExpiresAt) {
		return false
	}
	switch g.Scope {
	case GrantSession:
		return true
	case GrantTool:
		return g.Tool == tool
	case GrantArgs:
		if g.Tool != tool || len(g.Match) == 0 {
			return false
		}
		for _, m := range g.Match {
			if !m.Matches(args) {
				return false
			}
		}
		return true
	}
	return false
}

// ApprovalEvent names what an ApprovalPart records.
type ApprovalEvent string

const (
	// ApprovalEventApproved means a person approved the call, for it alone.
	ApprovalEventApproved ApprovalEvent = "approved"
	// ApprovalEventGranted means a person approved the call and granted a scope.
	ApprovalEventGranted ApprovalEvent = "granted"
	// ApprovalEventDenied means a person denied the call.
	ApprovalEventDenied ApprovalEvent = "denied"
	// ApprovalEventAutoApproved means a grant or the approval ramp approved the
	// call without asking.
	ApprovalEventAutoApproved ApprovalEvent = "auto_approved"
	// ApprovalEventAutoDenied means the call was refused without asking, because
	// people denied the tool too often.
	ApprovalEventAutoDenied ApprovalEvent = "auto_denied"
	// ApprovalEventSnapshot carries the whole policy state forward onto a
	// branch that compaction created, since the records it summarized away
	// are no longer on that branch. It replaces any state before it.
	ApprovalEventSnapshot ApprovalEvent = "snapshot"
)

// ApprovalPart records one decision of an approval policy in the tree,
// next to the result of the call it concerns. The policy's state, its
// grants and its approval and denial counts, is rebuilt from these records,
// so a restored or replayed conversation decides the same way. It is
// metadata: stripped before the provider call. It is system content only,
// and the loop reads it only from the tool result messages it writes.
type ApprovalPart struct {
	Event      ApprovalEvent `json:"event"`
	Tool       string        `json:"tool"`
	ToolCallID string        `json:"tool_call_id"`
	// Grant is the grant an ApprovalEventGranted created.
	Grant *Grant `json:"grant,omitempty"`
	// GrantID names the grant behind an ApprovalEventAutoApproved, empty
	// when the ramp approved the call.
	GrantID  string `json:"grant_id,omitempty"`
	Approver string `json:"approver,omitempty"`
	Reason   string `json:"reason,omitempty"`
	// Grants, Approvals and Denials are the state an ApprovalEventSnapshot
	// carries: every grant, and the approval and denial counts per tool.
	Grants    []Grant        `json:"grants,omitempty"`
	Approvals map[string]int `json:"approvals,omitempty"`
	Denials   map[string]int `json:"denials,omitempty"`
}

// Kind implements Part.
func (ApprovalPart) Kind() PartKind { return KindApproval }
func (ApprovalPart) isPart()        {}
func (ApprovalPart) isSystemPart()  {}

// ApprovalVerdict is an approval policy's decision about one call before
// anyone is asked. A durable runner records it as a step, so a replay
// decides the same way even after a grant expired.
type ApprovalVerdict struct {
	// Outcome is "ask", "approve" or "deny".
	Outcome string
	// Grant is the ID of the grant that approved the call.
	Grant  string
	Reason string
}

// Approval verdict outcomes.
const (
	VerdictAsk     = "ask"
	VerdictApprove = "approve"
	VerdictDeny    = "deny"
)
