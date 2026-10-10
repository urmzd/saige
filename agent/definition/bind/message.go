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
	for _, c := range m.Content {
		if t, ok := c.(types.TextContent); ok {
			parts = append(parts, t.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// withPrefix returns m with the content of prefix before its own.
func withPrefix(m, prefix types.UserMessage) types.UserMessage {
	content := make([]types.UserContent, 0, len(prefix.Content)+len(m.Content))
	content = append(content, prefix.Content...)
	content = append(content, m.Content...)
	return types.UserMessage{Content: content}
}
