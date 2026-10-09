package google

import (
	"context"
	"errors"
	"net/http"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/auth/credentials"
	"cloud.google.com/go/auth/httptransport"
	"google.golang.org/genai"
)

// backend selects the Gen AI service and how requests authenticate. The
// adapter and the embedder share it, so both reach Vertex AI the same way.
type backend struct {
	kind        genai.Backend
	project     string
	location    string
	httpClient  *http.Client
	credentials *auth.Credentials
}

// quotaProjectHeader names the project a Google Cloud request is billed and
// rate limited against.
const quotaProjectHeader = "X-Goog-User-Project"

// errVertexTarget reports a Vertex backend without a project or location.
var errVertexTarget = errors.New("google: vertex backend requires both project and location")

// detectCredentials finds Application Default Credentials. Tests replace it.
var detectCredentials = func() (*auth.Credentials, error) {
	return credentials.DetectDefault(&credentials.DetectOptions{
		Scopes: []string{"https://www.googleapis.com/auth/cloud-platform"},
	})
}

// newClient builds the SDK client. The client always gets an HTTP client
// (it carries the Retry-After capture), and the SDK skips its own credential
// lookup when given one, so Vertex credentials are attached here: the ones
// passed with WithCredentials, else Application Default Credentials. A
// caller-supplied HTTP client without credentials is trusted to
// authenticate by itself.
func (b backend) newClient(ctx context.Context, apiKey string) (*genai.Client, error) {
	cc := &genai.ClientConfig{APIKey: apiKey, Backend: b.kind, HTTPClient: withHeaderTransport(b.httpClient)}
	if b.kind == genai.BackendVertexAI {
		if b.project == "" || b.location == "" {
			return nil, errVertexTarget
		}
		cc.APIKey, cc.Project, cc.Location = "", b.project, b.location
		creds := b.credentials
		if creds == nil && b.httpClient == nil {
			var err error
			if creds, err = detectCredentials(); err != nil {
				return nil, errors.Join(errors.New("google: vertex: no Application Default Credentials"), err)
			}
		}
		if creds != nil {
			if err := httptransport.AddAuthorizationMiddleware(cc.HTTPClient, creds); err != nil {
				return nil, err
			}
			cc.Credentials = creds
			// Requests outside a project path, such as the publisher model
			// list, are billed to the quota project only when it is named in
			// a header; without it they fall to the OAuth client's project,
			// which usually has Vertex AI disabled.
			if qp, err := creds.QuotaProjectID(ctx); err == nil && qp != "" {
				cc.HTTPOptions.Headers = http.Header{quotaProjectHeader: {qp}}
			}
		}
	}
	return genai.NewClient(ctx, cc)
}
