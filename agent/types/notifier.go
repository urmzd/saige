package types

import (
	"context"
	"errors"
	"fmt"
)

// Notifier is a publish/subscribe channel between processes or goroutines.
// It carries wake-up signals and small messages; it is not a durable queue.
// A subscriber that is not listening when a message is published does not
// receive it, so callers check durable state after subscribing and treat a
// notification as a hint to look again.
//
// Implementations must be safe for concurrent use. agent/notify provides an
// in-memory implementation and postgres.Notifier a cross-process one.
type Notifier interface {
	// Publish sends payload to every current subscriber of channel. The
	// channel name must pass ValidateChannel.
	Publish(ctx context.Context, channel string, payload []byte) error

	// Subscribe starts delivery of channel's notifications. The returned
	// cancel function stops delivery and closes the channel; it is safe to
	// call more than once. Cancelling ctx has the same effect. The
	// notification channel is closed only after cancel, ctx cancellation, or
	// the notifier closing, never because of a transient connection loss.
	Subscribe(ctx context.Context, channel string) (<-chan Notification, func(), error)
}

// Notification is one message delivered to a subscriber.
type Notification struct {
	Channel string
	Payload []byte
}

// MaxChannelLen is the longest valid channel name. It matches the PostgreSQL
// identifier limit, so a name valid here is valid for LISTEN.
const MaxChannelLen = 63

// ErrNotifierClosed reports a Publish or Subscribe on a closed notifier.
var ErrNotifierClosed = errors.New("notifier: closed")

// ErrInvalidChannel reports a channel name ValidateChannel rejects.
var ErrInvalidChannel = errors.New("notifier: invalid channel name")

// ValidateChannel checks a channel name: 1 to MaxChannelLen bytes of ASCII
// letters, digits, '_', '.', ':' or '-'. The restriction keeps names portable
// across backends and safe to use as identifiers.
func ValidateChannel(channel string) error {
	if channel == "" || len(channel) > MaxChannelLen {
		return fmt.Errorf("%w: %q must be 1 to %d bytes", ErrInvalidChannel, channel, MaxChannelLen)
	}
	for i := 0; i < len(channel); i++ {
		c := channel[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_', c == '.', c == ':', c == '-':
		default:
			return fmt.Errorf("%w: %q contains %q", ErrInvalidChannel, channel, c)
		}
	}
	return nil
}
