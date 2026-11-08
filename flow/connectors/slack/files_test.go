package slack_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/attachment"
	"github.com/urmzd/saige/flow/attachment/intake"
	"github.com/urmzd/saige/flow/blob"
	"github.com/urmzd/saige/flow/connectors"
	"github.com/urmzd/saige/flow/connectors/slack"
	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/thread"
)

const (
	pngBytes = "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00"
	m4aBytes = "\x00\x00\x00\x1cftypM4A \x00\x00\x00\x00M4A mp42isom\x00\x00\x00\x08free"
	signIn   = "<!DOCTYPE html><html><head><title>Slack</title></head><body>Sign in to your workspace</body></html>"
)

// fileServer serves the bodies by path and counts requests.
type fileServer struct {
	*httptest.Server
	hits atomic.Int64
}

func serveFiles(t *testing.T, bodies map[string]string) *fileServer {
	t.Helper()
	fs := &fileServer{}
	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fs.hits.Add(1)
		body, ok := bodies[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(fs.Close)
	return fs
}

func loopbackOnly(a netip.Addr) error {
	if a.Unmap() == netip.MustParseAddr("127.0.0.1") {
		return nil
	}
	return intake.PublicOnly(a)
}

// startWithFiles is start with file intake on, fetching from loopback.
func startWithFiles(t *testing.T, store blob.Store, api *slack.Fake) (*slack.Connector, <-chan event.Envelope) {
	t.Helper()
	c, tags := start(t, slack.Config{
		API:       api,
		Bots:      map[string]string{"U08BOTID": "docs-bot"},
		Tenant:    "acme",
		Files:     connectors.Files{Store: store, Caps: intake.Caps{MaxFileBytes: 1 << 10}},
		FileToken: "xoxb-test",
	})
	slack.UseFetcher(c, intake.NewFetcher(store, intake.Hosts("127.0.0.1"),
		intake.WithPlainHTTP(), intake.WithNetworkPolicy(loopbackOnly)))
	return c, tags
}

// mentionWithFiles is an app_mention as Slack delivers one with files: the
// file objects ride on the event, in the order they were attached.
func mentionWithFiles(base string) string {
	return fmt.Sprintf(`{
  "team_id": "T01ABCDEF",
  "api_app_id": "A08APPID",
  "type": "event_callback",
  "event_id": "Ev08FILES",
  "event_time": 1700000090,
  "event": {
    "type": "app_mention",
    "user": "U0HUMAN1",
    "text": "<@U08BOTID> what is wrong here?",
    "ts": "1700000090.001200",
    "thread_ts": "1700000000.000100",
    "channel": "C02CHANNEL",
    "event_ts": "1700000090.001200",
    "files": [
      {"id": "F01SHOT", "name": "dashboard.png", "title": "the failing dashboard", "mimetype": "image/png", "filetype": "png",
       "size": 33, "mode": "hosted", "url_private": "%[1]s/files-pri/T01-F01SHOT/dashboard.png",
       "url_private_download": "%[1]s/files-pri/T01-F01SHOT/download/dashboard.png"},
      {"id": "F02CLIP", "name": "audio_message.m4a", "title": "audio_message.m4a", "mimetype": "", "filetype": "",
       "subtype": "slack_audio", "size": 44, "url_private": "%[1]s/files-pri/T01-F02CLIP/audio_message.m4a"},
      {"id": "F03DENY", "name": "notes.pdf", "title": "notes.pdf", "mimetype": "application/pdf", "filetype": "pdf",
       "size": 99, "url_private_download": "%[1]s/files-pri/T01-F03DENY/download/notes.pdf"},
      {"id": "F04STUB", "mode": "file_access", "file_access": "check_file_info", "created": 0, "user": ""},
      {"id": "F05HUGE", "name": "recording.mov", "title": "recording.mov", "mimetype": "video/quicktime", "size": 900000000,
       "url_private_download": "%[1]s/files-pri/T01-F05HUGE/download/recording.mov"}
    ]
  }
}`, base)
}

// Files on the message become its attachments: stored
// under the event's tenant, in the order Slack listed them, with the thread as
// their origin, and a voice clip as audio. A file that could not be had (a
// sign-in page served in its place, over the size cap) does not stop the event:
// it is carried as unread, with the reason the agent will be told.
func TestFilesOnAMentionBecomeTheTagsAttachmentsInOrder(t *testing.T) {
	store := blob.NewMemory()
	api := slack.NewFake()
	srv := serveFiles(t, map[string]string{
		"/files-pri/T01-F01SHOT/download/dashboard.png": pngBytes,
		"/files-pri/T01-F02CLIP/audio_message.m4a":      m4aBytes,
		"/files-pri/T01-F03DENY/download/notes.pdf":     signIn,
		"/files-pri/T01-F04STUB/download/trace.txt":     "panic: nil map\n",
	})
	api.AddFile(slack.File{ID: "F04STUB", Name: "trace.txt", Mimetype: "text/plain", Filetype: "text", Size: 15,
		URLPrivateDownload: srv.URL + "/files-pri/T01-F04STUB/download/trace.txt"})
	c, tags := startWithFiles(t, store, api)

	w := httptest.NewRecorder()
	c.ServeHTTP(w, post(t, mentionWithFiles(srv.URL)))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	tag := <-tags
	if tag.Tenant != "acme" {
		t.Errorf("event tenant = %q, want the connector's", tag.Tenant)
	}
	files := posted(t, tag).Attachments
	if len(files) != 5 {
		t.Fatalf("attachments = %+v, want one per file", files)
	}
	want := []struct{ name, mediaType, unread string }{
		{"dashboard.png", "image/png", ""},
		{"audio_message.m4a", "audio/mp4", ""},
		{"notes.pdf", "", "the surface refused access to it"},
		{"trace.txt", "text/plain", ""},
		{"recording.mov", "", "it is 858.3 MB, over the 1.0 KB limit for one file"},
	}
	for i, a := range files {
		if a.Name != want[i].name || a.MediaType != want[i].mediaType || a.Unread != want[i].unread {
			t.Errorf("attachment %d = %+v, want %+v", i, a, want[i])
		}
		if a.Origin.String() != tag.Channel {
			t.Errorf("attachment %d origin %s, want the event's channel %s", i, a.Origin, tag.Channel)
		}
		if a.Unread == "" {
			if _, err := store.Stat(context.Background(), "acme", blob.Digest(a.Digest)); err != nil {
				t.Errorf("attachment %d is not stored under the tag's tenant: %v", i, err)
			}
		}
	}
	if files[0].Alt != "the failing dashboard" {
		t.Errorf("the file title was not kept as alt text: %+v", files[0])
	}
	if got := files[1].Kind(); got != attachment.KindAudio {
		t.Errorf("a voice clip routed as %s, want audio", got)
	}
	if n := srv.hits.Load(); n != 4 {
		t.Errorf("the file host was asked %d times, want 4: an oversized file is refused before it is requested", n)
	}
}

// Without file intake the event is what it always was.
func TestWithoutFileIntakeAMentionCarriesNoAttachments(t *testing.T) {
	srv := serveFiles(t, map[string]string{})
	c, tags := start(t, slack.Config{Bots: map[string]string{"U08BOTID": "docs-bot"}})
	w := httptest.NewRecorder()
	c.ServeHTTP(w, post(t, mentionWithFiles(srv.URL)))
	if tag := <-tags; posted(t, tag).Attachments != nil || tag.Tenant != "" {
		t.Fatalf("event = %+v, want no attachments and no tenant", tag)
	}
	if n := srv.hits.Load(); n != 0 {
		t.Errorf("files were fetched %d times with intake off", n)
	}
}

// A file shared in a direct message is a request too.
func TestAFileSharedInADirectMessageIsPublishedWithIt(t *testing.T) {
	store := blob.NewMemory()
	srv := serveFiles(t, map[string]string{"/f/shot.png": pngBytes})
	c, tags := start(t, slack.Config{Tenant: "acme", FileToken: "xoxb-test",
		Files: connectors.Files{Store: store}})
	slack.UseFetcher(c, intake.NewFetcher(store, intake.Hosts("127.0.0.1"), intake.WithPlainHTTP(), intake.WithNetworkPolicy(loopbackOnly)))
	body := fmt.Sprintf(`{"team_id":"T01ABCDEF","type":"event_callback","event_id":"Ev09DM","event":{
	  "type":"message","subtype":"file_share","channel_type":"im","user":"U0HUMAN1","channel":"D01DM",
	  "text":"what is this?","ts":"1700000100.000100",
	  "files":[{"id":"F1","name":"shot.png","mimetype":"image/png","url_private_download":"%s/f/shot.png"}]}}`, srv.URL)
	w := httptest.NewRecorder()
	c.ServeHTTP(w, post(t, body))
	files := posted(t, <-tags).Attachments
	if len(files) != 1 || files[0].MediaType != "image/png" {
		t.Fatalf("attachments = %+v", files)
	}
}

// The bot token goes to Slack's file host over HTTPS and nowhere else: not to
// a look-alike host, not in the clear, not to wherever a download redirects.
func TestTheBotTokenIsSentOnlyToSlacksFileHost(t *testing.T) {
	headers := slack.FileHeaders("xoxb-secret")
	cases := map[string]bool{
		"https://files.slack.com/files-pri/T1-F1/download/a.png": true,
		"https://FILES.slack.com/files-pri/T1-F1/a.png":          true,
		"http://files.slack.com/files-pri/T1-F1/a.png":           false,
		"https://files.slack.com.evil.example/a.png":             false,
		"https://slack.com/api/files.info":                       false,
		"https://slack-files.s3.amazonaws.com/a.png":             false,
	}
	for raw, want := range cases {
		u, _ := url.Parse(raw)
		got := headers(u).Get("Authorization") == "Bearer xoxb-secret"
		if got != want {
			t.Errorf("%s: token sent = %v, want %v", raw, got, want)
		}
	}
	if slack.FileHeaders("")(&url.URL{Scheme: "https", Host: "files.slack.com"}) != nil {
		t.Error("an empty token produced a header")
	}
}

// Downloads are fetched from Slack's file host only, and a redirect anywhere
// else is refused before anything is sent to it.
func TestFileDownloadsAreConfinedToSlacksFileHost(t *testing.T) {
	allow := slack.FileHosts()
	for raw, want := range map[string]bool{
		"https://files.slack.com/files-pri/T1-F1/a.png": true,
		"https://files.slack.com.evil.example/a.png":    false,
		"https://evil.example/files.slack.com/a.png":    false,
		"https://slack.com/a.png":                       false,
	} {
		u, _ := url.Parse(raw)
		if got := allow(u) == nil; got != want {
			t.Errorf("%s: allowed = %v, want %v", raw, got, want)
		}
	}

	var followed atomic.Bool
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example/stolen.png", http.StatusFound)
	}))
	t.Cleanup(redirect.Close)
	f := intake.NewFetcher(blob.NewMemory(), func(u *url.URL) error {
		if u.Hostname() == "127.0.0.1" {
			return nil // the test's stand-in for files.slack.com
		}
		if u.Hostname() == "evil.example" {
			followed.Store(true)
		}
		return allow(u)
	}, intake.WithPlainHTTP(), intake.WithNetworkPolicy(loopbackOnly), intake.WithTimeout(5*time.Second))
	_, err := f.Fetch(context.Background(), "acme", redirect.URL+"/files-pri/x.png", intake.Meta{})
	if !errors.Is(err, intake.ErrNotAllowed) {
		t.Fatalf("redirect to a host off the allowlist = %v, want ErrNotAllowed", err)
	}
	if !followed.Load() {
		t.Error("the redirect target was never checked against the policy")
	}
	if strings.Contains(err.Error(), "stolen.png") {
		t.Errorf("the refusal leaks the redirect url: %v", err)
	}
}

// Recorded conversations.replies pages: the parent, a reply with a file, the
// app's own answer, and a second page.
func repliesServer(t *testing.T, files string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/conversations.replies" {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusBadRequest)
			return
		}
		if r.Header.Get("Authorization") != "Bearer xoxb-test" || r.URL.Query().Get("channel") != "C02CHANNEL" || r.URL.Query().Get("ts") != "1700000000.000100" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("cursor") == "" {
			_, _ = fmt.Fprintf(w, `{"ok":true,"has_more":true,"messages":[
			  {"type":"message","user":"U0HUMAN1","text":"the deploy failed &amp; <@U0HUMAN2> saw it","ts":"1700000000.000100","thread_ts":"1700000000.000100","reply_count":3},
			  {"type":"message","user":"U0HUMAN2","text":"here is the log","ts":"1700000010.000200","thread_ts":"1700000000.000100",
			   "files":[{"id":"F9","name":"deploy.log","mimetype":"text/plain","size":12,"url_private_download":"%s/f/deploy.log"}]},
			  {"type":"message","subtype":"bot_message","bot_id":"B08BOT","app_id":"A08APPID","text":"Rolling back.","ts":"1700000020.000300","thread_ts":"1700000000.000100"}
			],"response_metadata":{"next_cursor":"bmV4dA=="}}`, files)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true,"has_more":false,"messages":[
		  {"type":"message","user":"U08BOTID","text":"(posted as the agent's bot user)","ts":"1700000030.000400","thread_ts":"1700000000.000100"},
		  {"type":"message","bot_id":"B0OTHER","app_id":"A0OTHER","username":"ci","text":"build 812 failed","ts":"1700000040.000500","thread_ts":"1700000000.000100"}
		],"response_metadata":{"next_cursor":""}}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// The connector lists a thread for reconciliation: every page, oldest first,
// the timestamp as the stable id, the app's own posts marked Own, Slack markup
// decoded, and a message's files brought in under the tenant only when asked.
func TestTheSlackThreadIsListedFromConversationsReplies(t *testing.T) {
	files := serveFiles(t, map[string]string{"/f/deploy.log": "exit code 2\n"})
	srv, calls := repliesServer(t, files.URL)
	store := blob.NewMemory()
	c, err := slack.New(slack.Config{
		API:       slack.NewHTTPAPIAt(srv.URL, "xoxb-test", srv.Client()),
		Team:      "T01ABCDEF",
		Bots:      map[string]string{"U08BOTID": "docs-bot"},
		AppID:     "A08APPID",
		Tenant:    "acme",
		FileToken: "xoxb-test",
		Files:     connectors.Files{Store: store},
	})
	if err != nil {
		t.Fatal(err)
	}
	slack.UseFetcher(c, intake.NewFetcher(store, intake.Hosts("127.0.0.1"), intake.WithPlainHTTP(), intake.WithNetworkPolicy(loopbackOnly)))
	surface := address.MustParse("slack://T01ABCDEF/C02CHANNEL?thread=1700000000.000100")

	msgs, err := c.Messages(context.Background(), "acme", surface)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("conversations.replies called %d times, want both pages", calls.Load())
	}
	type row struct {
		id, text, actor string
		own, files      bool
	}
	var got []row
	for _, m := range msgs {
		got = append(got, row{m.ID, m.Text, m.Actor.ID, m.Own, m.Files != nil})
	}
	want := []row{
		{"1700000000.000100", "the deploy failed & @U0HUMAN2 saw it", "U0HUMAN1", false, false},
		{"1700000010.000200", "here is the log", "U0HUMAN2", false, true},
		{"1700000020.000300", "Rolling back.", "B08BOT", true, false},
		{"1700000030.000400", "(posted as the agent's bot user)", "U08BOTID", true, false},
		{"1700000040.000500", "build 812 failed", "B0OTHER", false, false},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("listed\n %v\nwant\n %v", got, want)
	}
	if files.hits.Load() != 0 {
		t.Error("listing a thread downloaded its files before the journal asked")
	}
	atts, err := msgs[1].Files(context.Background())
	if err != nil || len(atts) != 1 || atts[0].Name != "deploy.log" || atts[0].Origin.String() != thread.Surface(surface) {
		t.Fatalf("files of the reply = %+v, %v", atts, err)
	}
	if _, err := store.Stat(context.Background(), "acme", blob.Digest(atts[0].Digest)); err != nil {
		t.Errorf("the reply's file is not stored under the tenant: %v", err)
	}

	if _, err := c.Messages(context.Background(), "globex", surface); err == nil {
		t.Error("another tenant's listing was answered with this connector's token")
	}
	if _, err := c.Messages(context.Background(), "acme", address.MustParse("slack://T09OTHER/C02CHANNEL?thread=1700000000.000100")); err == nil {
		t.Error("a thread in another workspace was listed")
	}
	if _, err := c.Messages(context.Background(), "acme", address.MustParse("slack://T01ABCDEF/C02CHANNEL")); err == nil {
		t.Error("an address with no thread was listed")
	}
}

// files.info is a GET with query parameters, and a refusal reads as one.
func TestFileInfoReadsTheWholeFileObject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/files.info" {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		if r.URL.Query().Get("file") == "F1" {
			_, _ = io.WriteString(w, `{"ok":true,"file":{"id":"F1","name":"a.png","mimetype":"image/png","url_private_download":"https://files.slack.com/files-pri/T1-F1/download/a.png"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":false,"error":"access_denied"}`)
	}))
	t.Cleanup(srv.Close)
	api := slack.NewHTTPAPIAt(srv.URL, "xoxb-test", srv.Client())
	f, err := api.FileInfo(context.Background(), "T1", "F1")
	if err != nil || f.Name != "a.png" || f.URLPrivateDownload == "" {
		t.Fatalf("FileInfo = %+v, %v", f, err)
	}
	if _, err := api.FileInfo(context.Background(), "T1", "F2"); err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("FileInfo of a refused file = %v", err)
	}
}
