package event_test

import (
	"strings"
	"testing"

	"github.com/urmzd/saige/flow/event"
)

func TestValidTokenIsOneSegment(t *testing.T) {
	for s, want := range map[string]bool{
		"docs-bot":                               true,
		"Docs_Bot.v2":                            true,
		"a":                                      true,
		"":                                       false,
		".hidden":                                false,
		"a/b":                                    false,
		"a?b":                                    false,
		"a b":                                    false,
		"a:b":                                    false,
		"café":                                   false,
		strings.Repeat("a", event.MaxTokenLen):   true,
		strings.Repeat("a", event.MaxTokenLen+1): false,
	} {
		if got := event.ValidToken(s); got != want {
			t.Errorf("ValidToken(%q) = %v, want %v", s, got, want)
		}
	}
}
