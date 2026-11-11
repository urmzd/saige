package intake_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/attachment"
	"github.com/urmzd/saige/flow/attachment/intake"
	"github.com/urmzd/saige/flow/blob"
)

// storing returns a candidate that stores body, and counts its requests.
func storing(s blob.Store, name, body string, declared int64, calls *int) intake.Candidate {
	return intake.Candidate{Name: name, Size: declared, Fetch: func(ctx context.Context, tenant string, meta intake.Meta) (attachment.Attachment, error) {
		*calls++
		return intake.Store(ctx, s, tenant, strings.NewReader(body), meta)
	}}
}

func failing(name string, err error) intake.Candidate {
	return intake.Candidate{Name: name, Fetch: func(context.Context, string, intake.Meta) (attachment.Attachment, error) {
		return attachment.Attachment{}, err
	}}
}

// A message's files come back one per candidate, in order, each either stored
// or a reference saying why it is not, so a failure never stops the tag and
// never silently drops a file. The reason is a plain clause for the model.
func TestCollectKeepsEveryFileInOrderAndExplainsEachOneItCouldNotBringIn(t *testing.T) {
	s := blob.NewMemory()
	origin := address.MustParse("slack://T01/C02?thread=1.2")
	var calls int
	var skipped []intake.Skip
	files := []intake.Candidate{
		storing(s, "a.txt", "first", 0, &calls),
		failing("refused.png", fmt.Errorf("slack: %w: sign-in page", intake.ErrRefused)),
		failing("elsewhere.png", fmt.Errorf("%w: host %q is not on the allowlist", intake.ErrNotAllowed, "evil.example")),
		failing("private.png", fmt.Errorf("%w: 10.0.0.1 is not a public address", intake.ErrBlockedAddress)),
		failing("gone.png", fmt.Errorf("%w: 404 from files.slack.com", intake.ErrStatus)),
		failing("slow.png", context.DeadlineExceeded),
		failing("odd.png", errors.New("connection reset")),
		{Name: "nowhere.png"},
		storing(s, "b.txt", "second", 0, &calls),
	}
	got := intake.Collect(ctx, "acme", origin, intake.Caps{MaxFiles: 16}, files, func(sk intake.Skip) { skipped = append(skipped, sk) })
	want := []string{
		"",
		"the surface refused access to it",
		"it is hosted somewhere files are not fetched from",
		"its host resolves to an address files are not fetched from",
		"the surface did not return it",
		"fetching it took too long",
		"it could not be fetched",
		"the surface did not say where to fetch it from",
		"",
	}
	if len(got) != len(files) {
		t.Fatalf("Collect returned %d attachments for %d files", len(got), len(files))
	}
	for i, a := range got {
		if a.Unread != want[i] {
			t.Errorf("file %d (%s): unread = %q, want %q", i, files[i].Name, a.Unread, want[i])
		}
		if a.Name != files[i].Name || a.Origin.String() != origin.String() {
			t.Errorf("file %d lost its name or origin: %+v", i, a)
		}
		if err := a.Validate(); err != nil {
			t.Errorf("file %d: %v", i, err)
		}
	}
	if len(skipped) != 7 {
		t.Errorf("%d skips reported, want 7: %+v", len(skipped), skipped)
	}
	if calls != 2 {
		t.Errorf("%d stores, want 2", calls)
	}
}

// The caps decide before anything is requested whenever they can: past the
// file count, over a declared size, or over what the message has left.
func TestCollectAppliesItsCapsBeforeRequesting(t *testing.T) {
	s := blob.NewMemory()
	var calls int
	caps := intake.Caps{MaxFiles: 3, MaxFileBytes: 10, MaxTotalBytes: 12}
	files := []intake.Candidate{
		storing(s, "big.bin", "x", 11, &calls),             // declared over the per-file cap
		storing(s, "fits.txt", "0123456789", 0, &calls),    // 10 of 12
		storing(s, "over.txt", "0123", 0, &calls),          // only 2 left: refused by the store
		storing(s, "fourth.txt", "never asked", 0, &calls), // past the count
	}
	got := intake.Collect(ctx, "acme", address.Address{}, caps, files, nil)
	if got[0].Unread != "it is 11 bytes, over the 10 bytes limit for one file" {
		t.Errorf("declared oversize: %q", got[0].Unread)
	}
	if got[1].Unread != "" || got[1].Size != 10 {
		t.Errorf("a file inside the caps was not stored: %+v", got[1])
	}
	if got[2].Unread != "the message's files together are over the 12 bytes limit" {
		t.Errorf("total over: %q", got[2].Unread)
	}
	if got[3].Unread != "the message has more than 3 files, and only the first 3 are read" {
		t.Errorf("past the count: %q", got[3].Unread)
	}
	if calls != 2 {
		t.Errorf("%d requests, want 2: a declared oversize and a file past the count are never requested", calls)
	}
}

// One deadline covers the batch, so a slow surface delays a tag by a bounded
// amount however many files it carries.
func TestCollectSharesOneDeadlineAcrossTheMessage(t *testing.T) {
	slow := intake.Candidate{Name: "slow.bin", Fetch: func(ctx context.Context, _ string, _ intake.Meta) (attachment.Attachment, error) {
		<-ctx.Done()
		return attachment.Attachment{}, ctx.Err()
	}}
	start := time.Now()
	got := intake.Collect(ctx, "acme", address.Address{}, intake.Caps{Timeout: 50 * time.Millisecond}, []intake.Candidate{slow, slow, slow}, nil)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("three slow files took %s", elapsed)
	}
	for i, a := range got {
		if a.Unread != "fetching it took too long" {
			t.Errorf("file %d: %q", i, a.Unread)
		}
	}
}
