package threadtest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/attachment"
	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/thread"
)

const (
	tenantA = "acme"
	tenantB = "globex"
)

func slackThread(agent string) thread.Key {
	return thread.Key{Tenant: tenantA, Agent: agent, Surface: "slack://T01/C02?thread=1699.001"}
}

func user(run, text string) thread.Entry {
	return thread.Entry{Role: thread.RoleUser, Text: text, RunID: run, Actor: event.Actor{ID: "U1", Display: "ada"}}
}

func assistant(run, text string) thread.Entry {
	return thread.Entry{Role: thread.RoleAssistant, Text: text, RunID: run, Actor: event.Actor{ID: "docs-bot", Bot: true}}
}

func posted(id, text string) thread.Entry {
	return thread.Entry{Role: thread.RoleUser, Text: text, SurfaceID: id, Actor: event.Actor{ID: "U2", Display: "grace"}}
}

func mustAppend(t *testing.T, s thread.Store, key thread.Key, entries ...thread.Entry) []thread.Entry {
	t.Helper()
	out, err := s.Append(context.Background(), key, entries...)
	if err != nil {
		t.Fatalf("Append(%s): %v", key, err)
	}
	if len(out) != len(entries) {
		t.Fatalf("Append returned %d entries for %d inputs", len(out), len(entries))
	}
	return out
}

func mustHistory(t *testing.T, s thread.Store, key thread.Key) []thread.Entry {
	t.Helper()
	ctx := context.Background()
	head, err := s.Head(ctx, key)
	if err != nil {
		t.Fatalf("Head(%s): %v", key, err)
	}
	out, err := s.History(ctx, key, head)
	if err != nil {
		t.Fatalf("History(%s, %d): %v", key, head, err)
	}
	return out
}

func texts(entries []thread.Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Text
	}
	return out
}

// Factory returns a fresh, empty journal built with opts. It must isolate
// each call. Run passes a clock option so stamps are deterministic.
type Factory func(t *testing.T, opts ...thread.Option) thread.Store

// Run is the conformance suite every thread.Store must pass: contiguous
// sequences, idempotent appends by run and role and by surface message,
// history pinned forever, gaps reported as gaps, concurrent appends that
// neither interleave nor drop, isolation, refusal of what it could not keep
// consistent, and attachments kept as references. Memory and Postgres both
// run it.
func Run(t *testing.T, factory Factory) {
	newStore := func(t *testing.T) thread.Store {
		t.Helper()
		var mu sync.Mutex
		at := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
		return factory(t, thread.WithClock(func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			at = at.Add(time.Second)
			return at
		}))
	}

	// Sequence numbers are the pin a run holds, so they start at 1, rise by one,
	// and the entry read back is the entry that was written, field for field.
	t.Run("AppendAssignsSequencesFromOneWithoutHoles", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		key := slackThread("docs-bot")

		said := time.Date(2026, 10, 13, 8, 30, 0, 123456000, time.UTC)
		q := user("run-1", "how do I deploy?")
		q.SurfaceID, q.At = "1699.002", said
		first := mustAppend(t, s, key, q, assistant("run-1", "run make deploy"))
		second := mustAppend(t, s, key, user("run-2", "and roll back?"), assistant("run-2", "run make rollback"))

		for i, e := range append(first, second...) {
			if e.Seq != uint64(i+1) {
				t.Fatalf("entry %d has seq %d, want %d", i, e.Seq, i+1)
			}
		}
		got := mustHistory(t, s, key)
		if !reflect.DeepEqual(got, append(first, second...)) {
			t.Fatalf("history differs from what Append returned:\n got %+v\nwant %+v", got, append(first, second...))
		}
		if e := got[0]; e.Role != thread.RoleUser || e.Text != "how do I deploy?" || e.RunID != "run-1" ||
			e.SurfaceID != "1699.002" || !e.At.Equal(said) || e.Actor != (event.Actor{ID: "U1", Display: "ada"}) {
			t.Errorf("entry 1 did not round-trip: %+v", e)
		}
		if e := got[1]; e.Role != thread.RoleAssistant || !e.Actor.Bot || e.At.IsZero() {
			t.Errorf("entry 2 did not round-trip, or was not stamped: %+v", e)
		}
		if head, err := s.Head(context.Background(), key); err != nil || head != 4 {
			t.Errorf("Head = %d, %v; want 4", head, err)
		}
	})

	// Durable runs retry. A run that appended its exchange and crashed appends it
	// again, and the journal must come out exactly as if it had not.
	t.Run("AppendingTheSameRunAndRoleTwiceIsANoOp", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		key := slackThread("docs-bot")

		first := mustAppend(t, s, key, user("run-1", "question"), assistant("run-1", "answer"))
		mustAppend(t, s, key, user("run-2", "next"), assistant("run-2", "next answer"))

		// The retry carries different text and a different time, as a re-executed
		// turn would. Neither may replace what is stored.
		retry := mustAppend(t, s, key, user("run-1", "question, reworded"), assistant("run-1", "a different answer"))
		if !reflect.DeepEqual(retry, first) {
			t.Fatalf("the retried append returned %+v, want the stored entries %+v", retry, first)
		}
		if got := texts(mustHistory(t, s, key)); !reflect.DeepEqual(got, []string{"question", "answer", "next", "next answer"}) {
			t.Fatalf("history after a retried append = %q", got)
		}

		// A retry that only half landed the first time completes, and the half
		// that landed is left alone.
		half := slackThread("half")
		mustAppend(t, s, half, user("run-9", "question"))
		both := mustAppend(t, s, half, user("run-9", "question"), assistant("run-9", "answer"))
		if both[0].Seq != 1 || both[1].Seq != 2 {
			t.Fatalf("completing a half-landed append gave seqs %d, %d; want 1, 2", both[0].Seq, both[1].Seq)
		}
		// The same run and role named twice in one call is still one entry.
		twice := mustAppend(t, s, slackThread("twice"), user("run-3", "once"), user("run-3", "once more"))
		if twice[0].Seq != 1 || twice[1].Seq != 1 || twice[1].Text != "once" {
			t.Fatalf("a duplicate inside one call was written: %+v", twice)
		}
	})

	// A surface message is one entry however it arrives: seen by reconciliation
	// and then appended by the run it raised, or the other way round.
	t.Run("AppendingTheSameSurfaceMessageTwiceIsANoOp", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		key := slackThread("docs-bot")

		seen := mustAppend(t, s, key, posted("1699.005", "as first seen"))
		again := mustAppend(t, s, key, posted("1699.005", "as edited later"))
		if !reflect.DeepEqual(again, seen) {
			t.Fatalf("a re-seen surface message returned %+v, want %+v", again, seen)
		}

		// The run that message raised now appends it as its own user message.
		q := user("run-1", "as the run rendered it")
		q.SurfaceID = "1699.005"
		out := mustAppend(t, s, key, q, assistant("run-1", "answer"))
		if out[0].Seq != 1 || out[0].Text != "as first seen" {
			t.Fatalf("the run's copy of a journaled surface message was written or replaced it: %+v", out[0])
		}
		if out[1].Seq != 2 {
			t.Fatalf("the answer landed at seq %d, want 2", out[1].Seq)
		}
		if got := texts(mustHistory(t, s, key)); !reflect.DeepEqual(got, []string{"as first seen", "answer"}) {
			t.Fatalf("history = %q", got)
		}
	})

	// The pin: the answer for "up to N" never changes, whatever is appended after.
	t.Run("HistoryUpToASequenceIsTheSameForever", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		s := newStore(t)
		key := slackThread("docs-bot")

		mustAppend(t, s, key, user("run-1", "one"), assistant("run-1", "two"))
		pinned, err := s.History(ctx, key, 2)
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		for i := 2; i < 12; i++ {
			run := fmt.Sprintf("run-%d", i)
			mustAppend(t, s, key, user(run, "later question"), assistant(run, "later answer"))
			got, err := s.History(ctx, key, 2)
			if err != nil {
				t.Fatalf("History after %d appends: %v", i, err)
			}
			if !reflect.DeepEqual(got, pinned) {
				t.Fatalf("history up to 2 changed after a later append:\n got %+v\nwant %+v", got, pinned)
			}
		}
		// Mutating what was returned must not reach the journal.
		pinned[0].Text = "tampered"
		if got, _ := s.History(ctx, key, 1); got[0].Text != "one" {
			t.Fatalf("a caller edited the journal through a returned slice: %q", got[0].Text)
		}
		if got, err := s.History(ctx, key, 0); err != nil || len(got) != 0 {
			t.Errorf("History(0) = %v, %v; want no entries", got, err)
		}
		if head, err := s.Head(ctx, slackThread("nobody")); err != nil || head != 0 {
			t.Errorf("Head of an unknown thread = %d, %v; want 0", head, err)
		}
	})

	// A run pinned to N entries must be shown N entries or told the truth. A
	// shorter history handed over silently would be an agent that forgot.
	t.Run("HistoryPastTheHeadIsAGapNotAShorterHistory", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		s := newStore(t)
		key := slackThread("docs-bot")
		mustAppend(t, s, key, user("run-1", "one"), assistant("run-1", "two"))

		if _, err := s.History(ctx, key, 3); !errors.Is(err, thread.ErrGap) {
			t.Fatalf("History(3) of a 2-entry thread = %v, want ErrGap", err)
		}
		if _, err := s.History(ctx, slackThread("nobody"), 1); !errors.Is(err, thread.ErrGap) {
			t.Fatalf("History(1) of an unknown thread = %v, want ErrGap", err)
		}
	})

	// Concurrency: appenders to one thread serialize. No sequence is used twice,
	// none is skipped, no entry is lost, and the entries of one call stay
	// adjacent, so a question is always directly followed by its own answer.
	t.Run("ConcurrentAppendsNeitherInterleaveNorLoseAnEntry", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		key := slackThread("docs-bot")
		other := slackThread("other-bot")

		const runs = 24
		var wg sync.WaitGroup
		errs := make(chan error, runs*2)
		for i := range runs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				run := fmt.Sprintf("run-%02d", i)
				// Each run appends twice, as a retried run does, racing itself
				// as well as every other run.
				for range 2 {
					if _, err := s.Append(context.Background(), key, user(run, "q "+run), assistant(run, "a "+run)); err != nil {
						errs <- err
					}
				}
				if _, err := s.Append(context.Background(), other, user(run, "q "+run)); err != nil {
					errs <- err
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("Append: %v", err)
		}

		got := mustHistory(t, s, key)
		if len(got) != runs*2 {
			t.Fatalf("thread holds %d entries, want %d: an entry was lost or written twice", len(got), runs*2)
		}
		seen := map[string]bool{}
		for i, e := range got {
			if e.Seq != uint64(i+1) {
				t.Fatalf("entry %d has seq %d: sequences have a hole or a repeat", i, e.Seq)
			}
			if i%2 == 0 {
				if e.Role != thread.RoleUser || seen[e.RunID] {
					t.Fatalf("entry %d is %s of %s, want the first entry of an unseen run", i, e.Role, e.RunID)
				}
				seen[e.RunID] = true
				continue
			}
			if e.Role != thread.RoleAssistant || e.RunID != got[i-1].RunID {
				t.Fatalf("entry %d is %s of %s but follows %s: two runs interleaved", i, e.Role, e.RunID, got[i-1].RunID)
			}
		}
		if n := len(mustHistory(t, s, other)); n != runs {
			t.Fatalf("the neighbouring thread holds %d entries, want %d", n, runs)
		}
	})

	// Isolation is the key, all of it: another tenant, another agent, or another
	// surface is another journal with its own sequence, and the same run id or
	// surface id in two of them is two entries.
	t.Run("ThreadsShareNothingAcrossTenantAgentOrSurface", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		base := slackThread("docs-bot")
		keys := map[string]thread.Key{
			"base":            base,
			"another tenant":  {Tenant: tenantB, Agent: base.Agent, Surface: base.Surface},
			"another agent":   {Tenant: base.Tenant, Agent: "review-bot", Surface: base.Surface},
			"another surface": {Tenant: base.Tenant, Agent: base.Agent, Surface: "slack://T01/C02?thread=1700.009"},
		}
		for name, key := range keys {
			q := user("run-1", "said in "+name)
			q.SurfaceID = "1699.002"
			out := mustAppend(t, s, key, q)
			if out[0].Seq != 1 {
				t.Fatalf("%s: first entry has seq %d, want 1: it shares a sequence with another thread", name, out[0].Seq)
			}
		}
		for name, key := range keys {
			if got := texts(mustHistory(t, s, key)); !reflect.DeepEqual(got, []string{"said in " + name}) {
				t.Errorf("%s: history = %q, want only its own message", name, got)
			}
		}
	})

	// Default deny on what the journal cannot stand behind.
	t.Run("TheJournalRefusesWhatItCouldNotKeepConsistent", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		s := newStore(t)
		key := slackThread("docs-bot")

		cases := map[string]struct {
			key   thread.Key
			entry thread.Entry
		}{
			"no tenant":        {thread.Key{Agent: "docs-bot", Surface: key.Surface}, user("run-1", "hi")},
			"no agent":         {thread.Key{Tenant: tenantA, Surface: key.Surface}, user("run-1", "hi")},
			"no surface":       {thread.Key{Tenant: tenantA, Agent: "docs-bot"}, user("run-1", "hi")},
			"no role":          {key, thread.Entry{Text: "hi", RunID: "run-1"}},
			"unknown":          {key, thread.Entry{Role: "system", Text: "hi", RunID: "run-1"}},
			"no text":          {key, user("run-1", "  ")},
			"no text or files": {key, thread.Entry{Role: thread.RoleUser, Text: " ", SurfaceID: "1699.9", Attachments: []attachment.Attachment{}}},
			"no identity":      {key, thread.Entry{Role: thread.RoleUser, Text: "hi"}},
		}
		for name, tc := range cases {
			// A valid entry rides along, to prove a refused append writes nothing.
			if _, err := s.Append(ctx, tc.key, user("run-0", "valid"), tc.entry); !errors.Is(err, thread.ErrInvalid) {
				t.Errorf("%s: Append = %v, want ErrInvalid", name, err)
			}
		}
		if head, err := s.Head(ctx, key); err != nil || head != 0 {
			t.Fatalf("a refused append wrote something: head = %d, %v", head, err)
		}
		if _, err := s.Head(ctx, thread.Key{Agent: "docs-bot", Surface: key.Surface}); !errors.Is(err, thread.ErrInvalid) {
			t.Errorf("Head with no tenant = %v, want ErrInvalid", err)
		}
		if _, err := s.History(ctx, thread.Key{Agent: "docs-bot", Surface: key.Surface}, 0); !errors.Is(err, thread.ErrInvalid) {
			t.Errorf("History with no tenant = %v, want ErrInvalid", err)
		}
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := s.Append(cancelled, key, user("run-1", "hi")); !errors.Is(err, context.Canceled) {
			t.Errorf("Append on a cancelled context = %v, want context.Canceled", err)
		}
		if out, err := s.Append(ctx, key); err != nil || len(out) != 0 {
			t.Errorf("Append of nothing = %v, %v; want nothing and no error", out, err)
		}
	})

	// A file posted with a message is part of what was said, so the journal keeps
	// the reference with the entry: read back unchanged, in order, and never
	// reachable for editing through a returned slice. A message that was only a
	// file is still a message.
	t.Run("AnEntryKeepsItsAttachmentsAsReferencesInOrder", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		s := newStore(t)
		key := slackThread("docs-bot")
		origin := address.MustParse("slack://T01/C02?thread=1699.001")
		shot := attachment.Attachment{Digest: strings.Repeat("a", 64), MediaType: "image/png", Name: "shot.png", Size: 2048, Origin: origin, Alt: "the error"}
		log := attachment.Attachment{Digest: strings.Repeat("b", 64), MediaType: "text/plain", Name: "build.log", Size: 77}
		gone := attachment.Missing("huge.mov", origin, "", "it is larger than the 25.0 MB limit for one file")

		q := user("run-1", "why does this fail?")
		q.Attachments = []attachment.Attachment{shot, log, gone}
		fileOnly := posted("1699.003", "")
		fileOnly.Attachments = []attachment.Attachment{log}
		plain := posted("1699.004", "no files here")
		plain.Attachments = []attachment.Attachment{}

		wrote := mustAppend(t, s, key, q, assistant("run-1", "the log says why"), fileOnly, plain)
		got := mustHistory(t, s, key)
		if !reflect.DeepEqual(got, wrote) {
			t.Fatalf("entries read back differ from what Append returned:\n got %+v\nwant %+v", got, wrote)
		}
		if !reflect.DeepEqual(got[0].Attachments, q.Attachments) {
			t.Errorf("attachments = %+v, want %+v in order", got[0].Attachments, q.Attachments)
		}
		if got[1].Attachments != nil || got[3].Attachments != nil {
			t.Errorf("an entry with no attachments read back as %#v and %#v, want nil", got[1].Attachments, got[3].Attachments)
		}
		if len(got[2].Attachments) != 1 || got[2].Text != "" {
			t.Errorf("a file-only message read back as %+v", got[2])
		}

		got[0].Attachments[0].Name = "tampered.png"
		wrote[0].Attachments[1].Name = "tampered.log"
		q.Attachments[2].Name = "tampered.mov"
		again, err := s.History(ctx, key, 1)
		if err != nil {
			t.Fatal(err)
		}
		if names := []string{again[0].Attachments[0].Name, again[0].Attachments[1].Name, again[0].Attachments[2].Name}; !reflect.DeepEqual(names, []string{"shot.png", "build.log", "huge.mov"}) {
			t.Errorf("a caller edited stored attachments through a slice it held: %v", names)
		}

		bad := posted("1699.005", "forged")
		bad.Attachments = []attachment.Attachment{{Digest: "../etc/passwd", MediaType: "text/plain"}}
		if _, err := s.Append(ctx, key, bad); !errors.Is(err, thread.ErrInvalid) {
			t.Errorf("Append with a malformed attachment = %v, want ErrInvalid", err)
		}
	})
}
