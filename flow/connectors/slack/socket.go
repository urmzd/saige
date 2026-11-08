package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/coder/websocket"
)

// SocketOpenURL is Slack's apps.connections.open, which hands out a Socket
// Mode websocket URL for an app-level token.
const SocketOpenURL = "https://slack.com/api/apps.connections.open"

// Socket Mode envelope types Slack sends.
const (
	socketHello       = "hello"
	socketDisconnect  = "disconnect"
	socketEvents      = "events_api"
	socketCommands    = "slash_commands"
	socketInteractive = "interactive"
)

// maxSocketMessage bounds one envelope read from the socket, as MaxBody
// bounds an HTTP request.
const maxSocketMessage = 1 << 20

// SocketConfig turns on Socket Mode for a connector.
type SocketConfig struct {
	// AppToken is the app-level token (xapp-...) with connections:write.
	AppToken string
	// OpenURL is apps.connections.open; empty means SocketOpenURL. Tests
	// point it at a fake.
	OpenURL string
	// HTTPClient makes the open call; nil means http.DefaultClient.
	HTTPClient *http.Client
	// OnError hears why a connection ended before it is opened again; nil
	// drops it. The connector always reconnects until Ingest ends.
	OnError func(error)
	// MaxBackoff caps the wait between failed connections; zero means one
	// minute. The first wait is one second, doubling.
	MaxBackoff time.Duration
}

type socketEnvelope struct {
	Type       string          `json:"type"`
	EnvelopeID string          `json:"envelope_id"`
	Payload    json.RawMessage `json:"payload"`
	Reason     string          `json:"reason"`
}

// errSocketRefresh is Slack asking for a new connection; it is opened at
// once rather than after a backoff.
var errSocketRefresh = errors.New("slack: socket mode: Slack asked for a new connection")

// socket holds a Socket Mode connection open until ctx ends, opening a new
// one whenever Slack closes it or it fails.
func (c *Connector) socket(ctx context.Context) {
	s := c.cfg.Socket
	ceiling := s.MaxBackoff
	if ceiling <= 0 {
		ceiling = time.Minute
	}
	wait := time.Second
	for ctx.Err() == nil {
		served, err := c.socketOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if s.OnError != nil && err != nil && !errors.Is(err, errSocketRefresh) {
			s.OnError(err)
		}
		if served || errors.Is(err, errSocketRefresh) {
			wait = time.Second
			if errors.Is(err, errSocketRefresh) {
				continue
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, ceiling)
	}
}

// socketOnce opens one connection and serves it until it ends. served
// reports whether Slack greeted it, so a connection that worked resets the
// backoff.
func (c *Connector) socketOnce(ctx context.Context) (served bool, err error) {
	at, err := c.openSocket(ctx)
	if err != nil {
		return false, err
	}
	conn, resp, err := websocket.Dial(ctx, at, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return false, fmt.Errorf("slack: socket mode: dial: %w", err)
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(maxSocketMessage)
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return served, fmt.Errorf("slack: socket mode: read: %w", err)
		}
		var env socketEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			return served, fmt.Errorf("slack: socket mode: malformed envelope: %w", err)
		}
		switch env.Type {
		case socketHello:
			served = true
			continue
		case socketDisconnect:
			return served, errSocketRefresh
		}
		ack, ok := c.serveEnvelope(ctx, env)
		if !ok || env.EnvelopeID == "" {
			// Unacknowledged, Slack delivers it again.
			continue
		}
		ack["envelope_id"] = env.EnvelopeID
		out, err := json.Marshal(ack)
		if err != nil {
			return served, err
		}
		if err := conn.Write(ctx, websocket.MessageText, out); err != nil {
			return served, fmt.Errorf("slack: socket mode: ack: %w", err)
		}
	}
}

// serveEnvelope hands one envelope to the HTTP handler's verified half and
// reports whether to acknowledge it, with any response payload. The socket
// is this app's own, opened with its app token, so its envelopes need no
// signature.
func (c *Connector) serveEnvelope(ctx context.Context, env socketEnvelope) (map[string]any, bool) {
	path, body := "/", []byte(env.Payload)
	switch env.Type {
	case socketEvents:
	case socketCommands:
		var fields map[string]any
		if err := json.Unmarshal(env.Payload, &fields); err != nil {
			return nil, false
		}
		form := url.Values{}
		for k, v := range fields {
			if s, ok := v.(string); ok {
				form.Set(k, s)
			}
		}
		path, body = CommandsPath, []byte(form.Encode())
	case socketInteractive:
		// Not read yet: acknowledged so Slack does not deliver it again.
		return map[string]any{}, true
	default:
		return map[string]any{}, true
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewReader(body))
	if err != nil {
		return nil, false
	}
	rec := &socketReply{header: http.Header{}, status: http.StatusOK}
	c.serveVerified(rec, r, body)
	if rec.status >= http.StatusMultipleChoices {
		return nil, false
	}
	ack := map[string]any{}
	if rec.body.Len() > 0 && json.Valid(rec.body.Bytes()) {
		ack["payload"] = json.RawMessage(rec.body.Bytes())
	}
	return ack, true
}

// openSocket asks Slack for a Socket Mode URL with the app token.
func (c *Connector) openSocket(ctx context.Context) (string, error) {
	s := c.cfg.Socket
	if s.AppToken == "" {
		return "", errors.New("slack: socket mode needs an app-level token")
	}
	endpoint := s.OpenURL
	if endpoint == "" {
		endpoint = SocketOpenURL
	}
	client := s.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+s.AppToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("slack: socket mode: open: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		OK    bool   `json:"ok"`
		URL   string `json:"url"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil {
		return "", fmt.Errorf("slack: socket mode: open: %w", err)
	}
	if !out.OK || out.URL == "" {
		return "", fmt.Errorf("slack: socket mode: open: %s", out.Error)
	}
	return out.URL, nil
}

// socketReply is the ResponseWriter an envelope is served into: the status
// decides the acknowledgement, and a JSON body becomes its payload.
type socketReply struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *socketReply) Header() http.Header         { return r.header }
func (r *socketReply) WriteHeader(status int)      { r.status = status }
func (r *socketReply) Write(b []byte) (int, error) { return r.body.Write(b) }
