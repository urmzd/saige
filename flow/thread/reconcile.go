package thread

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/attachment"
	"github.com/urmzd/saige/flow/event"
)

// Message is one message as its surface holds it now.
type Message struct {
	// ID is the surface's own stable id for the message: a Slack timestamp,
	// a GitHub comment id. It is required, and it must not change when the
	// message is edited, because it is how the journal knows it has already
	// seen the message.
	ID string
	// Text is the message as it reads now. A message with neither text nor
	// files (a reaction, a join) is not journaled.
	Text string
	// Actor is who posted it.
	Actor event.Actor
	// At is when it was posted.
	At time.Time
	// Files brings in the files posted with the message, as attachment
	// references in surface order. Nil means the message has none.
	//
	// It is a function rather than a list because bringing a file in means
	// downloading it, and a surface lists the whole thread on every
	// reconciliation. Reconcile calls it only for a message the journal does
	// not hold yet, so each file is fetched once, when its message first
	// joins the journal. A file that cannot be fetched should come back as
	// attachment.Missing rather than as an error: an error abandons the whole
	// reconciliation, and the thread is then retried on the next tag.
	Files func(ctx context.Context) ([]attachment.Attachment, error)
	// Own marks a message this deployment posted itself: an agent's rendered
	// answer or its progress. Reconcile skips these. What an agent said
	// reaches the journal as the run's final answer, appended by the run;
	// the surface's copy is a rendering of that, possibly truncated or
	// reformatted, and journaling it too would record every answer twice.
	Own bool
}

// Lister is what a connector implements to let a thread be reconciled with
// its surface.
//
// Messages returns every message in the thread surface names, oldest first,
// as the surface holds them now. surface is a tag's Source address, unreduced,
// so the connector sees the same address it built. tenant scopes the call the
// way it scopes everything else: a connector serving several tenants must not
// answer from another one's credentials.
//
// A connector that implements Lister must also tell the runtime which surface
// message a tag is (see runtime.WithMessageID). Otherwise the tagging message
// is journaled once here, as a message someone posted, and once by its run.
type Lister interface {
	Messages(ctx context.Context, tenant string, surface address.Address) ([]Message, error)
}

// Reconcile appends every surface message the journal has not seen, in
// surface order, as user entries.
//
// It only appends. A message edited on the surface keeps the text the journal
// first saw, and a message deleted from the surface stays in the journal: a
// run already pinned to that history must be able to read it again unchanged,
// and a rewritten entry would move the first differing byte of every later
// request into the middle of the conversation. The surface stays the record
// of what the thread says today; the journal is the record of what the agent
// was shown.
//
// skip names surface messages to leave out. pkg/runtime passes the id of the
// message that raised the tag being accepted: that message is the run's new
// request, and it joins the journal when the run completes rather than
// appearing in the history the same run is pinned to.
//
// Reconcile is idempotent and safe to run concurrently with other appends: it
// is one Append, and Append recognizes the messages it already holds by their
// surface ids.
func Reconcile(ctx context.Context, store Store, lister Lister, key Key, surface address.Address, skip ...string) error {
	if store == nil || lister == nil {
		return fmt.Errorf("%w: reconcile needs a store and a lister", ErrInvalid)
	}
	if err := key.Validate(); err != nil {
		return err
	}
	if surface.IsZero() || key.OneShot() {
		// Nothing to list: the thread has no surface a second message could
		// have been posted on.
		return nil
	}
	msgs, err := lister.Messages(ctx, key.Tenant, surface)
	if err != nil {
		return fmt.Errorf("thread: reconcile %s: list surface: %w", key, err)
	}
	skipped := make(map[string]bool, len(skip))
	for _, id := range skip {
		skipped[id] = true
	}
	var known map[string]bool
	entries := make([]Entry, 0, len(msgs))
	for i, m := range msgs {
		if m.ID == "" {
			return fmt.Errorf("%w: reconcile %s: surface message %d of %d has no id", ErrInvalid, key, i+1, len(msgs))
		}
		if m.Own || skipped[m.ID] {
			continue
		}
		e := Entry{Role: RoleUser, Text: m.Text, Actor: m.Actor, SurfaceID: m.ID, At: m.At}
		if m.Files != nil {
			if known == nil {
				if known, err = heldSurfaceIDs(ctx, store, key); err != nil {
					return fmt.Errorf("thread: reconcile %s: %w", key, err)
				}
			}
			if !known[m.ID] {
				if e.Attachments, err = m.Files(ctx); err != nil {
					return fmt.Errorf("thread: reconcile %s: files of surface message %s: %w", key, m.ID, err)
				}
			}
		}
		if strings.TrimSpace(e.Text) == "" && len(e.Attachments) == 0 {
			continue
		}
		entries = append(entries, e)
	}
	if len(entries) == 0 {
		return nil
	}
	if _, err := store.Append(ctx, key, entries...); err != nil {
		return fmt.Errorf("thread: reconcile %s: %w", key, err)
	}
	return nil
}

// heldSurfaceIDs returns the surface message ids the thread already holds. It
// is read only when some listed message has files, which is what it exists to
// avoid fetching twice. A message appended between this read and the Append
// below is fetched once more and then recognized by Append, so a race costs a
// download and never a duplicate entry.
func heldSurfaceIDs(ctx context.Context, store Store, key Key) (map[string]bool, error) {
	head, err := store.Head(ctx, key)
	if err != nil {
		return nil, err
	}
	entries, err := store.History(ctx, key, head)
	if err != nil {
		return nil, err
	}
	held := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.SurfaceID != "" {
			held[e.SurfaceID] = true
		}
	}
	return held, nil
}

// Listers is a Lister that hands each surface to the Lister of the connector
// that owns it, by the address's scheme: "slack" to the Slack connector,
// "github" to GitHub's. A surface no connector lists (a schedule, a surface
// whose connector cannot read its history) has nothing to reconcile, which is
// an empty listing and not an error.
type Listers map[string]Lister

var _ Lister = Listers(nil)

// Messages implements Lister.
func (l Listers) Messages(ctx context.Context, tenant string, surface address.Address) ([]Message, error) {
	lister, ok := l[surface.Connector]
	if !ok || lister == nil {
		return nil, nil
	}
	return lister.Messages(ctx, tenant, surface)
}
