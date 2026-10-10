package main

import (
	"context"
	"crypto/subtle"
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
	"github.com/urmzd/saige/cmd/internal/agenthost"
)

// serveOptions configures the HTTP server behind saige serve.
// keyTurnID is the JSON field that names a turn in serve responses.
const keyTurnID = "turn_id"

// formatAGUI is the events query value that selects AG-UI events.
const formatAGUI = "agui"

// defaultMaxUpload caps one artifact upload unless --max-upload says
// otherwise.
const defaultMaxUpload = 32 << 20

type serveOptions struct {
	// newAgent builds the agent for a new session. Each session owns its
	// agent and conversation tree.
	newAgent func() (*agentsdk.Agent, error)
	// newSessionAgent, when set, replaces newAgent: it also returns what
	// the session releases when it ends, its grant limit, and a description
	// of the agent for the session's creation response.
	newSessionAgent func() (agenthost.Agent, error)
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
	// maxUpload caps one artifact upload, in bytes.
	maxUpload int64
	// artifactBudget caps the bytes each session's artifacts hold.
	artifactBudget int64
	// maxInline caps the inline media bytes of one part, in a turn's
	// request and in the events it streams; larger media goes through the
	// session's artifacts.
	maxInline int
	logger    *slog.Logger
}

// server holds sessions and their turns. Turns run on the server's context,
// not a request's, so they continue when a client disconnects.
type server struct {
	opts     serveOptions
	ctx      context.Context
	sessions *agenthost.Manager[*turns]
}

// session is one serve session: the shared session with serve's turns.
type session = agenthost.Session[*turns]

// turns are the turns a session ran, kept for replay.
type turns struct {
	mu   sync.Mutex
	byID map[string]*turn
	last *turn
}

// turn records one agent run as encoded wire envelopes for SSE replay.
type turn struct {
	id        string
	stream    *agentsdk.EventStream
	limit     int
	artifacts *agenthost.Artifacts

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

// sseEvent is one recorded delta: the delta itself, flattened with the
// tool call path it came through, and its wire v2 envelope.
type sseEvent struct {
	seq   uint64
	path  []string
	delta types.Delta
	kind  string
	data  []byte
}

func newServer(ctx context.Context, opts serveOptions) (*server, error) {
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
	if opts.maxUpload <= 0 {
		opts.maxUpload = defaultMaxUpload
	}
	if opts.maxInline <= 0 {
		opts.maxInline = types.DefaultMaxInlineBytes
	}
	if opts.logger == nil {
		opts.logger = slog.Default()
	}
	sessions, err := agenthost.New[*turns](agenthost.Config{
		Max: opts.maxSessions, IdleTTL: opts.idleTTL, Prefix: "s_", ArtifactBudget: opts.artifactBudget,
	})
	if err != nil {
		return nil, err
	}
	s := &server{opts: opts, ctx: ctx, sessions: sessions}
	go s.sessions.Sweep(ctx)
	return s, nil
}

// evictIdle drops every session with no running turn that has been idle
// for at least idleTTL.
func (s *server) evictIdle(now time.Time) { s.sessions.Evict(now) }

// handler routes the API. Every request passes the host and token checks;
// every POST must declare a JSON body, which a cross-site form cannot send
// without a CORS preflight this server never grants.
func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "wire_version": types.WireVersion, "wire_versions": []int{1, 2}})
	})
	mux.HandleFunc("POST /v1/sessions", s.createSession)
	mux.HandleFunc("DELETE /v1/sessions/{sid}", s.deleteSession)
	mux.HandleFunc("GET /v1/sessions/{sid}/tree", s.getTree)
	mux.HandleFunc("POST /v1/sessions/{sid}/artifacts", s.uploadArtifact)
	mux.HandleFunc("GET /v1/sessions/{sid}/artifacts/{id}", s.getArtifact)
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
			switch {
			case isUploadPath(r.URL.Path):
				// An upload declares the media's own type, but never one a
				// cross-site form can send without a preflight.
				if mt == "" || corsSimpleTypes[mt] {
					writeError(w, http.StatusUnsupportedMediaType, "uploads must declare the media type in Content-Type (use ?media_type= for text/plain)")
					return
				}
				r.Body = http.MaxBytesReader(w, r.Body, s.opts.maxUpload)
			case mt != "application/json":
				writeError(w, http.StatusUnsupportedMediaType, "POST requests must use Content-Type: application/json")
				return
			default:
				r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// corsSimpleTypes are the request content types a cross-site page can send
// without a CORS preflight.
var corsSimpleTypes = map[string]bool{
	"application/x-www-form-urlencoded": true,
	"multipart/form-data":               true,
	"text/plain":                        true,
}

// isUploadPath reports whether path is a session's artifact upload route.
func isUploadPath(path string) bool {
	rest, ok := strings.CutPrefix(path, "/v1/sessions/")
	if !ok {
		return false
	}
	sid, tail, _ := strings.Cut(rest, "/")
	return sid != "" && tail == "artifacts"
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
	sess, err := s.sessions.Create("", &turns{byID: map[string]*turn{}}, func() (agenthost.Agent, error) {
		if s.opts.newSessionAgent != nil {
			return s.opts.newSessionAgent()
		}
		a, err := s.opts.newAgent()
		return agenthost.Agent{Agent: a}, err
	})
	switch {
	case errors.Is(err, agenthost.ErrLimit):
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp := map[string]any{"session_id": sess.ID}
	if info := sess.Agent().Info; info != nil {
		resp["agent"] = info
	}
	writeJSON(w, http.StatusCreated, resp)
}

// deleteSession drops a session and cancels its running turn. Open event
// streams for its turns end with the turn.
func (s *server) deleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("sid")
	if s.sessions.Remove(id) == nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"session_id": id})
}

func (s *server) lookup(r *http.Request) *session {
	return s.sessions.Get(r.PathValue("sid"))
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
	sess.Host.mu.Lock()
	t := sess.Host.byID[r.PathValue("tid")]
	sess.Host.mu.Unlock()
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
	if err := tree.Print(w, sess.Agent().Agent.Tree()); err != nil {
		s.opts.logger.Warn("serve: print tree", "error", err)
	}
}

func (s *server) createTurn(w http.ResponseWriter, r *http.Request) {
	sess := s.session(w, r)
	if sess == nil {
		return
	}
	msg, status, code, err := s.turnMessage(r.Body, sess.Artifacts)
	if err != nil {
		writeCodedError(w, status, code, err.Error())
		return
	}

	sess.Host.mu.Lock()
	defer sess.Host.mu.Unlock()
	stream, err := sess.Start(s.ctx, msg)
	if err != nil {
		msg := err.Error()
		if errors.Is(err, agenthost.ErrBusy) && sess.Host.last != nil {
			// One run per branch: a new turn waits for the current one.
			msg += ": " + sess.Host.last.id
		}
		writeError(w, http.StatusConflict, msg)
		return
	}
	t := &turn{
		id:        "t_" + randomID(),
		stream:    stream,
		limit:     s.opts.bufferEvents,
		artifacts: sess.Artifacts,
		changed:   make(chan struct{}),
		timers:    map[string]*time.Timer{},
	}
	sess.Host.byID[t.id] = t
	sess.Host.last = t
	go s.record(t)
	writeJSON(w, http.StatusAccepted, map[string]string{keyTurnID: t.id})
}

// record drains the turn's stream into its event buffer. Each delta is
// flattened so sub-agent output carries its tool call path in the envelope
// instead of nested frames, and media over the inline limit is moved to the
// session's artifacts, so every client reads it by ref.
func (s *server) record(t *turn) {
	enc, _ := types.NewEncoder(types.EncodeOptions{MaxInlineBytes: s.opts.maxInline})
	for d := range t.stream.Deltas() {
		path, inner := types.FlattenDelta(d)
		if m, ok := inner.(types.MarkerDelta); ok {
			t.armApprovalTimeout(m.ToolCallID, s.opts.approvalTimeout)
		}
		for _, x := range agenthost.Externalize(inner, t.artifacts, s.opts.maxInline) {
			envs, err := enc.Encode(x)
			if err != nil || len(envs) != 1 {
				s.opts.logger.Warn("serve: encode delta", "turn", t.id, "error", err)
				continue
			}
			t.append(x, envs[0], path)
		}
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

// writeAGUIEvent writes one recorded delta as AG-UI events. A delta from a
// sub-agent is rewrapped in its tool calls, so the mapper reports it with
// its path.
func writeAGUIEvent(w io.Writer, m *agui.Mapper, ev sseEvent) error {
	d := ev.delta
	for i := len(ev.path) - 1; i >= 0; i-- {
		d = types.ToolExecDelta{ToolCallID: ev.path[i], Inner: d}
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
	events = append(events, m.Flush()...)
	for _, e := range events {
		if err := agui.WriteSSE(w, e); err != nil {
			return err
		}
	}
	return nil
}

func (t *turn) append(d types.Delta, env types.Envelope, path []string) {
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
	t.events = append(t.events, sseEvent{seq: t.seq, path: path, delta: d, kind: env.Kind, data: data})
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
//
// The client picks the wire version with ?wire=1|2 or with
// "Accept: application/vnd.saige.events+json;v=1"; the default is 2. A
// version 1 stream downgrades part deltas to version 1 kinds, reports
// output with no version 1 form as an error envelope, and names stored
// media by its saige-artifact:// URI. One recorded event can become
// several version 1 envelopes, which share its seq.
func (s *server) streamEvents(w http.ResponseWriter, r *http.Request) {
	t := s.turn(w, r)
	if t == nil {
		return
	}
	version, status, err := negotiateWire(r)
	if err != nil {
		writeError(w, status, err.Error())
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
	sid := r.PathValue("sid")
	if r.URL.Query().Get("format") == formatAGUI {
		mapper = agui.NewMapper(sid, t.id, agui.WithMediaLink(func(src types.Source) string {
			return artifactURL(sid, src)
		}))
	}
	wire := newWireWriter(version, s.opts.maxInline)

	h := w.Header()
	if mapper == nil {
		h.Set(headerWireVersion, strconv.Itoa(version))
	}
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
		// A version 1 stream feeds every event through its downgrader,
		// including those the client already has, so a resumed stream
		// pairs each part's deltas as the first one did.
		var batch []sseEvent
		for _, ev := range t.events {
			if ev.seq > wire.fed(last) {
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
			switch {
			case mapper != nil:
				err = writeAGUIEvent(w, mapper, ev)
			default:
				err = wire.write(w, ev, t.id, ev.seq > last)
			}
			if err != nil {
				return
			}
			last = max(last, ev.seq)
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
	res := agentsdk.Resolution{
		Approved:     *body.Approved,
		ModifiedArgs: body.ModifiedArgs,
		Message:      body.Message,
		Grant:        body.Grant,
	}
	check := func(r agentsdk.Resolution, now time.Time) error {
		return agenthost.ValidateGrant(r.Grant, r.Approved, now)
	}
	if sess := s.lookup(r); sess != nil {
		check = sess.CheckDecision
	}
	if err := check(res, time.Now()); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id := r.PathValue("toolCallID")
	err := t.stream.ResolveMarkerErr(id, res)
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

func randomID() string { return agenthost.RandomID() }
