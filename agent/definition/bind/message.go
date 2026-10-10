package bind

import (
	"strings"

	"github.com/urmzd/saige/agent/types"
)

// The two helpers below are the only code in this package that reads or
// builds message content, so a change to the message representation is
// confined to this file.

// userText joins the text of a user message.
func userText(m types.UserMessage) string {
	var parts []string
	for _, c := range m.Parts {
		if t, ok := c.(types.TextPart); ok {
			parts = append(parts, t.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// withPrefix returns m with the parts of prefix before its own.
func withPrefix(m, prefix types.UserMessage) types.UserMessage {
	parts := make([]types.UserPart, 0, len(prefix.Parts)+len(m.Parts))
	parts = append(parts, prefix.Parts...)
	parts = append(parts, m.Parts...)
	return types.UserMessage{Parts: parts}
}
