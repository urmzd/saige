package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/cmd/internal/agenthost"
)

// headerWireVersion names the wire version an event stream is written in.
const headerWireVersion = "Saige-Wire-Version"

// mediaTypeEvents is the media type a client can name in Accept, with a v
// parameter, to pick the wire version.
const mediaTypeEvents = "application/vnd.saige.events+json"

// Error codes serve returns with a refused turn body, so a client can tell
// the cases apart without parsing the message.
const (
	codeBadBody         = "bad_body"
	codeVendorFile      = "vendor_file_refused"
	codePartKind        = "part_kind_refused"
	codeURIScheme       = "uri_scheme_refused"
	codeInlineTooLarge  = "inline_too_large"
	codeArtifactMissing = "artifact_not_found"
	codeArtifactsFull   = "artifacts_full"
)

// negotiateWire returns the wire version the client asked for: ?wire= wins
// over Accept, and the default is the current version. It fails with the
// status to answer for a version this server does not write.
func negotiateWire(r *http.Request) (int, int, error) {
	if q := r.URL.Query().Get("wire"); q != "" {
		v, err := strconv.Atoi(q)
		if err != nil || v < 1 || v > types.WireVersion {
			return 0, http.StatusBadRequest, fmt.Errorf("wire must be 1 or %d", types.WireVersion)
		}
		return v, 0, nil
	}
	for _, rng := range strings.Split(r.Header.Get("Accept"), ",") {
		mt, params, err := mime.ParseMediaType(strings.TrimSpace(rng))
		if err != nil || mt != mediaTypeEvents {
			continue
		}
		raw, ok := params["v"]
		if !ok {
			return types.WireVersion, 0, nil
		}
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 || v > types.WireVersion {
			return 0, http.StatusNotAcceptable, fmt.Errorf("wire version %q is not served; this server writes 1 and %d", raw, types.WireVersion)
		}
		return v, 0, nil
	}
	return types.WireVersion, 0, nil
}

// wireWriter writes one client's event stream in its wire version. A
// version 2 stream writes the recorded envelopes as they are. A version 1
// stream keeps a downgrading encoder per tool call path, since each run's
// parts are paired separately.
type wireWriter struct {
	version   int
	maxInline int
	encoders  map[string]*types.Encoder
	last      uint64 // the last event fed through the encoders
}

func newWireWriter(version, maxInline int) *wireWriter {
	return &wireWriter{version: version, maxInline: maxInline, encoders: map[string]*types.Encoder{}}
}

// fed returns the seq after which the stream needs events: the client's
// last event for version 2, and the last event the downgraders saw for
// version 1.
func (w *wireWriter) fed(clientLast uint64) uint64 {
	if w.version == types.WireVersion {
		return clientLast
	}
	return w.last
}

// write writes ev when emit is set; a version 1 stream feeds its
// downgrader either way.
func (w *wireWriter) write(out io.Writer, ev sseEvent, runID string, emit bool) error {
	if w.version == types.WireVersion {
		if !emit {
			return nil
		}
		_, err := fmt.Fprintf(out, "id: %d\nevent: %s\ndata: %s\n\n", ev.seq, ev.kind, ev.data)
		return err
	}
	w.last = ev.seq
	key := strings.Join(ev.path, "/")
	enc := w.encoders[key]
	if enc == nil {
		var err error
		if enc, err = types.NewEncoder(types.EncodeOptions{Version: w.version, MaxInlineBytes: w.maxInline}); err != nil {
			return err
		}
		w.encoders[key] = enc
	}
	// Version 1 has no ref field: stored media is named by its URI.
	d, _ := agenthost.MapSources(ev.delta, func(src types.Source) (types.Source, error) {
		if src.URI == "" && src.Ref != "" {
			src.URI = src.Ref
		}
		return src, nil
	})
	envs, err := enc.Encode(d)
	if err != nil || !emit {
		return err
	}
	for _, env := range envs {
		env.Seq, env.RunID, env.Path = ev.seq, runID, ev.path
		data, err := json.Marshal(env)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(out, "id: %d\nevent: %s\ndata: %s\n\n", ev.seq, env.Kind, data); err != nil {
			return err
		}
	}
	return nil
}

// artifactURL is the download route for a source held in a session's
// artifacts, or "" for any other source.
func artifactURL(sid string, src types.Source) string {
	id, ok := strings.CutPrefix(src.Ref, types.ArtifactScheme)
	if !ok || id == "" {
		return ""
	}
	return "/v1/sessions/" + sid + "/artifacts/" + id
}

// turnMessage reads a turn body: {"parts": [...]} in the shared part codec,
// or the deprecated {"message": "..."}. Every part passes the client
// checks of agenthost.ClientParts. On failure it returns the status and
// error code to answer with.
func (s *server) turnMessage(body io.Reader, store *agenthost.Artifacts) (types.UserMessage, int, string, error) {
	var in struct {
		Message *string           `json:"message"`
		Parts   []json.RawMessage `json:"parts"`
	}
	bad := errors.New(`body must be {"parts": [<part>, ...]} or {"message": "<non-empty text>"}`)
	if err := json.NewDecoder(body).Decode(&in); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return types.UserMessage{}, http.StatusRequestEntityTooLarge, codeInlineTooLarge,
				fmt.Errorf("body over %d bytes; upload media as an artifact and send its ref", tooBig.Limit)
		}
		return types.UserMessage{}, http.StatusBadRequest, codeBadBody, bad
	}
	if in.Message != nil && in.Parts != nil {
		return types.UserMessage{}, http.StatusBadRequest, codeBadBody, errors.New(`send "parts" or "message", not both`)
	}
	var parts []types.UserPart
	switch {
	case in.Parts != nil:
		for i, raw := range in.Parts {
			p, err := types.UnmarshalRolePart[types.UserPart](raw)
			if err != nil {
				return types.UserMessage{}, http.StatusBadRequest, codeBadBody, fmt.Errorf("part %d: %w", i, err)
			}
			parts = append(parts, p)
		}
	case in.Message != nil:
		parts = []types.UserPart{types.Text(*in.Message)}
	default:
		return types.UserMessage{}, http.StatusBadRequest, codeBadBody, bad
	}
	msg, err := agenthost.ClientParts{MaxInline: s.opts.maxInline, Artifacts: store}.UserMessage(parts)
	switch {
	case err == nil:
		return msg, 0, "", nil
	case errors.Is(err, agenthost.ErrVendorFile):
		return msg, http.StatusBadRequest, codeVendorFile, err
	case errors.Is(err, agenthost.ErrPartKind):
		return msg, http.StatusBadRequest, codePartKind, err
	case errors.Is(err, agenthost.ErrURIScheme):
		return msg, http.StatusBadRequest, codeURIScheme, err
	case errors.Is(err, agenthost.ErrInlineTooLarge):
		return msg, http.StatusRequestEntityTooLarge, codeInlineTooLarge, err
	case errors.Is(err, agenthost.ErrArtifactNotFound):
		return msg, http.StatusBadRequest, codeArtifactMissing, err
	case errors.Is(err, agenthost.ErrArtifactsFull):
		return msg, http.StatusInsufficientStorage, codeArtifactsFull, err
	case errors.Is(err, agenthost.ErrEmptyMessage):
		return msg, http.StatusBadRequest, codeBadBody, bad
	default:
		return msg, http.StatusBadRequest, codeBadBody, err
	}
}

// uploadArtifact stores the request body in the session's artifacts and
// returns its ref. The body's Content-Type is the media type, unless
// ?media_type= overrides it (needed for text/plain, which a cross-site form
// could send); ?filename= names it.
func (s *server) uploadArtifact(w http.ResponseWriter, r *http.Request) {
	sess := s.session(w, r)
	if sess == nil {
		return
	}
	raw := r.URL.Query().Get("media_type")
	if raw == "" {
		raw = r.Header.Get("Content-Type")
	}
	mt, _, err := mime.ParseMediaType(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid media type "+strconv.Quote(raw))
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeCodedError(w, http.StatusRequestEntityTooLarge, codeInlineTooLarge, fmt.Sprintf("upload over %d bytes", tooBig.Limit))
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(data) == 0 {
		writeError(w, http.StatusBadRequest, "upload is empty")
		return
	}
	filename := ""
	if f := r.URL.Query().Get("filename"); f != "" {
		filename = path.Base(strings.ReplaceAll(f, "\\", "/"))
	}
	a, err := sess.Artifacts.Put(types.MediaType(mt), filename, data)
	if err != nil {
		writeCodedError(w, http.StatusInsufficientStorage, codeArtifactsFull, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, artifactJSON(sess.ID, a))
}

func artifactJSON(sid string, a agenthost.Artifact) map[string]any {
	out := map[string]any{
		"ref": a.Ref(), "sha256": a.Digest, "size": len(a.Data), "media_type": a.MediaType,
		"url": artifactURL(sid, a.Source()),
	}
	if a.Filename != "" {
		out["filename"] = a.Filename
	}
	return out
}

// getArtifact serves an artifact's bytes. They are always an attachment,
// never rendered in the server's origin.
func (s *server) getArtifact(w http.ResponseWriter, r *http.Request) {
	sess := s.session(w, r)
	if sess == nil {
		return
	}
	a, ok := sess.Artifacts.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "artifact not found")
		return
	}
	h := w.Header()
	h.Set("Content-Type", string(a.MediaType))
	h.Set("Content-Length", strconv.Itoa(len(a.Data)))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox")
	name := a.Filename
	if name == "" {
		name = a.Digest
	}
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	h.Set("ETag", strconv.Quote(a.Digest))
	h.Set("Cache-Control", "private, max-age=31536000, immutable")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(a.Data)
}

func writeCodedError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}
