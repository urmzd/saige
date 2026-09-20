package agent

import (
	"context"
	"fmt"

	"github.com/urmzd/saige/agent/types"
)

// HandoffContext separates the complete audit history from the next owner's
// input. Policies must treat Messages as read-only.
type HandoffContext struct {
	Entry    string
	Target   string
	Messages []types.Message
}

type HandoffContextPolicy interface {
	Select(context.Context, HandoffContext) ([]types.Message, error)
}

// OwnerContext is the default. Each owner sees the shared root instruction,
// its own prior turns, and transfer briefs. A new owner also receives the latest
// user task. Other owners' reasoning and tool transcripts stay in the audit tree.
// Returning to an owner resumes its previous view with the new transfer brief.
type OwnerContext struct{}

func (OwnerContext) Select(_ context.Context, request HandoffContext) ([]types.Message, error) {
	owner := request.Entry
	var out []types.Message
	var latestTask types.Message
	seen := request.Target == owner
	for i, message := range request.Messages {
		if i == 0 && message.Role() == types.RoleSystem {
			out = append(out, message)
			continue
		}
		var transfer *types.HandoffContent
		switch msg := message.(type) {
		case types.SystemMessage:
			for _, content := range msg.Content {
				if h, ok := content.(types.HandoffContent); ok {
					transfer = &h
				}
			}
		case types.UserMessage:
			var content []types.UserContent
			for _, block := range msg.Content {
				if h, ok := block.(types.HandoffContent); ok {
					transfer = &h
				} else {
					content = append(content, block)
				}
			}
			if len(content) > 0 {
				latestTask = types.UserMessage{Content: content}
			}
		}
		if transfer != nil {
			owner = transfer.To
			if owner == request.Target {
				if !seen && latestTask != nil {
					out = append(out, latestTask)
				}
				seen = true
				out = append(out, types.NewUserMessage(fmt.Sprintf("Handoff from %s to %s. Task brief or return data: %s", transfer.From, transfer.To, transfer.Reason)))
			}
			continue
		}
		if owner == request.Target {
			out = append(out, message)
		}
	}
	return out, nil
}

// FullHandoffContext explicitly preserves the previous shared-context behavior.
type FullHandoffContext struct{}

func (FullHandoffContext) Select(_ context.Context, request HandoffContext) ([]types.Message, error) {
	return append([]types.Message(nil), request.Messages...), nil
}

func WithHandoffContextPolicy(policy HandoffContextPolicy) AgentOption {
	return func(cfg *AgentConfig) { cfg.HandoffContextPolicy = policy }
}

func (a *Agent) selectHandoffContext(ctx context.Context, active activeContext, messages []types.Message) (activeContext, error) {
	if a.handoffs == nil {
		return active, nil
	}
	policy := a.cfg.HandoffContextPolicy
	if policy == nil {
		policy = OwnerContext{}
	}
	selected, err := policy.Select(ctx, HandoffContext{Entry: a.cfg.Name, Target: active.name, Messages: messages})
	if err != nil {
		return active, err
	}
	_, active.messages = a.prepareMessages(selected)
	if member := a.activeMember(active.name); member != nil && member.systemPrompt != "" {
		active.messages = overlaySystem(active.messages, member.systemPrompt)
	}
	return active, nil
}
