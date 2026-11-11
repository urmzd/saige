package credential_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urmzd/saige/flow/credential"
)

func TestClientSendsTheSourcesCurrentTokenOnEveryRequest(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization"))
	}))
	defer srv.Close()

	src := credential.NewStatic()
	src.Set("github", credential.App("42"), "first")
	c := credential.Client(src, "github", credential.App("42"), nil)
	for _, next := range []string{"second", ""} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
		req.Header.Set("Authorization", "Bearer stale")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if next != "" {
			src.Set("github", credential.App("42"), next)
		}
	}
	if want := []string{"Bearer first", "Bearer second"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("Authorization headers %q, want %q", got, want)
	}
}

func TestAPersonWithNoCredentialGetsNoneAndNoOneElses(t *testing.T) {
	sent := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { sent = true }))
	defer srv.Close()

	src := credential.NewStatic()
	src.Set("slack", credential.App("T01"), "xoxb-app")
	src.Set("slack", credential.User("ada"), "xoxp-ada")

	for _, s := range []credential.Subject{credential.User("bob"), credential.User(""), {Kind: "team", ID: "T01"}} {
		_, err := credential.Client(src, "slack", s, nil).Get(srv.URL)
		if !errors.Is(err, credential.ErrNoCredential) {
			t.Fatalf("%v: err %v, want ErrNoCredential", s, err)
		}
	}
	if sent {
		t.Fatal("a request without a credential reached the server")
	}
	tok, err := src.Token(context.Background(), "slack", credential.User("ada"))
	if err != nil || tok.Value != "xoxp-ada" {
		t.Fatalf("ada's token %v, %v", tok.Value, err)
	}
}

func TestATokenNeverPrintsItsValue(t *testing.T) {
	tok := credential.Token{Value: "xoxb-secret"}
	for _, s := range []string{fmt.Sprint(tok), fmt.Sprintf("%v %+v %#v %s", tok, tok, tok, tok)} {
		if strings.Contains(s, "xoxb-secret") {
			t.Fatalf("token printed its value: %s", s)
		}
	}
}

func TestSourceFuncIsASource(t *testing.T) {
	var asked string
	var src credential.Source = credential.SourceFunc(func(_ context.Context, c string, s credential.Subject) (credential.Token, error) {
		asked = c + " " + s.String()
		return credential.Token{Value: "v"}, nil
	})
	if _, err := src.Token(context.Background(), "github", credential.User("ada")); err != nil || asked != "github user:ada" {
		t.Fatalf("asked %q, err %v", asked, err)
	}
}
