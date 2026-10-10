package google

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/auth/httptransport"
	"google.golang.org/genai"

	"github.com/urmzd/saige/agent/batch"
	"github.com/urmzd/saige/agent/provider/internal/streamcheck"
	"github.com/urmzd/saige/agent/types"
)

// Batch mode limits, from the Gemini API documentation.
const (
	// inlineMaxBytes is the inline request limit (20 MB), with headroom
	// for the envelope the SDK adds.
	inlineMaxBytes = 19 << 20
	// batchKey is the metadata key (Gemini API) and request label (Vertex
	// AI) that carries a request's custom ID.
	batchKey = "saige_key"
	// metaCount names the handle field that records the request count.
	metaCount = "count"
	// maxScan bounds the jobs FindBatch lists.
	maxScan = 1000
)

var (
	_ types.BatchProvider = (*Adapter)(nil)
	_ types.BatchFinder   = (*Adapter)(nil)
	_ types.BatchProvider = (*VertexBatch)(nil)
	_ types.BatchFinder   = (*VertexBatch)(nil)
)

// errVertexBatch reports a batch submitted to the adapter on Vertex AI.
const errVertexBatch = "Vertex AI batch prediction reads its input from and writes its output to Cloud Storage; use NewVertexBatch with a bucket"

// Submit implements types.BatchProvider with Gemini API batch mode. The
// requests are sent inline, so a batch is limited to about 20 MB; each is
// validated and encoded as the streaming entry points do. On Vertex AI the
// adapter refuses: batch prediction there needs Cloud Storage, which
// NewVertexBatch provides.
func (a *Adapter) Submit(ctx context.Context, reqs []types.BatchRequest, opts types.BatchSubmitOptions) (types.BatchHandle, error) {
	caps := a.Capabilities()
	if a.backend.kind == genai.BackendVertexAI {
		return types.BatchHandle{}, caps.OptionError("batch", errVertexBatch)
	}
	if err := batch.CheckIDs(reqs); err != nil {
		return types.BatchHandle{}, err
	}
	inline := make([]*genai.InlinedRequest, len(reqs))
	size := 0
	for i, r := range reqs {
		contents, config, err := a.batchRequest(r)
		if err != nil {
			return types.BatchHandle{}, fmt.Errorf("request %s: %w", r.CustomID, err)
		}
		inline[i] = &genai.InlinedRequest{Contents: contents, Config: config, Metadata: map[string]string{batchKey: r.CustomID}}
		raw, err := json.Marshal(inline[i])
		if err != nil {
			return types.BatchHandle{}, fmt.Errorf("request %s: encode: %w", r.CustomID, err)
		}
		size += len(raw)
	}
	if size > inlineMaxBytes {
		return types.BatchHandle{}, caps.OptionError("batch", "inline batch requests are limited to 20 MB")
	}
	job, err := a.client.Batches.Create(ctx, a.model, &genai.BatchJobSource{InlinedRequests: inline},
		&genai.CreateBatchJobConfig{DisplayName: opts.Tag})
	if err != nil {
		return types.BatchHandle{}, classifyGoogleError(a.model, err, true)
	}
	return types.BatchHandle{Provider: providerName, Model: a.model, ID: job.Name,
		Meta: map[string]string{metaCount: strconv.Itoa(len(reqs))}}, nil
}

// batchRequest validates one request and builds its contents and config, as
// Stream with options and Stream with a schema do.
func (a *Adapter) batchRequest(r types.BatchRequest) ([]*genai.Content, *genai.GenerateContentConfig, error) {
	c, err := a.withRequestOptions(r.Options.Raw())
	if err != nil {
		return nil, nil, err
	}
	if c, err = c.compileDials(r.Options, r.Tools, r.Schema != nil); err != nil {
		return nil, nil, err
	}
	if err := c.Capabilities().ValidateRequest(r.Tools, r.Schema != nil); err != nil {
		return nil, nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, nil, err
	}
	if err := c.checkToolChoice(r.Tools); err != nil {
		return nil, nil, err
	}
	contents, config, err := c.cachedRequest(r.Messages, r.Tools)
	if err != nil {
		return nil, nil, err
	}
	if r.Schema != nil {
		config.ResponseMIMEType = string(types.MediaJSON)
		config.ResponseSchema = parameterSchemaToGemini(*r.Schema)
	}
	return contents, config, nil
}

// Status implements types.BatchProvider.
func (a *Adapter) Status(ctx context.Context, h types.BatchHandle) (types.BatchStatus, error) {
	job, err := a.client.Batches.Get(ctx, h.ID, nil)
	if err != nil {
		return types.BatchStatus{}, batchError(a.model, err)
	}
	return jobStatus(job, h), nil
}

func jobStatus(job *genai.BatchJob, h types.BatchHandle) types.BatchStatus {
	st := types.BatchStatus{VendorState: string(job.State), CreatedAt: job.CreateTime, EndedAt: job.EndTime}
	st.Counts.Total, _ = strconv.Atoi(h.Meta[metaCount])
	if cs := job.CompletionStats; cs != nil {
		st.Counts.Succeeded, st.Counts.Errored = int(cs.SuccessfulCount), int(cs.FailedCount)
		st.Counts.Processing = int(cs.IncompleteCount)
	}
	switch job.State {
	case genai.JobStateQueued, genai.JobStatePending, genai.JobStateUnspecified:
		st.State = types.BatchPending
	case genai.JobStateCancelling:
		st.State = types.BatchCanceling
	case genai.JobStateSucceeded, genai.JobStatePartiallySucceeded:
		st.State = types.BatchEnded
	case genai.JobStateCancelled:
		st.State = types.BatchCanceled
	case genai.JobStateExpired:
		st.State = types.BatchExpired
	case genai.JobStateFailed:
		st.State = types.BatchFailed
		if job.Error != nil {
			st.Error = job.Error.Message
		}
	default:
		st.State = types.BatchRunning
	}
	return st
}

// Results implements types.BatchProvider. Inline responses come back in
// request order with their metadata; a file destination is downloaded and
// read line by line.
func (a *Adapter) Results(ctx context.Context, h types.BatchHandle) iter.Seq2[types.BatchResult, error] {
	return func(yield func(types.BatchResult, error) bool) {
		job, err := a.client.Batches.Get(ctx, h.ID, nil)
		if err != nil {
			yield(types.BatchResult{}, batchError(a.model, err))
			return
		}
		outcome := unfinishedOutcome(job.State)
		if job.Dest == nil {
			return
		}
		for _, r := range job.Dest.InlinedResponses {
			res := types.BatchResult{CustomID: r.Metadata[batchKey]}
			fillResult(&res, a.model, r.Response, r.Error, outcome)
			if !yield(res, nil) {
				return
			}
		}
		if job.Dest.FileName == "" {
			return
		}
		raw, err := a.client.Files.Download(ctx, genai.NewDownloadURIFromFile(&genai.File{Name: job.Dest.FileName}), nil)
		if err != nil {
			yield(types.BatchResult{}, batchError(a.model, err))
			return
		}
		readLines(bytes.NewReader(raw), func(line []byte) bool {
			var l struct {
				Key      string                         `json:"key"`
				Response *genai.GenerateContentResponse `json:"response"`
				Error    *genai.JobError                `json:"error"`
			}
			if err := json.Unmarshal(line, &l); err != nil {
				yield(types.BatchResult{}, fmt.Errorf("google: decode batch result: %w", err))
				return false
			}
			res := types.BatchResult{CustomID: l.Key}
			fillResult(&res, a.model, l.Response, l.Error, outcome)
			return yield(res, nil)
		}, func(err error) { yield(types.BatchResult{}, err) })
	}
}

// unfinishedOutcome is the outcome of a request without a response in a
// batch that ended in state s.
func unfinishedOutcome(s genai.JobState) types.BatchOutcome {
	switch s {
	case genai.JobStateExpired:
		return types.BatchExpiredOutcome
	case genai.JobStateCancelled:
		return types.BatchCanceledOutcome
	}
	return types.BatchErrored
}

// fillResult converts one response, or one error, into res.
func fillResult(res *types.BatchResult, model string, resp *genai.GenerateContentResponse, jobErr *genai.JobError, unfinished types.BatchOutcome) {
	switch {
	case jobErr != nil:
		res.Outcome = types.BatchErrored
		code := ""
		if jobErr.Code != nil {
			code = strconv.Itoa(int(*jobErr.Code))
		}
		res.Err = &types.BatchRequestError{Outcome: res.Outcome, Code: code, Message: jobErr.Message}
	case resp == nil:
		res.Outcome = unfinished
		res.Err = &types.BatchRequestError{Outcome: unfinished}
	default:
		res.Outcome = types.BatchSucceeded
		res.Message, res.FinishReason, res.Usage = responseMessage(resp)
		switch {
		case resp.PromptFeedback != nil && resp.PromptFeedback.BlockReason != "":
			res.Err = promptBlockedError(model, resp.PromptFeedback)
		case types.IsContentFilterFinishReason(res.FinishReason):
			res.Err = streamcheck.Refused(providerName, model, res.FinishReason)
		case types.IsTruncationFinishReason(res.FinishReason):
			res.Err = &types.ProviderError{Provider: providerName, Model: model, Kind: types.ErrorKindTruncated,
				Err: fmt.Errorf("%w: stopped at %s", types.ErrResponseTruncated, res.FinishReason)}
		}
	}
}

// responseMessage converts a complete response as the streaming path does.
func responseMessage(resp *genai.GenerateContentResponse) (types.AssistantMessage, string, types.UsageDelta) {
	var msg types.AssistantMessage
	finish := ""
	if len(resp.Candidates) > 0 {
		cand := resp.Candidates[0]
		finish = string(cand.FinishReason)
		if cand.Content != nil {
			for _, part := range cand.Content.Parts {
				switch {
				case part.Text != "" && part.Thought:
					sig := ""
					if len(part.ThoughtSignature) > 0 {
						sig = base64.StdEncoding.EncodeToString(part.ThoughtSignature)
					}
					msg.Parts = append(msg.Parts, types.ThinkingPart{Text: part.Text, Signature: sig})
				case part.Text != "":
					msg.Parts = append(msg.Parts, types.TextPart{Text: part.Text})
				case part.FunctionCall != nil:
					if len(part.ThoughtSignature) > 0 {
						msg.Parts = append(msg.Parts, types.ThinkingPart{Signature: base64.StdEncoding.EncodeToString(part.ThoughtSignature)})
					}
					id := part.FunctionCall.ID
					if id == "" {
						id = types.NewID()
					}
					args := part.FunctionCall.Args
					if args == nil {
						args = map[string]any{}
					}
					msg.Parts = append(msg.Parts, types.ToolCallPart{ID: id, Name: part.FunctionCall.Name, Arguments: args})
				}
			}
		}
	}
	var usage types.UsageDelta
	if u := resp.UsageMetadata; u != nil {
		usage = types.UsageDelta{Cumulative: true, PromptTokens: int(u.PromptTokenCount),
			CachedPromptTokens: int(u.CachedContentTokenCount),
			CompletionTokens:   int(u.CandidatesTokenCount + u.ThoughtsTokenCount),
			TotalTokens:        int(u.TotalTokenCount), ResponseModel: resp.ModelVersion, ResponseID: resp.ResponseID}
	}
	if finish != "" {
		usage.FinishReasons = []string{finish}
	}
	return msg, finish, usage
}

// Cancel implements types.BatchProvider.
func (a *Adapter) Cancel(ctx context.Context, h types.BatchHandle) error {
	if err := a.client.Batches.Cancel(ctx, h.ID, nil); err != nil {
		return batchError(a.model, err)
	}
	return nil
}

// FindBatch implements types.BatchFinder by matching the tag, which Submit
// sends as the job's display name.
func (a *Adapter) FindBatch(ctx context.Context, q types.BatchQuery) (types.BatchHandle, bool, error) {
	return findJob(ctx, a.client, a.model, q)
}

func findJob(ctx context.Context, client *genai.Client, model string, q types.BatchQuery) (types.BatchHandle, bool, error) {
	if q.Tag == "" {
		return types.BatchHandle{}, false, fmt.Errorf("%w: lookup needs a tag", types.ErrBatchAmbiguous)
	}
	n := 0
	for job, err := range client.Batches.All(ctx) {
		if err != nil {
			return types.BatchHandle{}, false, batchError(model, err)
		}
		if n++; n > maxScan {
			return types.BatchHandle{}, false, fmt.Errorf("%w: more than %d jobs to search", types.ErrBatchAmbiguous, maxScan)
		}
		if job.DisplayName == q.Tag && !job.CreateTime.Before(q.Since) {
			return types.BatchHandle{Provider: providerName, Model: model, ID: job.Name,
				Meta: map[string]string{metaCount: strconv.Itoa(q.Count)}}, true, nil
		}
	}
	return types.BatchHandle{}, false, nil
}

// batchError classifies an error from a batch endpoint. A 404 means the job
// is unknown.
func batchError(model string, err error) error {
	var apiErr genai.APIError
	if errors.As(err, &apiErr) && apiErr.Code == http.StatusNotFound {
		return fmt.Errorf("%w: %w", types.ErrBatchNotFound, err)
	}
	return classifyGoogleError(model, err, true)
}

// readLines calls fn for each non-empty JSONL line until it returns false.
func readLines(r io.Reader, fn func([]byte) bool, onErr func(error)) bool {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		if !fn(line) {
			return false
		}
	}
	if err := sc.Err(); err != nil {
		onErr(fmt.Errorf("google: read batch results: %w", err))
		return false
	}
	return true
}

// ── Vertex AI ───────────────────────────────────────────────────────

// VertexBatch is a types.BatchProvider for Vertex AI batch prediction.
// Vertex AI reads batch input from Cloud Storage and writes output there, so
// it needs a bucket: each batch writes its input to
// gs://BUCKET/PREFIX/TAG/input.jsonl and its output under
// gs://BUCKET/PREFIX/TAG/output/. The credentials need object read and
// write on the bucket, and the Vertex AI service agent needs the same.
// Each request carries its custom ID as a label, which batch prediction
// echoes in the output.
type VertexBatch struct {
	a       *Adapter
	bucket  string
	prefix  string
	storage string
	http    *http.Client
	now     func() time.Time
}

// VertexBatchOption configures a VertexBatch.
type VertexBatchOption func(*VertexBatch)

// WithStorageEndpoint overrides the Cloud Storage JSON API endpoint, for a
// test server or a private endpoint.
func WithStorageEndpoint(u string) VertexBatchOption {
	return func(v *VertexBatch) { v.storage = strings.TrimRight(u, "/") }
}

// WithStorageClient sets the HTTP client for Cloud Storage. It must
// authenticate by itself. Default: the adapter's credentials.
func WithStorageClient(c *http.Client) VertexBatchOption {
	return func(v *VertexBatch) { v.http = c }
}

// NewVertexBatch returns a batch provider for a on Vertex AI, staging files
// under location, such as "gs://my-bucket/saige-batches".
func NewVertexBatch(a *Adapter, location string, opts ...VertexBatchOption) (*VertexBatch, error) {
	if a == nil || a.backend.kind != genai.BackendVertexAI {
		return nil, errors.New("google: NewVertexBatch needs an adapter built WithVertex")
	}
	rest, ok := strings.CutPrefix(location, "gs://")
	bucket, prefix, _ := strings.Cut(rest, "/")
	if !ok || bucket == "" {
		return nil, fmt.Errorf("google: batch location %q must be gs://BUCKET[/PREFIX]", location)
	}
	v := &VertexBatch{a: a, bucket: bucket, prefix: strings.Trim(prefix, "/"),
		storage: "https://storage.googleapis.com", now: time.Now}
	for _, o := range opts {
		o(v)
	}
	if v.http == nil {
		switch {
		case a.backend.httpClient != nil && a.backend.credentials == nil:
			v.http = a.backend.httpClient
		default:
			creds := a.backend.credentials
			if creds == nil {
				var err error
				if creds, err = detectCredentials(); err != nil {
					return nil, errors.Join(errors.New("google: vertex batch: no Application Default Credentials"), err)
				}
			}
			v.http = &http.Client{}
			if err := httptransport.AddAuthorizationMiddleware(v.http, creds); err != nil {
				return nil, err
			}
		}
	}
	return v, nil
}

// Name implements types.NamedProvider.
func (v *VertexBatch) Name() string { return providerName }

// Model implements types.ModelProvider.
func (v *VertexBatch) Model() string { return v.a.model }

// Capabilities reports the adapter's catalog row, for pricing.
func (v *VertexBatch) Capabilities() types.ModelCapabilities { return v.a.Capabilities() }

func (v *VertexBatch) object(tag, name string) string {
	parts := []string{}
	if v.prefix != "" {
		parts = append(parts, v.prefix)
	}
	return strings.Join(append(parts, tag, name), "/")
}

// Submit implements types.BatchProvider. Without a tag the batch is staged
// under a timestamp.
func (v *VertexBatch) Submit(ctx context.Context, reqs []types.BatchRequest, opts types.BatchSubmitOptions) (types.BatchHandle, error) {
	if err := batch.CheckIDs(reqs); err != nil {
		return types.BatchHandle{}, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, r := range reqs {
		contents, config, err := v.a.batchRequest(r)
		if err != nil {
			return types.BatchHandle{}, fmt.Errorf("request %s: %w", r.CustomID, err)
		}
		if err := enc.Encode(map[string]any{"request": restRequest(contents, config, r.CustomID)}); err != nil {
			return types.BatchHandle{}, fmt.Errorf("request %s: encode: %w", r.CustomID, err)
		}
	}
	tag := opts.Tag
	if tag == "" {
		tag = "batch-" + strconv.FormatInt(v.now().UnixNano(), 10)
	}
	input := v.object(tag, "input.jsonl")
	if err := v.upload(ctx, input, buf.Bytes()); err != nil {
		return types.BatchHandle{}, fmt.Errorf("%w: %w", batch.ErrNotSubmitted, err)
	}
	output := "gs://" + v.bucket + "/" + v.object(tag, "output")
	job, err := v.a.client.Batches.Create(ctx, v.a.model,
		&genai.BatchJobSource{Format: "jsonl", GCSURI: []string{"gs://" + v.bucket + "/" + input}},
		&genai.CreateBatchJobConfig{DisplayName: tag, Dest: &genai.BatchJobDestination{Format: "jsonl", GCSURI: output}})
	if err != nil {
		return types.BatchHandle{}, classifyGoogleError(v.a.model, err, true)
	}
	return types.BatchHandle{Provider: providerName, Model: v.a.model, ID: job.Name,
		Meta: map[string]string{metaCount: strconv.Itoa(len(reqs)), "output": output}}, nil
}

// restRequest is the REST form of a GenerateContentRequest, which batch
// prediction reads from each input line.
func restRequest(contents []*genai.Content, c *genai.GenerateContentConfig, key string) map[string]any {
	req := map[string]any{"contents": contents, "labels": map[string]string{batchKey: key}}
	if c.SystemInstruction != nil {
		req["systemInstruction"] = c.SystemInstruction
	}
	if len(c.Tools) > 0 {
		req["tools"] = c.Tools
	}
	if c.ToolConfig != nil {
		req["toolConfig"] = c.ToolConfig
	}
	if len(c.SafetySettings) > 0 {
		req["safetySettings"] = c.SafetySettings
	}
	if c.CachedContent != "" {
		req["cachedContent"] = c.CachedContent
	}
	gen := map[string]any{}
	set := func(k string, v any, ok bool) {
		if ok {
			gen[k] = v
		}
	}
	set("temperature", c.Temperature, c.Temperature != nil)
	set("topP", c.TopP, c.TopP != nil)
	set("topK", c.TopK, c.TopK != nil)
	set("maxOutputTokens", c.MaxOutputTokens, c.MaxOutputTokens != 0)
	set("stopSequences", c.StopSequences, len(c.StopSequences) > 0)
	set("presencePenalty", c.PresencePenalty, c.PresencePenalty != nil)
	set("frequencyPenalty", c.FrequencyPenalty, c.FrequencyPenalty != nil)
	set("seed", c.Seed, c.Seed != nil)
	set("responseMimeType", c.ResponseMIMEType, c.ResponseMIMEType != "")
	set("responseSchema", c.ResponseSchema, c.ResponseSchema != nil)
	set("responseJsonSchema", c.ResponseJsonSchema, c.ResponseJsonSchema != nil)
	set("thinkingConfig", c.ThinkingConfig, c.ThinkingConfig != nil)
	if len(gen) > 0 {
		req["generationConfig"] = gen
	}
	return req
}

// Status implements types.BatchProvider.
func (v *VertexBatch) Status(ctx context.Context, h types.BatchHandle) (types.BatchStatus, error) {
	job, err := v.a.client.Batches.Get(ctx, h.ID, nil)
	if err != nil {
		return types.BatchStatus{}, batchError(v.a.model, err)
	}
	return jobStatus(job, h), nil
}

// Results implements types.BatchProvider. It reads every JSONL file under
// the job's output location. Order is not defined; each line's request
// label names its custom ID.
func (v *VertexBatch) Results(ctx context.Context, h types.BatchHandle) iter.Seq2[types.BatchResult, error] {
	return func(yield func(types.BatchResult, error) bool) {
		job, err := v.a.client.Batches.Get(ctx, h.ID, nil)
		if err != nil {
			yield(types.BatchResult{}, batchError(v.a.model, err))
			return
		}
		outcome := unfinishedOutcome(job.State)
		output := h.Meta["output"]
		if job.Dest != nil && job.Dest.GCSURI != "" {
			output = job.Dest.GCSURI
		}
		rest, _ := strings.CutPrefix(output, "gs://"+v.bucket+"/")
		names, err := v.list(ctx, strings.TrimRight(rest, "/")+"/")
		if err != nil {
			yield(types.BatchResult{}, err)
			return
		}
		for _, name := range names {
			if !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			body, err := v.download(ctx, name)
			if err != nil {
				yield(types.BatchResult{}, err)
				return
			}
			ok := readLines(bytes.NewReader(body), func(line []byte) bool {
				var l struct {
					Status   string                             `json:"status"`
					Request  struct{ Labels map[string]string } `json:"request"`
					Response *genai.GenerateContentResponse     `json:"response"`
				}
				if err := json.Unmarshal(line, &l); err != nil {
					yield(types.BatchResult{}, fmt.Errorf("google: decode batch result: %w", err))
					return false
				}
				res := types.BatchResult{CustomID: l.Request.Labels[batchKey]}
				var jobErr *genai.JobError
				if l.Status != "" {
					jobErr = &genai.JobError{Message: l.Status}
				}
				if l.Response != nil && len(l.Response.Candidates) == 0 && l.Response.UsageMetadata == nil && jobErr == nil {
					l.Response = nil
				}
				fillResult(&res, v.a.model, l.Response, jobErr, outcome)
				return yield(res, nil)
			}, func(err error) { yield(types.BatchResult{}, err) })
			if !ok {
				return
			}
		}
	}
}

// Cancel implements types.BatchProvider.
func (v *VertexBatch) Cancel(ctx context.Context, h types.BatchHandle) error {
	if err := v.a.client.Batches.Cancel(ctx, h.ID, nil); err != nil {
		return batchError(v.a.model, err)
	}
	return nil
}

// FindBatch implements types.BatchFinder by matching the tag, which Submit
// sends as the job's display name.
func (v *VertexBatch) FindBatch(ctx context.Context, q types.BatchQuery) (types.BatchHandle, bool, error) {
	h, ok, err := findJob(ctx, v.a.client, v.a.model, q)
	if ok {
		h.Meta["output"] = "gs://" + v.bucket + "/" + v.object(q.Tag, "output")
	}
	return h, ok, err
}

// ── Cloud Storage JSON API ──────────────────────────────────────────

func (v *VertexBatch) do(req *http.Request) ([]byte, error) {
	resp, err := v.http.Do(req) //nolint:gosec // the storage endpoint and bucket come from the host's configuration

	if err != nil {
		return nil, fmt.Errorf("google: cloud storage: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("google: cloud storage: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, streamcheck.HTTPError(providerName, v.a.model, resp.StatusCode, resp.Header, string(body),
			fmt.Errorf("cloud storage %s: %s", req.Method, resp.Status))
	}
	return body, nil
}

func (v *VertexBatch) upload(ctx context.Context, name string, data []byte) error {
	u := v.storage + "/upload/storage/v1/b/" + url.PathEscape(v.bucket) + "/o?uploadType=media&name=" + url.QueryEscape(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/jsonl")
	_, err = v.do(req)
	return err
}

func (v *VertexBatch) list(ctx context.Context, prefix string) ([]string, error) {
	var names []string
	token := ""
	for {
		u := v.storage + "/storage/v1/b/" + url.PathEscape(v.bucket) + "/o?prefix=" + url.QueryEscape(prefix)
		if token != "" {
			u += "&pageToken=" + url.QueryEscape(token)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		body, err := v.do(req)
		if err != nil {
			return nil, err
		}
		var page struct {
			Items []struct {
				Name string `json:"name"`
			} `json:"items"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("google: cloud storage list: %w", err)
		}
		for _, it := range page.Items {
			names = append(names, it.Name)
		}
		if token = page.NextPageToken; token == "" {
			sort.Strings(names)
			return names, nil
		}
	}
}

func (v *VertexBatch) download(ctx context.Context, name string) ([]byte, error) {
	u := v.storage + "/storage/v1/b/" + url.PathEscape(v.bucket) + "/o/" + url.PathEscape(name) + "?alt=media"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	return v.do(req)
}
