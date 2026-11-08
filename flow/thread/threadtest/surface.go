// Package threadtest holds two things for tests of the thread journal: Run,
// the conformance suite every thread.Store must pass, and Surface, a
// thread.Lister that holds messages in memory and lets a test do to them what
// people do on a real surface, which is post, edit and delete.
//
// Surface exists so that the reconciliation rules (append only, in surface
// order, never rewriting what the journal already holds, files brought in
// once) are provable without a real surface.
package threadtest

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/attachment"
	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/thread"
)

// Surface is an in-memory surface. The zero value is not usable; call
// NewSurface.
type Surface struct {
	mu      sync.Mutex
	threads map[string][]thread.Message // tenant + canonical surface -> messages
	next    int
	lists   int
	fetches int
	err     error
}

// NewSurface returns an empty surface.
func NewSurface() *Surface { return &Surface{threads: make(map[string][]thread.Message)} }

var _ thread.Lister = (*Surface)(nil)

func slot(tenant string, surface address.Address) string {
	return tenant + "\x00" + thread.Surface(surface)
}

// Post adds a message from a person to a thread and returns its id.
func (s *Surface) Post(tenant string, surface address.Address, who, text string) string {
	return s.post(tenant, surface, thread.Message{Text: text, Actor: event.Actor{ID: who, Display: who}})
}

// PostFiles adds a message from a person with files on it and returns its
// id. The files are handed to a reconciliation through Message.Files, and
// every call of it is counted (see Fetches), which is how a test proves a
// file is brought in once rather than on every reconciliation.
func (s *Surface) PostFiles(tenant string, surface address.Address, who, text string, files ...attachment.Attachment) string {
	held := append([]attachment.Attachment(nil), files...)
	return s.post(tenant, surface, thread.Message{
		Text:  text,
		Actor: event.Actor{ID: who, Display: who},
		Files: func(ctx context.Context) ([]attachment.Attachment, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			s.mu.Lock()
			s.fetches++
			s.mu.Unlock()
			return append([]attachment.Attachment(nil), held...), nil
		},
	})
}

// Fetches reports how many times a message's files were brought in.
func (s *Surface) Fetches() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fetches
}

// PostOwn adds a message the deployment itself posted, such as an agent's
// rendered answer.
func (s *Surface) PostOwn(tenant string, surface address.Address, text string) string {
	return s.post(tenant, surface, thread.Message{Text: text, Actor: event.Actor{ID: "fiber", Bot: true}, Own: true})
}

func (s *Surface) post(tenant string, surface address.Address, m thread.Message) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	m.ID = fmt.Sprintf("m%04d", s.next)
	m.At = time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC).Add(time.Duration(s.next) * time.Minute)
	k := slot(tenant, surface)
	s.threads[k] = append(s.threads[k], m)
	return m.ID
}

// Edit replaces the text of a message, keeping its id and position.
func (s *Surface) Edit(tenant string, surface address.Address, id, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, m := range s.threads[slot(tenant, surface)] {
		if m.ID == id {
			s.threads[slot(tenant, surface)][i].Text = text
		}
	}
}

// Delete removes a message.
func (s *Surface) Delete(tenant string, surface address.Address, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := slot(tenant, surface)
	kept := s.threads[k][:0:0]
	for _, m := range s.threads[k] {
		if m.ID != id {
			kept = append(kept, m)
		}
	}
	s.threads[k] = kept
}

// Fail makes every later Messages call return err. Nil heals it.
func (s *Surface) Fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

// Lists reports how many times the surface was listed.
func (s *Surface) Lists() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lists
}

// Messages implements thread.Lister.
func (s *Surface) Messages(ctx context.Context, tenant string, surface address.Address) ([]thread.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lists++
	if s.err != nil {
		return nil, s.err
	}
	return append([]thread.Message(nil), s.threads[slot(tenant, surface)]...), nil
}
