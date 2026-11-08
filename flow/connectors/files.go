package connectors

import (
	"log/slog"

	"github.com/urmzd/saige/flow/attachment/intake"
	"github.com/urmzd/saige/flow/blob"
)

// Files is how a trigger brings in the files on the message that raised a
// tag. Every connector that can see files takes one, and its zero value
// leaves files out: the tag then carries no attachments, as before.
//
// The bytes go to Store under the connector's tenant, and the tag carries
// attachment references only. A file that cannot be had (too large, on a host
// the connector does not fetch from, refused by the surface) does not stop the
// tag: it is carried as an unread reference, and the agent is told a file was
// attached and why it could not be read.
type Files struct {
	// Store receives the bytes. Nil disables file intake.
	Store blob.Store
	// Caps bound one message's files. The zero value takes intake's
	// defaults.
	Caps intake.Caps
	// Log receives one line per file that was not brought in: which file,
	// and why. Never a URL, which may be signed, and never a credential.
	// Nil discards.
	Log *slog.Logger
}

// Enabled reports whether files are brought in at all.
func (f Files) Enabled() bool { return f.Store != nil }
