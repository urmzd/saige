package slack

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/urmzd/saige/flow/attachment"
	"github.com/urmzd/saige/flow/attachment/intake"
)

// FileHost is the only host Slack serves file downloads from
// (url_private_download), and the only host the bot token is sent to when
// fetching one.
const FileHost = "files.slack.com"

// File is a Slack file object, as an event or files.info carries it. Only
// what intake needs is decoded.
type File struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Title    string `json:"title"`
	Mimetype string `json:"mimetype"`
	Filetype string `json:"filetype"`
	// Subtype is "slack_audio" for a voice clip and "slack_video" for a
	// video clip.
	Subtype string `json:"subtype"`
	Size    int64  `json:"size"`
	// URLPrivateDownload is preferred; Slack documents it only for files in
	// an editable mode, so URLPrivate is the fallback.
	URLPrivate         string `json:"url_private"`
	URLPrivateDownload string `json:"url_private_download"`
	// FileAccess is "check_file_info" when the event carries only a stub of
	// the file (Slack Connect does this) and files.info must be asked for
	// the rest.
	FileAccess string `json:"file_access"`
}

// FileAPI is the optional capability to read a file object in full. HTTPAPI
// and Fake implement it. Without it a stub file (FileAccess
// "check_file_info") is carried as unread.
type FileAPI interface {
	FileInfo(ctx context.Context, workspace, id string) (File, error)
}

// fileHosts is the host policy for file downloads: exactly FileHost. A
// redirect anywhere else is refused, and the file is carried as unread.
func fileHosts() intake.Allow { return intake.Hosts(FileHost) }

// fileHeaders returns the per-hop credential policy: the bot token, over
// HTTPS, to FileHost only.
func fileHeaders(token string) func(*url.URL) http.Header {
	return func(u *url.URL) http.Header {
		if token == "" || u.Scheme != "https" || !strings.EqualFold(u.Hostname(), FileHost) {
			return nil
		}
		return http.Header{"Authorization": {"Bearer " + token}}
	}
}

func fileRequestHeaders(fixed string, source func(context.Context) (string, error)) func(context.Context, *url.URL) (http.Header, error) {
	return func(ctx context.Context, u *url.URL) (http.Header, error) {
		if u.Scheme != "https" || !strings.EqualFold(u.Host, FileHost) {
			return nil, nil
		}
		token := fixed
		if source != nil {
			var err error
			token, err = source(ctx)
			if err != nil || token == "" {
				return nil, errors.New("slack: file authorization unavailable")
			}
		}
		return fileHeaders(token)(u), nil
	}
}

// candidates turns the files on a message into intake candidates, in the
// order Slack listed them.
func (c *Connector) candidates(workspace string, files []File) []intake.Candidate {
	if c.fetcher == nil || len(files) == 0 {
		return nil
	}
	out := make([]intake.Candidate, 0, len(files))
	for _, f := range files {
		name := firstNonEmpty(f.Name, f.Title, f.ID)
		alt := ""
		if f.Title != "" && f.Title != f.Name {
			alt = f.Title
		}
		out = append(out, intake.Candidate{Name: name, Alt: alt, Size: f.Size, Fetch: c.fetchFile(workspace, f)})
	}
	return out
}

// fetchFile is how one Slack file is brought in.
func (c *Connector) fetchFile(workspace string, f File) func(context.Context, string, intake.Meta) (attachment.Attachment, error) {
	return func(ctx context.Context, tenant string, meta intake.Meta) (attachment.Attachment, error) {
		if f.FileAccess == "check_file_info" {
			api, ok := c.cfg.API.(FileAPI)
			if !ok {
				return attachment.Attachment{}, fmt.Errorf("slack: file %s is a stub and this API cannot read files.info", f.ID)
			}
			full, err := api.FileInfo(ctx, workspace, f.ID)
			if err != nil {
				if errorCode(err) == "access_denied" || errorCode(err) == "missing_scope" {
					return attachment.Attachment{}, fmt.Errorf("slack: files.info %s: %w: %s", f.ID, intake.ErrRefused, errorCode(err))
				}
				return attachment.Attachment{}, fmt.Errorf("slack: files.info %s: %w", f.ID, err)
			}
			f = full
			// The stub had no name to offer; the full object does.
			meta.Name = firstNonEmpty(f.Name, f.Title, meta.Name)
			if f.Title != "" && f.Title != f.Name {
				meta.Alt = f.Title
			}
		}
		link := firstNonEmpty(f.URLPrivateDownload, f.URLPrivate)
		if link == "" {
			return attachment.Attachment{}, fmt.Errorf("slack: file %s has no download url", f.ID)
		}
		att, err := c.fetcher.Fetch(ctx, tenant, link, meta)
		if status := (*intake.StatusError)(nil); errors.As(err, &status) && status.Refused() {
			return attachment.Attachment{}, fmt.Errorf("slack: %w: %w", intake.ErrRefused, err)
		}
		if err != nil {
			return attachment.Attachment{}, err
		}
		if signInPage(att, f) {
			// Slack answers a download it will not authorize (no token, a
			// token without files:read) with 200 and its sign-in page. The
			// page is not the file, and must not reach the agent as if it
			// were.
			return attachment.Attachment{}, fmt.Errorf("slack: %w: file %s came back as an HTML page; the token may lack the files:read scope", intake.ErrRefused, f.ID)
		}
		return att, nil
	}
}

// signInPage reports an HTML body where Slack declared some other file. An
// HTML file someone actually shared is declared as one, by its mimetype or
// its filetype, and is kept.
func signInPage(att attachment.Attachment, f File) bool {
	if att.MediaType != "text/html" {
		return false
	}
	declared := strings.ToLower(f.Mimetype)
	return !strings.HasPrefix(declared, "text/html") && !strings.EqualFold(f.Filetype, "html")
}
