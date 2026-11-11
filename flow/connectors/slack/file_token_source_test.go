package slack

import (
	"context"
	"errors"
	"net/url"
	"testing"
)

func TestFileTokenSourceRotatesAndNeverFallsBack(t *testing.T) {
	token := "first"
	calls := 0
	fail := false
	headers := fileRequestHeaders("stale", func(context.Context) (string, error) {
		calls++
		if fail {
			return "", errors.New("secret refresh failure")
		}
		return token, nil
	})
	u, _ := url.Parse("https://files.slack.com/files-pri/example")
	for _, want := range []string{"first", "second"} {
		token = want
		got, err := headers(context.Background(), u)
		if err != nil || got.Get("Authorization") != "Bearer "+want {
			t.Fatal("wrong rotating header", err)
		}
	}
	for _, raw := range []string{"https://files.slack.com.evil.test/file", "https://files.slack.com:444/file", "http://files.slack.com/file", "https://cdn.slack-edge.com/file"} {
		foreign, _ := url.Parse(raw)
		got, err := headers(context.Background(), foreign)
		if err != nil || got.Get("Authorization") != "" {
			t.Fatal("token sent outside exact host")
		}
	}
	if calls != 2 {
		t.Fatal("source called for another origin")
	}
	fail = true
	got, err := headers(context.Background(), u)
	if err == nil || got.Get("Authorization") != "" || err.Error() == "secret refresh failure" {
		t.Fatal("refresh failure fell back or leaked details")
	}
}
