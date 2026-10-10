package postgres

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/urmzd/saige/agent/notify"
	"github.com/urmzd/saige/agent/types"
)

// MaxNotifyPayload is the PostgreSQL NOTIFY payload limit: a payload must be
// shorter than 8000 bytes. Notifier stores a larger encoded payload in the
// saige_notifications table and sends its ID instead.
const MaxNotifyPayload = 8000

// Payload markers. The first byte of every NOTIFY payload Notifier sends
// says how to read the rest.
const (
	markText   = 't' // the rest is the payload, valid UTF-8 without NUL
	markBinary = 'b' // the rest is the payload in standard base64
	markStored = 'r' // the rest is a saige_notifications ID
)

// NotifierOptions configures a Notifier. The zero value is valid.
type NotifierOptions struct {
	// MinBackoff and MaxBackoff bound the delay between reconnect attempts
	// of the listening connection. Defaults: 100ms and 10s.
	MinBackoff time.Duration
	MaxBackoff time.Duration
	// PayloadTTL is how long a payload too large for NOTIFY stays in
	// saige_notifications for subscribers to load. Default: 5 minutes.
	PayloadTTL time.Duration
	// Buffer is the per-subscriber channel capacity. Zero uses
	// notify.DefaultBuffer.
	Buffer int
	// ApplicationName is the listening connection's application_name.
	// Default: "saige-notifier".
	ApplicationName string
	// OnReconnect runs after the listening connection is replaced and every
	// channel is listened to again. Notifications published while it was
	// down are lost, so use it to re-check durable state.
	OnReconnect func()
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Notifier is a types.Notifier on PostgreSQL LISTEN and NOTIFY.
//
// It holds one dedicated connection that listens to every channel with a
// local subscriber, and publishes through the pool with pg_notify. When the
// listening connection drops, it reconnects with exponential backoff and
// listens to every channel again; subscriber channels stay open across the
// gap. Notifications sent during the gap are not delivered.
//
// A payload whose encoded form is MaxNotifyPayload bytes or more is written
// to saige_notifications in the same transaction as its NOTIFY; subscribers
// load it by ID. Rows expire after PayloadTTL and each large publish deletes
// expired rows. SweepNotifications deletes them on demand. RunMigrations
// creates the table.
type Notifier struct {
	pool *pgxpool.Pool
	opts NotifierOptions
	hub  notify.Hub

	ctx  context.Context
	stop context.CancelFunc
	done chan struct{}

	mu      sync.Mutex
	desired map[string]int
	waiters []chan struct{}
	dirty   bool
	wake    context.CancelFunc
	closed  bool
}

var _ types.Notifier = (*Notifier)(nil)

// NewNotifier starts a notifier on pool. The listening connection uses the
// pool's connection settings but is not part of the pool. Close the notifier
// before the pool.
func NewNotifier(pool *pgxpool.Pool, opts NotifierOptions) *Notifier {
	if opts.MinBackoff <= 0 {
		opts.MinBackoff = 100 * time.Millisecond
	}
	if opts.MaxBackoff < opts.MinBackoff {
		opts.MaxBackoff = max(10*time.Second, opts.MinBackoff)
	}
	if opts.PayloadTTL <= 0 {
		opts.PayloadTTL = 5 * time.Minute
	}
	if opts.ApplicationName == "" {
		opts.ApplicationName = "saige-notifier"
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	ctx, stop := context.WithCancel(context.Background())
	n := &Notifier{
		pool:    pool,
		opts:    opts,
		hub:     notify.Hub{Buffer: opts.Buffer},
		ctx:     ctx,
		stop:    stop,
		done:    make(chan struct{}),
		desired: map[string]int{},
	}
	go n.run()
	return n
}

// Publish implements types.Notifier. Delivery happens when the publishing
// statement commits, which for Publish is immediately.
func (n *Notifier) Publish(ctx context.Context, channel string, payload []byte) error {
	if err := types.ValidateChannel(channel); err != nil {
		return err
	}
	if n.isClosed() {
		return types.ErrNotifierClosed
	}
	enc := encodePayload(payload)
	if len(enc) < MaxNotifyPayload {
		if _, err := n.pool.Exec(ctx, `SELECT pg_notify($1, $2)`, channel, enc); err != nil {
			return fmt.Errorf("notifier: publish: %w", err)
		}
		return nil
	}
	return pgx.BeginFunc(ctx, n.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM saige_notifications WHERE expires_at < now()`); err != nil {
			return fmt.Errorf("notifier: sweep: %w", err)
		}
		var id int64
		err := tx.QueryRow(ctx,
			`INSERT INTO saige_notifications (channel, payload, expires_at)
			 VALUES ($1, $2, now() + $3::bigint * interval '1 microsecond')
			 RETURNING id`,
			channel, payload, n.opts.PayloadTTL.Microseconds(),
		).Scan(&id)
		if err != nil {
			return fmt.Errorf("notifier: store payload: %w", err)
		}
		if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, channel, string(markStored)+strconv.FormatInt(id, 10)); err != nil {
			return fmt.Errorf("notifier: publish: %w", err)
		}
		return nil
	})
}

// SweepNotifications deletes stored payloads past their TTL and returns how
// many it removed.
func (n *Notifier) SweepNotifications(ctx context.Context) (int64, error) {
	tag, err := n.pool.Exec(ctx, `DELETE FROM saige_notifications WHERE expires_at < now()`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// Subscribe implements types.Notifier. It returns once the listening
// connection is listening to channel, so a message published after Subscribe
// returns is delivered. While the connection is down it waits for the
// reconnect or for ctx.
func (n *Notifier) Subscribe(ctx context.Context, channel string) (<-chan types.Notification, func(), error) {
	if err := types.ValidateChannel(channel); err != nil {
		return nil, nil, err
	}
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return nil, nil, types.ErrNotifierClosed
	}
	n.desired[channel]++
	n.mu.Unlock()

	ch, cancel, err := n.hub.Subscribe(ctx, channel, func() { n.release(channel) })
	if err != nil {
		n.release(channel)
		return nil, nil, err
	}

	ready := make(chan struct{})
	n.mu.Lock()
	n.waiters = append(n.waiters, ready)
	n.dirty = true
	wake := n.wake
	n.mu.Unlock()
	if wake != nil {
		wake()
	}
	select {
	case <-ready:
		return ch, cancel, nil
	case <-ctx.Done():
		cancel()
		return nil, nil, ctx.Err()
	case <-n.done:
		cancel()
		return nil, nil, types.ErrNotifierClosed
	}
}

// release drops one subscriber's interest in channel.
func (n *Notifier) release(channel string) {
	n.mu.Lock()
	if n.desired[channel]--; n.desired[channel] <= 0 {
		delete(n.desired, channel)
	}
	n.dirty = true
	wake := n.wake
	n.mu.Unlock()
	if wake != nil {
		wake()
	}
}

// Close stops listening, closes the listening connection and ends every
// subscription. It does not close the pool. It is idempotent.
func (n *Notifier) Close(ctx context.Context) error {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return waitDone(ctx, n.done)
	}
	n.closed = true
	n.mu.Unlock()
	n.stop()
	if err := waitDone(ctx, n.done); err != nil {
		return err
	}
	return n.hub.Close(ctx)
}

// waitDone waits for done to close, or for ctx to end.
func waitDone(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (n *Notifier) isClosed() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.closed
}

// run owns the listening connection for the notifier's lifetime.
func (n *Notifier) run() {
	defer close(n.done)
	backoff := n.opts.MinBackoff
	connected := false
	for {
		conn, err := n.connect()
		if err == nil {
			err = n.serve(conn, connected, func() { backoff = n.opts.MinBackoff })
			connected = true
			closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			_ = conn.Close(closeCtx)
			cancel()
		}
		if n.ctx.Err() != nil {
			return
		}
		n.opts.Logger.Warn("notifier: listening connection lost; reconnecting", "err", err, "backoff", backoff)
		t := time.NewTimer(backoff)
		select {
		case <-n.ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		backoff = min(backoff*2, n.opts.MaxBackoff)
	}
}

func (n *Notifier) connect() (*pgx.Conn, error) {
	cfg := n.pool.Config().ConnConfig.Copy()
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	cfg.RuntimeParams["application_name"] = n.opts.ApplicationName
	return pgx.ConnectConfig(n.ctx, cfg)
}

// serve listens on conn until it fails or the notifier closes. reconnected
// reports that an earlier connection existed; healthy runs once every
// channel is listened to.
func (n *Notifier) serve(conn *pgx.Conn, reconnected bool, healthy func()) error {
	listened := map[string]bool{}
	announced := false
	for {
		n.mu.Lock()
		want := make(map[string]bool, len(n.desired))
		for c := range n.desired {
			want[c] = true
		}
		waiters := n.waiters
		n.waiters = nil
		n.dirty = false
		n.mu.Unlock()

		if err := syncListens(n.ctx, conn, listened, want); err != nil {
			n.mu.Lock()
			n.waiters = append(waiters, n.waiters...)
			n.mu.Unlock()
			return err
		}
		for _, w := range waiters {
			close(w)
		}
		if !announced {
			announced = true
			healthy()
			if reconnected && n.opts.OnReconnect != nil {
				n.opts.OnReconnect()
			}
		}

		waitCtx, cancel := context.WithCancel(n.ctx)
		n.mu.Lock()
		n.wake = cancel
		if n.dirty {
			cancel()
		}
		n.mu.Unlock()
		msg, err := conn.WaitForNotification(waitCtx)
		n.mu.Lock()
		n.wake = nil
		n.mu.Unlock()
		woken := waitCtx.Err() != nil
		cancel()

		if msg != nil {
			n.dispatch(msg)
		}
		if err != nil {
			if n.ctx.Err() != nil {
				return n.ctx.Err()
			}
			if woken && !conn.IsClosed() {
				continue
			}
			return err
		}
	}
}

// syncListens issues LISTEN and UNLISTEN so conn listens to exactly want.
func syncListens(ctx context.Context, conn *pgx.Conn, listened, want map[string]bool) error {
	for c := range want {
		if listened[c] {
			continue
		}
		if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{c}.Sanitize()); err != nil {
			return fmt.Errorf("listen %s: %w", c, err)
		}
		listened[c] = true
	}
	for c := range listened {
		if want[c] {
			continue
		}
		if _, err := conn.Exec(ctx, "UNLISTEN "+pgx.Identifier{c}.Sanitize()); err != nil {
			return fmt.Errorf("unlisten %s: %w", c, err)
		}
		delete(listened, c)
	}
	return nil
}

// dispatch decodes one NOTIFY and delivers it to local subscribers.
func (n *Notifier) dispatch(msg *pgconn.Notification) {
	if !n.hub.Subscribed(msg.Channel) {
		return
	}
	payload, err := n.decodePayload(msg.Payload)
	if err != nil {
		n.opts.Logger.Warn("notifier: dropping undecodable notification", "channel", msg.Channel, "err", err)
		return
	}
	_ = n.hub.Deliver(n.ctx, msg.Channel, payload)
}

func encodePayload(payload []byte) string {
	if utf8.Valid(payload) && !containsNUL(payload) {
		return string(markText) + string(payload)
	}
	return string(markBinary) + base64.StdEncoding.EncodeToString(payload)
}

func containsNUL(b []byte) bool {
	for _, c := range b {
		if c == 0 {
			return true
		}
	}
	return false
}

var errBadPayload = errors.New("notifier: unknown payload encoding")

func (n *Notifier) decodePayload(s string) ([]byte, error) {
	if s == "" {
		return nil, errBadPayload
	}
	switch s[0] {
	case markText:
		return []byte(s[1:]), nil
	case markBinary:
		return base64.StdEncoding.DecodeString(s[1:])
	case markStored:
		id, err := strconv.ParseInt(s[1:], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errBadPayload, err)
		}
		ctx, cancel := context.WithTimeout(n.ctx, 30*time.Second)
		defer cancel()
		var payload []byte
		err = n.pool.QueryRow(ctx, `SELECT payload FROM saige_notifications WHERE id = $1`, id).Scan(&payload)
		if err != nil {
			return nil, fmt.Errorf("notifier: load stored payload %d: %w", id, err)
		}
		return payload, nil
	default:
		return nil, errBadPayload
	}
}
