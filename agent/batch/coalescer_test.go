package batch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// TestCoalescerOneBatchPerRound checks that calls from every participant go
// out as one batch once all of them wait, and that each caller gets its own
// answer back.
func TestCoalescerOneBatchPerRound(t *testing.T) {
	v := newFakeVendor()
	c := NewCoalescer(fastRunner(v, NewMemoryStore()), WithMaxWait(time.Minute))
	ctx := context.Background()
	const n = 6
	leaves := make([]func(), n)
	for i := range leaves {
		leaves[i] = c.Join()
	}
	var wg sync.WaitGroup
	answers := make([]string, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer leaves[i]()
			kctx := WithKey(ctx, fmt.Sprintf("case-%d", i))
			ch, err := c.ChatStream(kctx, []types.Message{types.NewUserMessage(fmt.Sprintf("q%d", i))}, nil)
			if err != nil {
				t.Error(err)
				return
			}
			res := Collect("", ch)
			answers[i] = res.Text()
		}()
	}
	wg.Wait()
	if v.submitCount() != 1 {
		t.Fatalf("vendor submits = %d, want 1", v.submitCount())
	}
	for i, a := range answers {
		if a != fmt.Sprintf("answer to q%d", i) {
			t.Fatalf("answer %d = %q", i, a)
		}
	}
}

// TestCoalescerIdleWindow checks that without participants a batch is sent
// once calls stop arriving, and that judge-style Generate calls work.
func TestCoalescerIdleWindow(t *testing.T) {
	v := newFakeVendor()
	c := NewCoalescer(fastRunner(v, NewMemoryStore()), WithWindow(30*time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	out := make([]string, 3)
	for i := range out {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := c.Generate(WithKey(ctx, "judge"), fmt.Sprintf("p%d", i))
			if err != nil {
				t.Error(err)
			}
			out[i] = s
		}()
	}
	wg.Wait()
	for i, s := range out {
		if s != fmt.Sprintf("answer to p%d", i) {
			t.Fatalf("answer %d = %q", i, s)
		}
	}
	if v.submitCount() != 1 {
		t.Fatalf("vendor submits = %d, want 1", v.submitCount())
	}
	// Request IDs are the key and an ordinal.
	for _, b := range v.batches {
		if len(b.reqs) != 3 {
			t.Fatalf("batch size = %d", len(b.reqs))
		}
	}
	schema := json.RawMessage(`{"type":"object","properties":{"score":{"type":"number"}},"required":["score"]}`)
	if _, err := c.GenerateStructured(ctx, "p", schema); err != nil {
		t.Fatal(err)
	}
}

// TestCoalescerFailedRequest checks that a request the vendor failed reaches
// its caller as an error delta.
func TestCoalescerFailedRequest(t *testing.T) {
	v := newFakeVendor()
	v.outcome = func(types.BatchRequest) types.BatchOutcome { return types.BatchExpiredOutcome }
	c := NewCoalescer(fastRunner(v, NewMemoryStore()), WithWindow(time.Millisecond))
	ch, err := c.ChatStream(context.Background(), []types.Message{types.NewUserMessage("x")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	res := Collect("x", ch)
	if res.Err == nil || !strings.Contains(res.Err.Error(), "expired") {
		t.Fatalf("err = %v, want an expired request", res.Err)
	}
}

// TestCoalescerResumesSameBatch checks that a restarted run, sending the
// same calls under the same keys, resumes the recorded job.
func TestCoalescerResumesSameBatch(t *testing.T) {
	v := newFakeVendor()
	store := NewMemoryStore()
	for range 2 {
		c := NewCoalescer(fastRunner(v, store), WithWindow(time.Millisecond))
		if _, err := c.Generate(WithKey(context.Background(), "case-1"), "same prompt"); err != nil {
			t.Fatal(err)
		}
	}
	if v.submitCount() != 1 {
		t.Fatalf("vendor submits = %d, want 1", v.submitCount())
	}
}
