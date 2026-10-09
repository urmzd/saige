package types

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateChannel(t *testing.T) {
	for _, ok := range []string{"a", "saige.durable.local", "cache:response-v1", "A_9", strings.Repeat("x", MaxChannelLen)} {
		if err := ValidateChannel(ok); err != nil {
			t.Errorf("ValidateChannel(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "with space", `quote"`, "semi;", "nul\x00", "é", strings.Repeat("x", MaxChannelLen+1)} {
		if err := ValidateChannel(bad); !errors.Is(err, ErrInvalidChannel) {
			t.Errorf("ValidateChannel(%q) = %v, want ErrInvalidChannel", bad, err)
		}
	}
}
