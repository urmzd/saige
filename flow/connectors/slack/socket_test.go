package slack_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/urmzd/saige/flow/connectors/slack"
)

// fakeSocketSlack is Slack's side of Socket Mode: apps.connections.open
// hands out a websocket, and each connection runs script.
type fakeSocketSlack struct {
	t      *testing.T
	server *httptest.Server
	script func(ctx context.Context, conn *websocket.Conn, n int)

	mu    sync.Mutex
	opens int
	token string
}

func newFakeSocketSlack(t *testing.T, script func(ctx context.Context, conn *websocket.Conn, n int)) *fakeSocketSlack {
	t.Helper()
	f := &fakeSocketSlack{t: t, script: script}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /open", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.opens++
		f.token = r.Header.Get("Authorization")
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "url": "ws" + strings.TrimPrefix(f.server.URL, "http") + "/socket"})
	})
	mux.HandleFunc("GET /socket", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer func() { _ = conn.CloseNow() }()
		f.mu.Lock()
		n := f.opens
		f.mu.Unlock()
		f.script(r.Context(), conn, n)
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeSocketSlack) config(onError func(error)) *slack.SocketConfig {
	return &slack.SocketConfig{AppToken: "xapp-test", OpenURL: f.server.URL + "/open", OnError: onError, MaxBackoff: 10 * time.Millisecond}
}

func send(ctx context.Context, t *testing.T, conn *websocket.Conn, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
		t.Errorf("write: %v", err)
	}
}

func ackOf(ctx context.Context, t *testing.T, conn *websocket.Conn) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Errorf("no ack: %v", err)
		return ""
	}
	var ack struct {
		EnvelopeID string `json:"envelope_id"`
	}
	_ = json.Unmarshal(data, &ack)
	return ack.EnvelopeID
}

// In Socket Mode an event arrives over the app's own websocket, with no
// public URL and no signature, is published as an HTTP delivery would be,
// and is acknowledged. When Slack asks for a new connection, one is opened.
func TestSocketModeDeliversEventsAndReconnects(t *testing.T) {
	acks := make(chan string, 4)
	f := newFakeSocketSlack(t, func(ctx context.Context, conn *websocket.Conn, n int) {
		send(ctx, t, conn, map[string]any{"type": "hello"})
		if n == 1 {
			send(ctx, t, conn, map[string]any{"type": "events_api", "envelope_id": "env-1", "payload": json.RawMessage(appMention)})
			acks <- ackOf(ctx, t, conn)
			send(ctx, t, conn, map[string]any{"type": "interactive", "envelope_id": "env-2", "payload": map[string]any{"type": "block_actions"}})
			acks <- ackOf(ctx, t, conn)
			send(ctx, t, conn, map[string]any{"type": "disconnect", "reason": "refresh_requested"})
			return
		}
		<-ctx.Done()
	})
	var mu sync.Mutex
	var failures []error
	_, tags := start(t, slack.Config{Socket: f.config(func(err error) { mu.Lock(); failures = append(failures, err); mu.Unlock() })})

	select {
	case e := <-tags:
		if !strings.Contains(e.Channel, "C02CHANNEL") {
			t.Fatalf("published %+v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the socket's event was not published")
	}
	for _, want := range []string{"env-1", "env-2"} {
		select {
		case got := <-acks:
			if got != want {
				t.Fatalf("acknowledged %q, want %q", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s was not acknowledged", want)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		opens, token := f.opens, f.token
		f.mu.Unlock()
		if opens >= 2 {
			if token != "Bearer xapp-test" {
				t.Fatalf("opened with %q", token)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no new connection after Slack asked for one")
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(failures) != 0 {
		t.Fatalf("a refresh was reported as a failure: %v", failures)
	}
}

// A refused open is reported and tried again; nothing is published.
func TestSocketModeRetriesARefusedOpen(t *testing.T) {
	var mu sync.Mutex
	opens := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		opens++
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "invalid_auth"})
	}))
	t.Cleanup(server.Close)
	errs := make(chan error, 8)
	start(t, slack.Config{Socket: &slack.SocketConfig{AppToken: "xapp-bad", OpenURL: server.URL, MaxBackoff: 5 * time.Millisecond, OnError: func(err error) {
		select {
		case errs <- err:
		default:
		}
	}}})
	select {
	case err := <-errs:
		if !strings.Contains(err.Error(), "invalid_auth") {
			t.Fatalf("reported %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a refused open was not reported")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := opens
		mu.Unlock()
		if n >= 2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a refused open was not tried again")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
