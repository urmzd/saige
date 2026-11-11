// Package files is the part of file intake every trigger shares: collecting a
// message's files onto its event under the per-event caps, logging what was
// left out, and publishing the event without discarding work the provider
// stopped waiting for.
//
// What a file is on each surface, where it may be fetched from and with which
// credential stays in each connector. This package never sees a URL.
package files

import (
	"context"
	"log/slog"
	"time"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/attachment/intake"
	"github.com/urmzd/saige/flow/connectors"
	"github.com/urmzd/saige/flow/event"
)

// HandoffGrace bounds how long an event that carries files waits to be
// published once its files are in. See Attach.
const HandoffGrace = 10 * time.Second

// Attach brings in candidates as attachments, in order, under tenant, with
// origin (the event's surface) as each file's origin, and sets them on e's
// request. It returns the context to publish e with, and its cancel.
//
// Fetching can outlast a provider's acknowledgement window (Slack waits three
// seconds), and a provider that gives up closes the request. So the files are
// fetched under a context that survives the request, bounded by the caps'
// timeout, and the publish that follows gets a short grace of its own rather
// than the request's context: discarding an event whose files are already in
// would only make the provider's retry fetch them again. A retry that does
// arrive carries the same event id, and the runtime accepts it as the run it
// already has.
//
// With no candidates, file intake disabled, or a body with no request, it
// changes nothing and returns ctx itself.
func Attach(ctx context.Context, cfg connectors.Files, connector, tenant string, origin address.Address, e *event.Envelope, candidates []intake.Candidate) (context.Context, context.CancelFunc) {
	req, ok := event.RequestOf(e.Data)
	if !cfg.Enabled() || len(candidates) == 0 || tenant == "" || !ok {
		return ctx, func() {}
	}
	detached := context.WithoutCancel(ctx)
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	req.Attachments = intake.Collect(detached, tenant, origin, cfg.Caps, candidates, func(s intake.Skip) {
		log.Warn("connector: a file on the message was not brought in; the agent is told it could not be read",
			"connector", connector, "event", e.ID, "file", s.Name, "reason", s.Reason, "error", s.Err)
	})
	e.Data, _ = event.WithRequest(e.Data, req)
	return context.WithTimeout(detached, HandoffGrace)
}
