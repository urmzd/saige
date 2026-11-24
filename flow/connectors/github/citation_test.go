package github_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urmzd/saige/flow/connectors/github"
	"github.com/urmzd/saige/flow/event"
)

// retrieved returns citation events for three passages, as a turn that
// retrieved them publishes them, starting at seq.
func retrieved(seq uint64) []event.Envelope {
	var out []event.Envelope
	for i, name := range []string{"alpha", "beta", "gamma"} {
		out = append(out, ev(seq+uint64(i), event.TypeCitation, fmt.Sprintf(
			`{"label":"[%d]","index":%d,"text":"passage text of %s","source":{"document_title":"%s guide","source_uri":"https://example.com/%s"}}`,
			i+1, i, name, name, name)))
	}
	return out
}

func TestCommentListsOnlyTheSourcesTheAnswerCites(t *testing.T) {
	tests := []struct {
		name   string
		answer string
		want   []string
	}{
		{"one of three", "Beta covers it [2].", []string{"[2] [beta guide](https://example.com/beta)"}},
		{"none", "No source was needed.", nil},
		{"label only in a code block", "Example:\n```\nxs[2]\n```\n", nil},
		{"label in a quotation", `The guide says "it retries [3]".`, []string{"[3] [gamma guide](https://example.com/gamma)"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := github.NewFake()
			c := sinkConnector(t, fake)
			events := append([]event.Envelope{ev(1, event.TypeAccepted, "")}, retrieved(2)...)
			events = append(events, textEvent(5, tc.answer), ev(6, event.TypeCompleted, ""))
			for _, e := range events {
				if err := c.Deliver(context.Background(), target, e); err != nil {
					t.Fatalf("Deliver(seq %d): %v", e.Seq, err)
				}
			}

			comments := fake.Comments()
			if len(comments) != 1 {
				t.Fatalf("github holds %d comments, want 1", len(comments))
			}
			body := comments[0].Body
			if strings.Contains(body, "passage text") {
				t.Errorf("comment quotes passage text:\n%s", body)
			}
			if got := strings.Count(body, "(https://example.com/"); got != len(tc.want) {
				t.Errorf("comment has %d footnote links, want %d:\n%s", got, len(tc.want), body)
			}
			for _, w := range tc.want {
				if !strings.Contains(body, w) {
					t.Errorf("comment lacks %q:\n%s", w, body)
				}
			}
		})
	}
}

func TestFootnoteLinkStaysOneMarkdownLink(t *testing.T) {
	tests := []struct {
		name, uri, want string
	}{
		{"delimiters in the uri", "https://example.com/a b(c)?q=<x>\\y", "[1] [Guide](https://example.com/a%20b%28c%29?q=%3Cx%3E%5Cy)"},
		{"userinfo", "https://user:secret@example.com/a", "[1] [Guide](https://example.com/a)"},
		{"not http", "javascript:alert(1)", "[1] Guide"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := github.NewFake()
			c := sinkConnector(t, fake)
			for _, e := range []event.Envelope{
				ev(1, event.TypeAccepted, ""),
				ev(2, event.TypeCitation, fmt.Sprintf(`{"label":"[1]","source":{"document_title":"Guide","source_uri":%q}}`, tc.uri)),
				textEvent(3, "See [1]."),
				ev(4, event.TypeCompleted, ""),
			} {
				if err := c.Deliver(context.Background(), target, e); err != nil {
					t.Fatalf("Deliver(seq %d): %v", e.Seq, err)
				}
			}
			comments := fake.Comments()
			if len(comments) != 1 {
				t.Fatalf("github holds %d comments, want 1", len(comments))
			}
			body := comments[0].Body
			if !strings.Contains(body, tc.want) {
				t.Errorf("comment lacks %q:\n%s", tc.want, body)
			}
			if strings.Contains(body, "secret") || strings.Contains(body, "javascript") {
				t.Errorf("comment carries a uri it should not link:\n%s", body)
			}
		})
	}
}

func TestFootnoteTitleIsShownAsText(t *testing.T) {
	tests := []struct {
		name, title, want string
		raw               []string
	}{
		{"mention", "by @octocat", "by @\u200docto", []string{"@octocat"}},
		{"image", "![img](https://example.com/i.png)", `\!\[img\]\(https://example.com/i.png\)`, []string{"![img]", "](https://example.com/i.png)"}},
		{"raw html", `<img src="x">`, `\<img src="x"\>`, []string{"<img"}},
		{"autolink", "<https://example.com/x>", `\<https://example.com/x\>`, []string{"<https://example.com/x>"}},
		{"brackets", "a]b[c", `a\]b\[c`, []string{"a]b[c"}},
		{"trailing backslash", `path\`, `path\\`, nil},
		{"backticks", "use `x`", "use \\`x\\`", []string{"`x`"}},
	}
	for _, tc := range tests {
		for _, uri := range []string{"https://example.com/s", "javascript:alert(1)"} {
			t.Run(tc.name+" "+uri[:5], func(t *testing.T) {
				fake := github.NewFake()
				c := sinkConnector(t, fake)
				for _, e := range []event.Envelope{
					ev(1, event.TypeAccepted, ""),
					ev(2, event.TypeCitation, fmt.Sprintf(`{"label":"[1]","source":{"document_title":%q,"source_uri":%q}}`, tc.title, uri)),
					textEvent(3, "See [1]."),
					ev(4, event.TypeCompleted, ""),
				} {
					if err := c.Deliver(context.Background(), target, e); err != nil {
						t.Fatalf("Deliver(seq %d): %v", e.Seq, err)
					}
				}
				comments := fake.Comments()
				if len(comments) != 1 {
					t.Fatalf("github holds %d comments, want 1", len(comments))
				}
				body := comments[0].Body
				if !strings.Contains(body, tc.want) {
					t.Errorf("comment lacks %q:\n%s", tc.want, body)
				}
				for _, r := range tc.raw {
					if unescaped(body, r) {
						t.Errorf("comment carries %q unescaped:\n%s", r, body)
					}
				}
				// The footnote line ends with the title or the link, never
				// with an unescaped backslash that would escape what follows.
				line := body[strings.Index(body, "[1] "):]
				line, _, _ = strings.Cut(line, "\n")
				if strings.HasSuffix(strings.TrimSuffix(line, `\\`), `\`) {
					t.Errorf("footnote ends with a lone backslash: %q", line)
				}
			})
		}
	}
}

// unescaped reports whether body holds r at a position not preceded by a
// backslash.
func unescaped(body, r string) bool {
	for i := 0; ; {
		j := strings.Index(body[i:], r)
		if j < 0 {
			return false
		}
		if at := i + j; at == 0 || body[at-1] != '\\' {
			return true
		}
		i += j + 1
	}
}
