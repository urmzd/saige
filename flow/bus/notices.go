package bus

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// noticeChannel is the LISTEN/NOTIFY channel notices travel on. It is not
// the events' channel, so a notice never wakes an event reader.
const noticeChannel = "fiber_notices"

// MaxNotice is the most bytes a notice's payload may hold. Postgres refuses
// a NOTIFY payload of 8,000 bytes or more; a notice says that something
// changed, not what it now is, so it needs far less.
const MaxNotice = 2048

// Notices carries small, lossy notices between the processes on one
// database through Postgres NOTIFY, without a table: a store tells every
// process that a tenant's records changed, and each process tells its own
// watchers. It is not the bus. Nothing is kept, a notice sent while no
// process listens is gone, and a listener whose connection drops misses what
// was sent until it reconnects. A watcher fetches again on the next notice.
//
// A notice is scoped to the schema it was sent from, so deployments that
// share a database in different schemas do not hear each other.
//
// It holds one LISTEN connection, taken from the pool while anything
// listens. It does not own the pool.
type Notices struct {
	pool  *pgxpool.Pool
	retry time.Duration

	listening atomic.Bool

	mu        sync.Mutex
	schema    string
	listeners map[string]map[int]func(tenant string, payload []byte)
	next      int
	count     int
	cancel    context.CancelFunc
}

// NewNotices returns notices on pool. It does not touch the database until
// something sends or listens.
func NewNotices(pool *pgxpool.Pool) *Notices {
	return &Notices{pool: pool, retry: time.Second}
}

// notice is a NOTIFY payload: the schema it was sent from, its topic, its
// tenant and what the sender said.
type notice struct {
	Schema  string          `json:"s"`
	Topic   string          `json:"k"`
	Tenant  string          `json:"t"`
	Payload json.RawMessage `json:"p"`
}

// Send tells every process listening on topic, this one included, that
// tenant has payload, which must be JSON of at most MaxNotice bytes.
func (n *Notices) Send(ctx context.Context, topic, tenant string, payload []byte) error {
	if topic == "" || tenant == "" {
		return fmt.Errorf("%w: a notice needs a topic and a tenant", ErrInvalid)
	}
	if len(payload) > MaxNotice {
		return fmt.Errorf("%w: a notice of %d bytes is over %d", ErrInvalid, len(payload), MaxNotice)
	}
	if !json.Valid(payload) {
		return fmt.Errorf("%w: a notice's payload is not JSON", ErrInvalid)
	}
	_, err := n.pool.Exec(ctx, `SELECT pg_notify($1, json_build_object('s', current_schema(), 'k', $2::text, 't', $3::text, 'p', $4::json)::text)`,
		noticeChannel, topic, tenant, string(payload))
	if err != nil {
		return fmt.Errorf("bus: notice %s for %q: %w", topic, tenant, err)
	}
	return nil
}

// Listen calls fn with every notice sent on topic from this schema, by any
// process, until cancel is called. fn runs on the listening connection's
// goroutine and must not block.
func (n *Notices) Listen(topic string, fn func(tenant string, payload []byte)) (cancel func()) {
	n.mu.Lock()
	if n.listeners == nil {
		n.listeners = map[string]map[int]func(string, []byte){}
	}
	if n.listeners[topic] == nil {
		n.listeners[topic] = map[int]func(string, []byte){}
	}
	id := n.next
	n.next++
	n.listeners[topic][id] = fn
	n.count++
	if n.count == 1 {
		ctx, stop := context.WithCancel(context.Background())
		n.cancel = stop
		go pgListen(ctx, n.pool, n.retry, noticeChannel, &n.listening, n.connected, n.notified)
	}
	n.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			n.mu.Lock()
			defer n.mu.Unlock()
			delete(n.listeners[topic], id)
			if len(n.listeners[topic]) == 0 {
				delete(n.listeners, topic)
			}
			n.count--
			if n.count == 0 && n.cancel != nil {
				n.cancel()
				n.cancel = nil
			}
		})
	}
}

// Listening reports whether a connection is listening now.
func (n *Notices) Listening() bool { return n.listening.Load() }

// connected learns the schema a new connection sends from, so notices from
// another schema are dropped.
func (n *Notices) connected(ctx context.Context, conn *pgx.Conn) error {
	var schema string
	if err := conn.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		return err
	}
	n.mu.Lock()
	n.schema = schema
	n.mu.Unlock()
	return nil
}

func (n *Notices) notified(raw string) {
	var in notice
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		return
	}
	n.mu.Lock()
	if in.Schema != n.schema {
		n.mu.Unlock()
		return
	}
	fns := make([]func(string, []byte), 0, len(n.listeners[in.Topic]))
	for _, fn := range n.listeners[in.Topic] {
		fns = append(fns, fn)
	}
	n.mu.Unlock()
	for _, fn := range fns {
		fn(in.Tenant, in.Payload)
	}
}
