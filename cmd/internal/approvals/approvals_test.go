package approvals

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/types"
)

func TestStoreLifecycle(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "approvals"))
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(s.Dir()); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode: %v %v", info, err)
	}
	p := Pending{Token: NewToken(), Agent: "helper@1.0.0", Tool: "write_file", Arguments: map[string]any{"path": "a"},
		MaxGrant: types.GrantTool, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
	if err := s.Put(p); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(filepath.Join(s.Dir(), p.Token+".json")); info.Mode().Perm() != 0o600 {
		t.Fatalf("record mode %v", info.Mode())
	}
	list, err := s.List()
	if err != nil || len(list) != 1 || list[0].Tool != "write_file" || list[0].Decision != nil {
		t.Fatalf("list = %+v %v", list, err)
	}

	// A grant beyond the cap, or on a denial, is refused.
	if err := s.Decide(p.Token, Decision{Approved: true, Grant: &types.GrantRequest{Scope: types.GrantSession}}); !errors.Is(err, types.ErrInvalidGrant) {
		t.Fatalf("grant beyond the cap: %v", err)
	}
	if err := s.Decide(p.Token, Decision{Grant: &types.GrantRequest{Scope: types.GrantTool}}); err == nil {
		t.Fatal("a grant on a denial was accepted")
	}

	// Concurrent decisions: exactly one wins.
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.Decide(p.Token, Decision{Approved: i%2 == 0, Grant: nil})
		}()
	}
	wg.Wait()
	close(errs)
	wins := 0
	for err := range errs {
		switch {
		case err == nil:
			wins++
		case !errors.Is(err, ErrDecided):
			t.Fatalf("unexpected error %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d decisions won", wins)
	}
	e, err := s.Get(p.Token)
	if err != nil || e.Decision == nil || e.Decision.DecidedAt.IsZero() {
		t.Fatalf("get = %+v %v", e, err)
	}
	if err := s.Remove(p.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(p.Token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after remove: %v", err)
	}
}

func TestStoreRefusesExpiredAndMalformed(t *testing.T) {
	s, _ := Open(t.TempDir())
	p := Pending{Token: NewToken(), Tool: "x", CreatedAt: time.Now().Add(-2 * time.Hour), ExpiresAt: time.Now().Add(-time.Hour)}
	if err := s.Put(p); err != nil {
		t.Fatal(err)
	}
	if err := s.Decide(p.Token, Decision{Approved: true}); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired: %v", err)
	}
	for _, bad := range []string{"", "apr_../../etc/passwd", "apr_XYZ", "../" + p.Token} {
		if _, err := s.Get(bad); !errors.Is(err, ErrToken) {
			t.Fatalf("Get(%q) = %v", bad, err)
		}
	}
	if err := s.Decide(NewToken(), Decision{Approved: true}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown token: %v", err)
	}
}

func TestDefaultDir(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	tests := []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{EnvDir: "/x"}, "/x"},
		{map[string]string{"XDG_STATE_HOME": "/s", "HOME": "/h"}, "/s/saige/approvals"},
		{map[string]string{"HOME": "/h"}, "/h/.local/state/saige/approvals"},
		{map[string]string{}, ""},
	}
	for _, tt := range tests {
		if got := DefaultDir(env(tt.env)); got != tt.want {
			t.Fatalf("DefaultDir(%v) = %q, want %q", tt.env, got, tt.want)
		}
	}
}
