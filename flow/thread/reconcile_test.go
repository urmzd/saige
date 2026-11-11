package thread_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/attachment"
	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/thread"
	"github.com/urmzd/saige/flow/thread/threadtest"
)

var slackSource = address.MustParse("slack://T01/C02?thread=1699.001")

func reconcile(t *testing.T, s thread.Store, surface *threadtest.Surface, key thread.Key, skip ...string) {
	t.Helper()
	if err := thread.Reconcile(context.Background(), s, surface, key, slackSource, skip...); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

// People talk in a thread without tagging the agent. Reconciling brings what
// they said into the journal, in the order the surface holds it, once.
func TestReconcileAppendsUnseenSurfaceMessagesInSurfaceOrder(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	surface := threadtest.NewSurface()
	key := slackThread("docs-bot")

	first := surface.Post(tenantA, slackSource, "ada", "the deploy failed")
	surface.Post(tenantA, slackSource, "grace", "which stage?")
	reconcile(t, s, surface, key)

	got := mustHistory(t, s, key)
	if !reflect.DeepEqual(texts(got), []string{"the deploy failed", "which stage?"}) {
		t.Fatalf("journal = %q", texts(got))
	}
	if e := got[0]; e.Role != thread.RoleUser || e.SurfaceID != first || e.RunID != "" ||
		e.Actor != (event.Actor{ID: "ada", Display: "ada"}) || e.At.IsZero() {
		t.Errorf("a reconciled message lost who said it, when, or which message it is: %+v", e)
	}

	// Reconciling again, with nothing new, changes nothing. With something
	// new, only that is appended, after what was there.
	reconcile(t, s, surface, key)
	surface.Post(tenantA, slackSource, "ada", "the migrate stage")
	reconcile(t, s, surface, key)
	reconcile(t, s, surface, key)
	if got := texts(mustHistory(t, s, key)); !reflect.DeepEqual(got, []string{"the deploy failed", "which stage?", "the migrate stage"}) {
		t.Fatalf("journal after re-reconciling = %q", got)
	}
}

// Append only. An edit or a delete on the surface is real, and it still does
// not rewrite history: a run pinned to these entries reads them again
// unchanged, and so does a prefix cache.
func TestReconcileNeverRewritesWhatTheJournalHolds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	surface := threadtest.NewSurface()
	key := slackThread("docs-bot")

	edited := surface.Post(tenantA, slackSource, "ada", "ship it on friday")
	deleted := surface.Post(tenantA, slackSource, "grace", "said in haste")
	surface.Post(tenantA, slackSource, "ada", "third")
	reconcile(t, s, surface, key)
	before, err := s.History(ctx, key, 3)
	if err != nil {
		t.Fatalf("History: %v", err)
	}

	surface.Edit(tenantA, slackSource, edited, "ship it on monday")
	surface.Delete(tenantA, slackSource, deleted)
	surface.Post(tenantA, slackSource, "grace", "fourth")
	reconcile(t, s, surface, key)

	after, err := s.History(ctx, key, 3)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("an edit or delete on the surface rewrote the journal:\n got %+v\nwant %+v", after, before)
	}
	if got := texts(mustHistory(t, s, key)); !reflect.DeepEqual(got, []string{"ship it on friday", "said in haste", "third", "fourth"}) {
		t.Fatalf("journal = %q, want the original three and the new message appended", got)
	}
}

// What the agent itself posted, and the message being answered right now, are
// not things someone else said in the thread.
func TestReconcileSkipsTheAgentsOwnOutputAndTheMessageBeingAnswered(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	surface := threadtest.NewSurface()
	key := slackThread("docs-bot")

	surface.Post(tenantA, slackSource, "ada", "context")
	surface.PostOwn(tenantA, slackSource, "the agent's rendered answer")
	surface.Post(tenantA, slackSource, "grace", "")
	tagging := surface.Post(tenantA, slackSource, "ada", "@docs-bot what now?")
	reconcile(t, s, surface, key, tagging)

	if got := texts(mustHistory(t, s, key)); !reflect.DeepEqual(got, []string{"context"}) {
		t.Fatalf("journal = %q, want only what someone else said", got)
	}

	// When the run that message raised completes, it appends the message
	// itself, and a later reconciliation recognizes it rather than adding it
	// a second time.
	q := user("run-1", "what now?")
	q.SurfaceID = tagging
	mustAppend(t, s, key, q, assistant("run-1", "do this"))
	reconcile(t, s, surface, key)
	if got := texts(mustHistory(t, s, key)); !reflect.DeepEqual(got, []string{"context", "what now?", "do this"}) {
		t.Fatalf("journal after the run and a second reconciliation = %q", got)
	}
}

// A surface is per tenant and per thread, and a reconciliation reads only the
// one it was asked about.
func TestReconcileStaysInsideItsTenantAndThread(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	surface := threadtest.NewSurface()
	key := slackThread("docs-bot")
	elsewhere := address.MustParse("slack://T01/C02?thread=1700.009")

	surface.Post(tenantA, slackSource, "ada", "ours")
	surface.Post(tenantB, slackSource, "mallory", "another tenant's")
	surface.Post(tenantA, elsewhere, "grace", "another thread's")
	reconcile(t, s, surface, key)

	if got := texts(mustHistory(t, s, key)); !reflect.DeepEqual(got, []string{"ours"}) {
		t.Fatalf("journal = %q, want only this tenant's messages in this thread", got)
	}
}

// A surface that cannot be listed, or that returns something the journal
// could not recognize later, fails the reconciliation and writes nothing.
func TestReconcileFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	key := slackThread("docs-bot")

	down := threadtest.NewSurface()
	down.Post(tenantA, slackSource, "ada", "unreachable")
	outage := errors.New("slack is down")
	down.Fail(outage)
	if err := thread.Reconcile(ctx, s, down, key, slackSource); !errors.Is(err, outage) {
		t.Errorf("Reconcile against a failing surface = %v, want the surface's error", err)
	}

	noID := listerFunc(func(context.Context, string, address.Address) ([]thread.Message, error) {
		return []thread.Message{{ID: "m1", Text: "fine"}, {Text: "no id"}}, nil
	})
	if err := thread.Reconcile(ctx, s, noID, key, slackSource); !errors.Is(err, thread.ErrInvalid) {
		t.Errorf("Reconcile of a message with no id = %v, want ErrInvalid", err)
	}
	if err := thread.Reconcile(ctx, s, nil, key, slackSource); !errors.Is(err, thread.ErrInvalid) {
		t.Errorf("Reconcile with no lister = %v, want ErrInvalid", err)
	}
	if head, err := s.Head(ctx, key); err != nil || head != 0 {
		t.Fatalf("a failed reconciliation wrote something: head = %d, %v", head, err)
	}

	// A thread with no surface has nothing to list, and the lister is not
	// asked.
	oneShot := thread.KeyOf(event.Envelope{ID: "Ev1", Tenant: tenantA, Source: "test"}, "docs-bot")
	if err := thread.Reconcile(ctx, s, down, oneShot, address.Address{}); err != nil {
		t.Errorf("Reconcile of a one-shot thread = %v, want nil", err)
	}
	if n := down.Lists(); n != 1 {
		t.Errorf("the surface was listed %d times, want once: a one-shot thread has nothing to list", n)
	}
}

type listerFunc func(context.Context, string, address.Address) ([]thread.Message, error)

func (f listerFunc) Messages(ctx context.Context, tenant string, surface address.Address) ([]thread.Message, error) {
	return f(ctx, tenant, surface)
}

// Files posted in a thread reach the journal with their message, in order, and
// each message's files are brought in once: when the message first joins the
// journal, never again on a later reconciliation, and never for the message
// being answered or one the deployment posted itself. A message that is only
// a file is still a message.
func TestReconcileBringsInEachMessagesFilesOnceAndInOrder(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	surface := threadtest.NewSurface()
	key := slackThread("docs-bot")
	shot := attachment.Attachment{Digest: strings.Repeat("a", 64), MediaType: "image/png", Name: "shot.png", Size: 10}
	trace := attachment.Attachment{Digest: strings.Repeat("b", 64), MediaType: "text/plain", Name: "trace.txt", Size: 5}
	refused := attachment.Missing("big.mov", slackSource, "", "it is larger than the 25.0 MB limit for one file")

	surface.PostFiles(tenantA, slackSource, "ada", "here is the failure", shot, trace)
	surface.PostFiles(tenantA, slackSource, "grace", "", refused)
	surface.PostFiles(tenantA, slackSource, "ada", "", nil...)
	tagging := surface.PostFiles(tenantA, slackSource, "ada", "@docs-bot why?", shot)
	reconcile(t, s, surface, key, tagging)
	reconcile(t, s, surface, key, tagging)

	got := mustHistory(t, s, key)
	if len(got) != 2 {
		t.Fatalf("journal = %+v, want the two messages that said or attached something", got)
	}
	if !reflect.DeepEqual(got[0].Attachments, []attachment.Attachment{shot, trace}) || got[0].Text != "here is the failure" {
		t.Errorf("first message = %+v", got[0])
	}
	if !reflect.DeepEqual(got[1].Attachments, []attachment.Attachment{refused}) || got[1].Text != "" {
		t.Errorf("a file-only message = %+v", got[1])
	}
	// Three messages not yet journaled were fetched on the first pass (the
	// empty one has nothing, but its message was new); the second pass
	// fetched only the one with nothing to journal, which never joins.
	if n := surface.Fetches(); n != 4 {
		t.Errorf("files were brought in %d times over two reconciliations, want 4: once per new message, plus the empty one again", n)
	}
}

// A surface whose files cannot be listed fails the reconciliation as a whole,
// which the runtime logs and moves past, and writes nothing half way.
func TestReconcileWritesNothingWhenAMessagesFilesFail(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	key := slackThread("docs-bot")
	boom := errors.New("files.info: ratelimited")
	lister := listerFunc(func(context.Context, string, address.Address) ([]thread.Message, error) {
		return []thread.Message{
			{ID: "1", Text: "first"},
			{ID: "2", Text: "second", Files: func(context.Context) ([]attachment.Attachment, error) { return nil, boom }},
		}, nil
	})
	if err := thread.Reconcile(context.Background(), s, lister, key, slackSource); !errors.Is(err, boom) {
		t.Fatalf("Reconcile = %v, want the files error", err)
	}
	if got := mustHistory(t, s, key); len(got) != 0 {
		t.Fatalf("a failed reconciliation wrote %+v", got)
	}
}

// The dispatcher hands a surface to its own connector's lister, with the
// tenant unchanged, and a surface no connector lists reconciles to nothing.
func TestListersDispatchesOnTheSurfacesConnector(t *testing.T) {
	t.Parallel()
	var asked []string
	lister := func(name string) thread.Lister {
		return listerFunc(func(_ context.Context, tenant string, surface address.Address) ([]thread.Message, error) {
			asked = append(asked, name+" "+tenant+" "+surface.String())
			return []thread.Message{{ID: name + "-1", Text: "from " + name}}, nil
		})
	}
	l := thread.Listers{"slack": lister("slack"), "github": lister("github")}
	gh := address.MustParse("github://acme/handbook/issues/7")
	msgs, err := l.Messages(context.Background(), tenantA, gh)
	if err != nil || len(msgs) != 1 || msgs[0].ID != "github-1" {
		t.Fatalf("github surface = %+v, %v", msgs, err)
	}
	if msgs, err := l.Messages(context.Background(), tenantA, address.MustParse("jira://acme/PROJ-5")); err != nil || msgs != nil {
		t.Fatalf("a surface with no lister = %+v, %v; want nothing and no error", msgs, err)
	}
	if want := []string{"github acme " + gh.String()}; !reflect.DeepEqual(asked, want) {
		t.Errorf("asked %q, want %q", asked, want)
	}
	s := newStore(t)
	if err := thread.Reconcile(context.Background(), s, l, slackThread("docs-bot"), slackSource); err != nil {
		t.Fatal(err)
	}
	if got := texts(mustHistory(t, s, slackThread("docs-bot"))); !reflect.DeepEqual(got, []string{"from slack"}) {
		t.Errorf("reconciled through the dispatcher: %q", got)
	}
}
