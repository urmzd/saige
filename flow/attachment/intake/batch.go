package intake

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/attachment"
	"github.com/urmzd/saige/flow/blob"
)

// ErrRefused reports a surface that would not hand over a file: it answered
// with an error status, or with a sign-in page where a file was expected. A
// connector wraps it so that the note a model reads says the surface refused,
// not that the file was broken.
var ErrRefused = errors.New("intake: the surface refused access to the file")

// Batch defaults: what one triggering message may bring in.
const (
	// DefaultMaxFiles bounds how many files one message brings in. Further
	// files are still listed, as unread references, so the agent knows they
	// were there.
	DefaultMaxFiles = 8
	// DefaultMaxTotalBytes bounds the bytes one message brings in, across
	// all its files.
	DefaultMaxTotalBytes int64 = 64 << 20
	// DefaultBatchTimeout bounds how long one message's files may take,
	// together.
	DefaultBatchTimeout = 30 * time.Second
)

// Caps bound what one message brings in. A zero field takes its default.
type Caps struct {
	// MaxFiles is how many files are fetched.
	MaxFiles int
	// MaxFileBytes bounds one file. Zero means DefaultMaxBytes.
	MaxFileBytes int64
	// MaxTotalBytes bounds every file of the message together.
	MaxTotalBytes int64
	// Timeout bounds the whole batch.
	Timeout time.Duration
}

func (c Caps) withDefaults() Caps {
	if c.MaxFiles <= 0 {
		c.MaxFiles = DefaultMaxFiles
	}
	if c.MaxFileBytes <= 0 {
		c.MaxFileBytes = DefaultMaxBytes
	}
	if c.MaxTotalBytes <= 0 {
		c.MaxTotalBytes = DefaultMaxTotalBytes
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultBatchTimeout
	}
	return c
}

// Candidate is one file a surface says a message carries, before anything
// has been fetched.
type Candidate struct {
	// Name and Alt are what the surface called the file and any description
	// it gave. They are cleaned.
	Name, Alt string
	// Size is the size the surface declared, or zero when it declared none.
	// A declared size over the cap is refused without a request.
	Size int64
	// Fetch brings the file in under tenant, holding it to meta.MaxBytes.
	// It is where a connector's host policy, credentials and surface quirks
	// live; most implementations end in Fetcher.Fetch. It must not put a URL
	// or a token in the error it returns: the error is logged.
	Fetch func(ctx context.Context, tenant string, meta Meta) (attachment.Attachment, error)
}

// Skip is a file Collect did not bring in, for the caller to log.
type Skip struct {
	// Name is the file's cleaned name.
	Name string
	// Reason is the clause the agent is shown.
	Reason string
	// Err is the underlying failure, nil for a file refused by a cap. It
	// carries no URL or credential if Fetch kept to its contract.
	Err error
}

// Collect brings in a message's files, in the order given, and returns one
// attachment per candidate: the stored file, or attachment.Missing saying
// why it is not there. It never fails: a file that cannot be had is reported
// to the agent rather than allowed to stop the tag, and skip, when set, is
// told about each one so that it can be logged.
//
// The caps are applied in order: files past MaxFiles are not requested; a
// file whose declared size is over the per-file cap, or over what remains of
// the total, is not requested; every request is held to the smaller of the
// two; and the batch shares one deadline.
func Collect(ctx context.Context, tenant string, origin address.Address, caps Caps, files []Candidate, skip func(Skip)) []attachment.Attachment {
	if len(files) == 0 {
		return nil
	}
	caps = caps.withDefaults()
	ctx, cancel := context.WithTimeout(ctx, caps.Timeout)
	defer cancel()

	out := make([]attachment.Attachment, 0, len(files))
	remaining := caps.MaxTotalBytes
	missing := func(f Candidate, reason string, err error) {
		m := attachment.Missing(f.Name, origin, f.Alt, reason)
		if skip != nil {
			skip(Skip{Name: m.Name, Reason: m.Unread, Err: err})
		}
		out = append(out, m)
	}
	for i, f := range files {
		limit := min(caps.MaxFileBytes, remaining)
		switch {
		case len(out) == attachment.MaxPerTag:
			// The wire bound on a tag. Nothing more can even be listed.
			return out
		case i >= caps.MaxFiles:
			missing(f, fmt.Sprintf("the message has more than %d files, and only the first %d are read", caps.MaxFiles, caps.MaxFiles), nil)
			continue
		case f.Fetch == nil:
			missing(f, "the surface did not say where to fetch it from", nil)
			continue
		case f.Size > caps.MaxFileBytes:
			missing(f, fmt.Sprintf("it is %s, over the %s limit for one file", size(f.Size), size(caps.MaxFileBytes)), nil)
			continue
		case limit <= 0 || f.Size > limit:
			missing(f, fmt.Sprintf("the message's files together are over the %s limit", size(caps.MaxTotalBytes)), nil)
			continue
		}
		att, err := f.Fetch(ctx, tenant, Meta{Name: f.Name, Origin: origin, Alt: f.Alt, MaxBytes: limit})
		if err != nil {
			missing(f, reasonFor(err, limit, caps), err)
			continue
		}
		remaining -= att.Size
		out = append(out, att)
	}
	return out
}

// reasonFor turns a fetch failure into the clause a model reads. It names the
// kind of failure and never the URL.
func reasonFor(err error, limit int64, caps Caps) string {
	switch {
	case errors.Is(err, blob.ErrTooLarge) && limit < caps.MaxFileBytes:
		return fmt.Sprintf("the message's files together are over the %s limit", size(caps.MaxTotalBytes))
	case errors.Is(err, blob.ErrTooLarge):
		return fmt.Sprintf("it is over the %s limit for one file", size(caps.MaxFileBytes))
	case errors.Is(err, ErrRefused):
		return "the surface refused access to it"
	case errors.Is(err, ErrNotAllowed), errors.Is(err, ErrTooManyRedirects):
		return "it is hosted somewhere files are not fetched from"
	case errors.Is(err, ErrBlockedAddress):
		return "its host resolves to an address files are not fetched from"
	case errors.Is(err, ErrStatus):
		return "the surface did not return it"
	case errors.Is(err, context.DeadlineExceeded):
		return "fetching it took too long"
	}
	return "it could not be fetched"
}

// size renders a byte count for a reason.
func size(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}
