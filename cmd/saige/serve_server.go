package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agui"
	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
)

// serveOptions configures the HTTP server behind saige serve.
// keyTurnID is the JSON field that names a turn in serve responses.
const keyTurnID = "turn_id"

// formatAGUI is the events query value that selects AG-UI events.
const formatAGUI = "agui"

type serveOptions struct {
	// newAgent builds the agent for a new session. Each session owns its
	// agent and conversation tree.
	newAgent func() (*agentsdk.Agent, error)
	// newSessionAgent, when set, replaces newAgent: it also returns what
	// the session releases when it ends, its grant limit, and a description
	// of the agent for the session's creation response.
	newSessionAgent func() (sessionAgent, error)
	// token, when set, must arrive as "Authorization: Bearer <token>".
	token string
	// approvalTimeout denies a pending approval nobody answered in time.
	approvalTimeout time.Duration
	// maxSessions caps live sessions.
	maxSessions int
	// idleTTL drops a session whose last turn finished this long ago, or
	// that never ran a turn and was created this long ago.
	idleTTL time.Duration
	// bufferEvents is how many events a turn keeps for replay.
	bufferEvents int
	// heartbeat is the interval between SSE keep-alive comments.
	heartbeat time.Duration
	logger    *slog.Logger
}

// server holds sessions and their turns. Turns run on the server's context,
// not a request's, so they continue when a client disconnects.
type server struct {
	opts     serveOptions
	ctx      context.Context
	mu       sync.Mutex
	sessions map[string]*session
	// reserved counts session slots held by creates that are still
	// building their agent, so concurrent creates cannot pass the cap.
	reserved int
}

// sessionAgent is an agent built for one session, with what goes with it.
type sessionAgent struct {
	agent *agentsdk.Agent
	// release frees what the agent holds, such as MCP connections.
	release func()
	// checkGrant rejects a grant the agent's definition does not allow.
	checkGrant func(*types.GrantRequest) error
	// info is reported as "agent" when the session is created, such as the
	// pinned definition the session runs.
	info any
}

type session struct {
	id    string
	agent *agentsdk.Agent
	// release and checkGrant come from sessionAgent; either may be nil.
	release    func()
	checkGrant func(*types.GrantRequest) error
	mu         sync.Mutex
	turns      map[string]*turn
	last       *turn
	// idleSince is when the session was created or its last turn
	// finished. It is guarded by mu.
	idleSince time.Time
}

// turn records one agent run as encoded wire envelopes for SSE replay.
type turn struct {
	id     string
	stream *agentsdk.EventStream
	limit  int

	mu      sync.Mutex
	events  []sseEvent // the most recent limit events, in seq order
	seq     uint64
	done    bool
	err     error
	changed chan struct{} // closed and replaced on every change
	timers  map[string]*time.Timer
	// finishedAt is when the turn ended; zero while it runs.
	finishedAt time.Time
}

type sseEvent struct {
	seq  uint64
	kind string
	data []byte
}

func newServer(ctx context.Context, opts serveOptions) *server {
	if opts.approvalTimeout <= 0 {
		opts.approvalTimeout = 10 * time.Minute
	}
	if opts.maxSessions <= 0 {
		opts.maxSessions = 64
	}
	if opts.bufferEvents <= 0 {
		opts.bufferEvents = 10000
	}
	if opts.idleTTL <= 0 {
		opts.idleTTL = time.Hour
	}
	if opts.heartbeat <= 0 {
		opts.heartbeat = 15 * time.Second
	}
	if opts.logger == nil {
		opts.logger = slog.Default()
	}
	s := &server{opts: opts, ctx: ctx, sessions: map[string]*session{}}
	go s.sweepIdle()
	return s
}

// sweepIdle drops idle sessions until the server's context ends.
func (s *server) sweepIdle() {
	tick := time.NewTicker(min(s.opts.idleTTL/4, time.Minute))
	defer tick.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-tick.C:
			s.evictIdle(now)
		}
	}
}

// evictIdle drops every session with no running turn that has been idle
// for at least idleTTL. A running turn keeps its session alive however
// long it takes.
func (s *server) evictIdle(now time.Time) {
	s.mu.Lock()
	var dropped []*session
	for id, sess := range s.sessions {
		if sess.idleFor(now) >= s.opts.idleTTL {
			delete(s.sessions, id)
			dropped = append(dropped, sess)
		}
	}
	s.mu.Unlock()
	for _, sess := range dropped {
		sess.end()
	}
}

// end releases what the session's agent holds.
func (sess *session) end() {
	if sess.release != nil {
		sess.release()
	}
}

// idleFor reports how long the session has had no running turn, or zero
// while a turn runs.
func (sess *session) idleFor(now time.Time) time.Duration {
	sess.mu.Lock()
	last, since := sess.last, sess.idleSince
	sess.mu.Unlock()
	if last != nil {
		last.mu.Lock()
		done, finished := last.done, last.finishedAt
		last.mu.Unlock()
		if !done {
			return 0
		}
		since = finished
	}
	return now.Sub(since)
}

// handler routes the API. Every request passes the host and token checks;
// every POST must declare a JSON body, which a cross-site form cannot send
// without a CORS preflight this server never grants.
func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "wire_version": types.WireVersion})
	})
	mux.HandleFunc("POST /v1/sessions", s.createSession)
	mux.HandleFunc("DELETE /v1/sessions/{sid}", s.deleteSession)
	mux.HandleFunc("GET /v1/sessions/{sid}/tree", s.getTree)
	mux.HandleFunc("POST /v1/sessions/{sid}/turns", s.createTurn)
	mux.HandleFunc("GET /v1/sessions/{sid}/turns/{tid}", s.getTurn)
	mux.HandleFunc("GET /v1/sessions/{sid}/turns/{tid}/events", s.streamEvents)
	mux.HandleFunc("POST /v1/sessions/{sid}/turns/{tid}/interrupts/{toolCallID...}", s.resolveInterrupt)
	mux.HandleFunc("POST /v1/sessions/{sid}/turns/{tid}/cancel", s.cancelTurn)
	return s.guard(mux)
}

func (s *server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.opts.token == "" && !isLoopbackHost(r.Host) {
			// Without a token the server trusts only local clients; a
			// foreign Host header means DNS rebinding or a proxied request.
			writeError(w, http.StatusForbidden, "host not allowed")
			return
		}
		if s.opts.token != "" {
			got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.opts.token)) != 1 {
				writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
				return
			}
		}
		if r.Method == http.MethodPost {
			mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if mt != "application/json" {
				writeError(w, http.StatusUnsupportedMediaType, "POST requests must use Content-Type: application/json")
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		}
		next.ServeHTTP(w, r)
	})
}

// isLoopbackHost reports whether a Host header names this machine.
func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *server) createSession(w http.ResponseWriter, _ *http.Request) {
	// The slot is reserved before the agent is built, so the cap holds
	// however many creates run at once.
	s.mu.Lock()
	if len(s.sessions)+s.reserved >= s.opts.maxSessions {
		s.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, "session limit reached")
		return
	}
	s.reserved++
	s.mu.Unlock()

	var sa sessionAgent
	var err error
	if s.opts.newSessionAgent != nil {
		sa, err = s.opts.newSessionAgent()
	} else {
		sa.agent, err = s.opts.newAgent()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reserved--
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	sess := &session{id: "s_" + randomID(), agent: sa.agent, release: sa.release, checkGrant: sa.checkGrant,
		turns: map[string]*turn{}, idleSince: time.Now()}
	s.sessions[sess.id] = sess
	resp := map[string]any{"session_id": sess.id}
	if sa.info != nil {
		resp["agent"] = sa.info
	}
	writeJSON(w, http.StatusCreated, resp)
}

// deleteSession drops a session and cancels its running turn. Open event
// streams for its turns end with the turn.
func (s *server) deleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("sid")
	s.mu.Lock()
	sess := s.sessions[id]
	delete(s.sessions, id)
	s.mu.Unlock()
	if sess == nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	sess.mu.Lock()
	last := sess.last
	sess.mu.Unlock()
	if last != nil {
		last.stream.Cancel()
	}
	sess.end()
	writeJSON(w, http.StatusOK, map[string]string{"session_id": id})
}

func (s *server) lookup(r *http.Request) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[r.PathValue("sid")]
}

func (s *server) session(w http.ResponseWriter, r *http.Request) *session {
	sess := s.lookup(r)
	if sess == nil {
		writeError(w, http.StatusNotFound, "session not found")
	}
	return sess
}

func (s *server) turn(w http.ResponseWriter, r *http.Request) *turn {
	sess := s.session(w, r)
	if sess == nil {
		return nil
	}
	sess.mu.Lock()
	t := sess.turns[r.PathValue("tid")]
	sess.mu.Unlock()
	if t == nil {
		writeError(w, http.StatusNotFound, "turn not found")
	}
	return t
}

func (s *server) getTree(w http.ResponseWriter, r *http.Request) {
	sess := s.session(w, r)
	if sess == nil {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := tree.Print(w, sess.agent.Tree()); err != nil {
		s.opts.logger.Warn("serve: print tree", "error", err)
	}
}

func (s *server) createTurn(w http.ResponseWriter, r *http.Request) {
	sess := s.session(w, r)
	if sess == nil {
		return
	}
	var body struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Message) == "" {
		writeError(w, http.StatusBadRequest, `body must be {"message": "<non-empty text>"}`)
		return
	}

	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.last != nil && !sess.last.finished() {
		// One run per branch: a new turn waits for the current one.
		writeError(w, http.StatusConflict, "a turn is already running in this session: "+sess.last.id)
		return
	}
	t := &turn{
		id:      "t_" + randomID(),
		limit:   s.opts.bufferEvents,
		changed: make(chan struct{}),
		timers:  map[string]*time.Timer{},
	}
	t.stream = sess.agent.Invoke(s.ctx, []types.Message{types.UserMsg(types.Text(body.Message))})
	sess.turns[t.id] = t
	sess.last = t
	go s.record(t)
	writeJSON(w, http.StatusAccepted, map[string]string{keyTurnID: t.id})
}

// record drains the turn's stream into its event buffer. Each delta is
// flattened so sub-agent output carries its tool call path in the envelope
// instead of nested frames.
func (s *server) record(t *turn) {
	for d := range t.stream.Deltas() {
		path, inner := types.FlattenDelta(d)
		env, err := types.NewDeltaEnvelope(inner)
		if err != nil {
			s.opts.logger.Warn("serve: encode delta", "turn", t.id, "error", err)
			continue
		}
		if m, ok := inner.(types.MarkerDelta); ok {
			t.armApprovalTimeout(m.ToolCallID, s.opts.approvalTimeout)
		}
		t.append(env, path)
	}
	err := t.stream.Wait()
	t.mu.Lock()
	t.done = true
	t.err = err
	t.finishedAt = time.Now()
	for _, tm := range t.timers {
		tm.Stop()
	}
	t.notifyLocked()
	t.mu.Unlock()
}

// writeAGUIEvent writes one recorded envelope as AG-UI events. A delta from
// a sub-agent is rewrapped in its tool calls, so the mapper reports it with
// its path.
func writeAGUIEvent(w io.Writer, m *agui.Mapper, ev sseEvent) error {
	env, err := types.UnmarshalEnvelope(ev.data)
	if err != nil {
		return err
	}
	d, err := env.Delta()
	if err != nil {
		return err
	}
	for i := len(env.Path) - 1; i >= 0; i-- {
		d = types.ToolExecDelta{ToolCallID: env.Path[i], Inner: d}
	}
	events, err := m.Map(d)
	if err != nil {
		return err
	}
	for _, e := range events {
		if _, err := fmt.Fprintf(w, "id: %d\n", ev.seq); err != nil {
			return err
		}
		if err := agui.WriteSSE(w, e); err != nil {
			return err
		}
	}
	return nil
}

// finishAGUI ends an AG-UI stream whose turn is done. A turn that failed
// without an error delta reports its error; one that stopped without a done
// delta reports an incomplete stream.
func finishAGUI(w io.Writer, m *agui.Mapper, turnErr error) error {
	var events []agui.Event
	if turnErr != nil && !m.Finished() {
		events, _ = m.Map(types.ErrorDelta{Error: turnErr})
	}
	events = append(events, m.Close()...)
	for _, e := range events {
		if err := agui.WriteSSE(w, e); err != nil {
			return err
		}
	}
	return nil
}

func (t *turn) append(env types.Envelope, path []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seq++
	env.Seq = t.seq
	env.RunID = t.id
	env.Path = path
	data, err := json.Marshal(env)
	if err != nil {
		return
	}
	t.events = append(t.events, sseEvent{seq: t.seq, kind: env.Kind, data: data})
	if over := len(t.events) - t.limit; over > 0 {
		t.events = append(t.events[:0:0], t.events[over:]...)
	}
	t.notifyLocked()
}

func (t *turn) notifyLocked() {
	close(t.changed)
	t.changed = make(chan struct{})
}

func (t *turn) finished() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.done
}

// armApprovalTimeout denies the call if no decision arrives in time. An
// unanswered approval is never treated as a yes, including when the client
// that would answer it has disconnected.
func (t *turn) armApprovalTimeout(toolCallID string, d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if old := t.timers[toolCallID]; old != nil {
		old.Stop()
	}
	t.timers[toolCallID] = time.AfterFunc(d, func() {
		_ = t.stream.ResolveMarkerErr(toolCallID, agentsdk.Resolution{
			Approved: false,
			Message:  "approval timed out with no decision; the call was denied",
		})
	})
}

func (t *turn) disarm(toolCallID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if tm := t.timers[toolCallID]; tm != nil {
		tm.Stop()
		delete(t.timers, toolCallID)
	}
}

func (s *server) getTurn(w http.ResponseWriter, r *http.Request) {
	t := s.turn(w, r)
	if t == nil {
		return
	}
	t.mu.Lock()
	resp := map[string]any{keyTurnID: t.id, "done": t.done, "last_seq": t.seq}
	if t.err != nil {
		resp["error"] = t.err.Error()
	}
	t.mu.Unlock()
	writeJSON(w, http.StatusOK, resp)
}

// streamEvents serves the turn as Server-Sent Events: id is the envelope
// seq, event is its kind, and data is the envelope. A client that
// reconnects with Last-Event-ID (or ?after=) receives only later events.
// The stream ends after the turn's last event.
//
// The turn keeps only its most recent events. When the next event a client
// needs is no longer kept, the request fails with 410 Gone before the stream
// starts, or a "gap" event ends a stream that fell behind. Either way the
// client must refetch the turn and tree, then resume from oldest_seq - 1.
func (s *server) streamEvents(w http.ResponseWriter, r *http.Request) {
	t := s.turn(w, r)
	if t == nil {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported")
		return
	}
	after := r.Header.Get("Last-Event-ID")
	if after == "" {
		after = r.URL.Query().Get("after")
	}
	var last uint64
	if after != "" {
		n, err := strconv.ParseUint(after, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "Last-Event-ID must be a sequence number")
			return
		}
		last = n
	}
	if oldest, ok := t.missing(last); ok {
		writeJSON(w, http.StatusGone, map[string]any{
			"error":      "events after the requested sequence are no longer kept",
			"oldest_seq": oldest,
		})
		return
	}

	// format=agui streams AG-UI events instead of wire envelopes.
	var mapper *agui.Mapper
	if r.URL.Query().Get("format") == formatAGUI {
		mapper = agui.NewMapper(r.PathValue("sid"), t.id)
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if mapper != nil && last == 0 {
		if err := agui.WriteSSE(w, mapper.Start()); err != nil {
			return
		}
	}
	flusher.Flush()

	heartbeat := time.NewTicker(s.opts.heartbeat)
	defer heartbeat.Stop()
	for {
		t.mu.Lock()
		oldest, gap := t.missingLocked(last)
		var batch []sseEvent
		for _, ev := range t.events {
			if ev.seq > last {
				batch = append(batch, ev)
			}
		}
		done, changed, turnErr := t.done, t.changed, t.err
		t.mu.Unlock()

		if gap {
			// This client fell more than the buffer behind the turn.
			_, _ = fmt.Fprintf(w, "event: gap\ndata: {\"oldest_seq\":%d}\n\n", oldest) //nolint:gosec // an integer, sent as an SSE event, not HTML
			flusher.Flush()
			return
		}

		for _, ev := range batch {
			var err error
			if mapper != nil {
				err = writeAGUIEvent(w, mapper, ev)
			} else {
				_, err = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.seq, ev.kind, ev.data)
			}
			if err != nil {
				return
			}
			last = ev.seq
		}
		if len(batch) > 0 {
			flusher.Flush()
		}
		if done {
			if mapper != nil {
				_ = finishAGUI(w, mapper, turnErr)
				flusher.Flush()
			}
			return
		}
		select {
		case <-changed:
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// missing reports whether the event after seq last has been dropped from
// the buffer, and the oldest seq still kept.
func (t *turn) missing(last uint64) (uint64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.missingLocked(last)
}

func (t *turn) missingLocked(last uint64) (uint64, bool) {
	if len(t.events) == 0 {
		return 0, false
	}
	oldest := t.events[0].seq
	return oldest, last+1 < oldest
}

// resolveInterrupt answers a marker. A sub-agent's marker id contains the
// tool call ids that lead to it joined by "/", so the id takes the rest of
// the path; a client may send the slashes as-is or percent-encoded.
func (s *server) resolveInterrupt(w http.ResponseWriter, r *http.Request) {
	t := s.turn(w, r)
	if t == nil {
		return
	}
	var body struct {
		Approved     *bool               `json:"approved"`
		Message      string              `json:"message"`
		ModifiedArgs map[string]any      `json:"modified_args"`
		Grant        *types.GrantRequest `json:"grant"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Approved == nil {
		// A decision must be explicit: a missing field is never a yes.
		writeError(w, http.StatusBadRequest, `body must include "approved": true or false`)
		return
	}
	if err := checkGrant(body.Grant, *body.Approved, time.Now()); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if sess := s.lookup(r); sess != nil && sess.checkGrant != nil {
		if err := sess.checkGrant(body.Grant); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	id := r.PathValue("toolCallID")
	err := t.stream.ResolveMarkerErr(id, agentsdk.Resolution{
		Approved:     *body.Approved,
		ModifiedArgs: body.ModifiedArgs,
		Message:      body.Message,
		Grant:        body.Grant,
	})
	switch {
	case errors.Is(err, agentsdk.ErrUnknownMarker):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, agentsdk.ErrMarkerResolved):
		writeError(w, http.StatusConflict, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		t.disarm(id)
		writeJSON(w, http.StatusOK, map[string]any{"tool_call_id": id, "approved": *body.Approved})
	}
}

// checkGrant validates the grant a client attached to a decision: only an
// approval can carry one, its scope and matchers must be well formed, and
// its expiry must lie in the future.
func checkGrant(g *types.GrantRequest, approved bool, now time.Time) error {
	if g == nil {
		return nil
	}
	if !approved {
		return errors.New(`"grant" requires "approved": true`)
	}
	if err := g.Validate(); err != nil {
		return err
	}
	if !g.ExpiresAt.IsZero() && !g.ExpiresAt.After(now) {
		return fmt.Errorf("%w: expires_at %s is not in the future", types.ErrInvalidGrant, g.ExpiresAt.Format(time.RFC3339))
	}
	return nil
}

func (s *server) cancelTurn(w http.ResponseWriter, r *http.Request) {
	t := s.turn(w, r)
	if t == nil {
		return
	}
	t.stream.Cancel()
	writeJSON(w, http.StatusAccepted, map[string]string{keyTurnID: t.id})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func randomID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
