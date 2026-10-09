package fetch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestCheckAddress(t *testing.T) {
	tests := []struct {
		addr         string
		blockPrivate bool
		blocked      bool
	}{
		{"93.184.216.34:443", true, false},
		{"[2606:4700::1111]:443", true, false},
		{"169.254.169.254:80", false, true},
		{"[::ffff:169.254.169.254]:80", false, true},
		{"[fd00:ec2::254]:80", false, true},
		{"100.100.100.200:80", false, true},
		{"[fe80::1]:80", false, true},
		{"127.0.0.1:80", true, true},
		{"127.0.0.1:80", false, false},
		{"10.0.0.5:80", true, true},
		{"192.168.1.1:80", true, true},
		{"172.16.0.1:80", true, true},
		{"100.64.0.1:80", true, true},
		{"[::1]:80", true, true},
		{"[fc00::1]:80", true, true},
		{"0.0.0.0:80", true, true},
		{"224.0.0.1:80", true, true},
		{"example.com:80", false, true},
	}
	for _, tt := range tests {
		err := CheckAddress(tt.addr, tt.blockPrivate)
		if got := errors.Is(err, ErrBlockedAddress); got != tt.blocked {
			t.Errorf("CheckAddress(%q, %v) = %v, blocked want %v", tt.addr, tt.blockPrivate, err, tt.blocked)
		}
	}
}

func TestFetch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/page", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><title>Doc</title><style>.x{}</style></head>
<body><h1>Hello</h1><script>alert(1)</script><p>First   para.</p><svg/><p>Second</p></body></html>`))
	})
	mux.HandleFunc("/json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/big", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(strings.Repeat("a", 5000)))
	})
	mux.HandleFunc("/bin", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte{0x89, 'P', 'N', 'G'})
	})
	mux.HandleFunc("/missing", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	})
	mux.HandleFunc("/to-metadata", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	})
	mux.HandleFunc("/to-page", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/page", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	local := NewTool(AllowPrivateNetworks(), WithMaxBytes(4096))
	tests := []struct {
		name    string
		tool    types.Tool
		args    map[string]any
		want    []string
		wantNot []string
		wantErr error
	}{
		{name: "loopback refused by default", tool: NewTool(), args: map[string]any{"url": srv.URL + "/page"}, wantErr: ErrBlockedAddress},
		{name: "html converted to text", tool: local, args: map[string]any{"url": srv.URL + "/page"}, want: []string{"Status: 200", "Doc\n\nHello\n", "First para.", "Second"}, wantNot: []string{"alert", ".x{}", "<p>"}},
		{name: "json returned as is", tool: local, args: map[string]any{"url": srv.URL + "/json"}, want: []string{`{"ok":true}`}},
		{name: "max_chars clips", tool: local, args: map[string]any{"url": srv.URL + "/big", "max_chars": float64(100)}, want: []string{"more characters"}},
		{name: "body cap", tool: local, args: map[string]any{"url": srv.URL + "/big"}, want: []string{"exceeded 4096 bytes"}},
		{name: "redirect followed", tool: local, args: map[string]any{"url": srv.URL + "/to-page"}, want: []string{"/page", "Hello"}},
		{name: "redirect to metadata refused", tool: local, args: map[string]any{"url": srv.URL + "/to-metadata"}, wantErr: ErrBlockedAddress},
		{name: "binary refused", tool: local, args: map[string]any{"url": srv.URL + "/bin"}, wantErr: errAny},
		{name: "http error is an error", tool: local, args: map[string]any{"url": srv.URL + "/missing"}, wantErr: errAny},
		{name: "scheme refused", tool: local, args: map[string]any{"url": "file:///etc/passwd"}, wantErr: errAny},
		{name: "relative url refused", tool: local, args: map[string]any{"url": "/page"}, wantErr: errAny},
		{name: "credentials refused", tool: local, args: map[string]any{"url": "http://u:p@example.com/"}, wantErr: errAny},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.tool.Execute(context.Background(), tt.args)
			switch {
			case tt.wantErr == errAny:
				if err == nil {
					t.Fatalf("expected an error, got %q", got)
				}
				return
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			case err != nil:
				t.Fatal(err)
			}
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("output missing %q:\n%s", w, got)
				}
			}
			for _, w := range tt.wantNot {
				if strings.Contains(got, w) {
					t.Errorf("output contains %q:\n%s", w, got)
				}
			}
		})
	}
}

var errAny = errors.New("any error")

func TestFetchIsReadOnly(t *testing.T) {
	if c := NewTool().Definition().Capability; c != types.ToolCapabilityRead {
		t.Errorf("capability = %q, want read", c)
	}
}
