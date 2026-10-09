package tui

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

// TestRunVerbosePrintsApprovalHeaderOnce checks that a verbose session shows
// one approval header and one copy of the marker details: the renderer prints
// them, and the prompt that follows asks only its question.
func TestRunVerbosePrintsApprovalHeaderOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	a, _ := markedAgent()
	var out bytes.Buffer
	r := &Runner{Verbose: true, Template: TemplateMinimal, In: strings.NewReader("delete it\nn\n"), Out: &out}
	r.Output = NewStyledOutput(&out, &out, TemplateMinimal)
	if err := r.Run(ctx, a); err != nil {
		t.Fatalf("Run: %v", err)
	}
	text := out.String()
	headers := strings.Count(text, "Approval required") + strings.Count(text, "requires approval")
	if headers != 1 {
		t.Fatalf("approval headers = %d, want 1; output:\n%s", headers, text)
	}
	if n := strings.Count(text, "needs approval"); n != 1 {
		t.Fatalf("marker detail printed %d times, want 1; output:\n%s", n, text)
	}
	if !strings.Contains(text, approvalPrompt) {
		t.Fatalf("no approval question; output:\n%s", text)
	}
}
