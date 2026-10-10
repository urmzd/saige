// Package tool provides agent Tool implementations for RAG Pipeline operations.
package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	agenttypes "github.com/urmzd/saige/agent/types"
	ragtypes "github.com/urmzd/saige/rag/types"
)

// --- Parameter types for SchemaFrom ---

type searchParams struct {
	Query        string   `json:"query" description:"Search query text"`
	Limit        int      `json:"limit,omitempty" description:"Maximum number of results (at most 50)"`
	ContentTypes []string `json:"content_types,omitempty" description:"Filter by content types"`
	MinScore     float64  `json:"min_score,omitempty" description:"Minimum relevance from 0 to 1, relative to a hit ranked first by every retriever. Omit it unless results contain too much noise; 0.5 is already strict"`
}

type lookupParams struct {
	VariantUUID string `json:"variant_uuid" description:"UUID of the variant to look up"`
	Section     bool   `json:"section,omitempty" description:"Return the full text of the variant's parent section instead of the variant alone"`
}

type updateParams struct {
	DocumentUUID string `json:"document_uuid" description:"UUID of the document to update"`
	SourceURI    string `json:"source_uri" description:"Source URI of the new content"`
	MIMEType     string `json:"mime_type" description:"MIME type of the new content"`
	Data         string `json:"data" description:"Base64-encoded or plain text content data"`
}

type deleteParams struct {
	DocumentUUID string `json:"document_uuid" description:"UUID of the document to delete"`
}

type reconstructParams struct {
	DocumentUUID string `json:"document_uuid" description:"UUID of the document to reconstruct"`
}

// --- SearchTool ---

// maxSearchLimit caps the number of hits rag_search returns in one call.
const maxSearchLimit = 50

// Default output caps for rag_lookup and rag_reconstruct: bytes of text for
// one variant, and bytes of encoded output for one call.
const (
	defaultMaxVariantText = 16 << 10
	defaultMaxOutputText  = 64 << 10
)

// SearchTool searches the pipeline and returns provenance-only hits.
//
// As a RichTool it also returns a retrieval citation per hit, anchored to
// the hit's chunk (document, section and variant, with the byte offsets of
// any highlighted terms), and the media of image, audio, video and document
// hits as parts, within the media cap (see WithMediaLimits). Execute returns
// the same text without the media.
type SearchTool struct {
	pipeline ragtypes.Pipeline
	media    mediaLimits
}

var (
	_ agenttypes.RichTool = (*SearchTool)(nil)
	_ agenttypes.RichTool = (*LookupTool)(nil)
)

// Definition implements agenttypes.Tool.
func (t *SearchTool) Definition() agenttypes.ToolDef {
	return agenttypes.ToolDef{
		Name:        "rag_search",
		Description: "Search the knowledge base. Returns a JSON array of scored hits with provenance metadata (no full content; use rag_lookup to dereference). When a retriever failed but others returned hits, returns an object {hits, degraded: true, warnings} instead.",
		Parameters:  agenttypes.SchemaFrom[searchParams](),
		Capability:  agenttypes.ToolCapabilityRead,
	}
}

// Execute implements agenttypes.Tool.
func (t *SearchTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	r, err := t.ExecuteRich(ctx, args)
	if err != nil {
		return "", err
	}
	return textOnly(r), nil
}

// ExecuteRich implements agenttypes.RichTool: the hits as JSON text, the
// media of media hits, and one citation per hit.
func (t *SearchTool) ExecuteRich(ctx context.Context, args map[string]any) (agenttypes.ToolResult, error) {
	query, _ := args["query"].(string)
	if query == "" {
		return agenttypes.ToolResult{}, fmt.Errorf("query is required")
	}

	var opts []ragtypes.SearchOption
	if limit, ok := toInt(args["limit"]); ok && limit > 0 {
		opts = append(opts, ragtypes.WithLimit(min(limit, maxSearchLimit)))
	}
	if minScore, ok := args["min_score"].(float64); ok && minScore > 0 {
		opts = append(opts, ragtypes.WithMinScore(minScore))
	}
	if ctRaw, ok := args["content_types"].([]any); ok {
		var ctypes []ragtypes.ContentType
		for _, ct := range ctRaw {
			if s, ok := ct.(string); ok {
				ctypes = append(ctypes, ragtypes.ContentType(s))
			}
		}
		if len(ctypes) > 0 {
			opts = append(opts, ragtypes.WithContentTypes(ctypes...))
		}
	}

	result, err := t.pipeline.Search(ctx, query, opts...)
	// A partial failure still returns usable hits; report it alongside them.
	degraded := err != nil && errors.Is(err, ragtypes.ErrPartialSearch) && result != nil
	if err != nil && !degraded {
		return agenttypes.ToolResult{}, err
	}

	// Return provenance-only hits (no full content to keep agent context lean).
	type provenanceHit struct {
		VariantUUID string               `json:"variant_uuid"`
		Score       float64              `json:"score"`
		ContentType ragtypes.ContentType `json:"content_type"`
		Provenance  ragtypes.Provenance  `json:"provenance"`
	}
	hits := make([]provenanceHit, len(result.Hits))
	for i, h := range result.Hits {
		hits[i] = provenanceHit{
			VariantUUID: h.Variant.UUID,
			Score:       h.Score,
			ContentType: h.Variant.ContentType,
			Provenance:  h.Provenance,
		}
	}

	var payload any = hits
	if degraded {
		payload = struct {
			Hits     []provenanceHit `json:"hits"`
			Degraded bool            `json:"degraded"`
			Warnings []string        `json:"warnings"`
		}{Hits: hits, Degraded: true, Warnings: []string{err.Error()}}
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return agenttypes.ToolResult{}, fmt.Errorf("marshal results: %w", err)
	}
	out := agenttypes.ToolResult{Parts: []agenttypes.ToolOutputPart{agenttypes.Text(string(data))}}
	budget := t.media.withDefaults()
	for _, h := range result.Hits {
		out.Citations = append(out.Citations, hitCitation("rag_search", h, ""))
		out.Parts = budget.attach(out.Parts, h.Variant)
	}
	return out, nil
}

// textOnly is the text Execute returns: the JSON text part, without the
// labels of attached media.
func textOnly(r agenttypes.ToolResult) string {
	if len(r.Parts) == 0 {
		return ""
	}
	if t, ok := r.Parts[0].(agenttypes.TextPart); ok {
		return t.Text
	}
	return r.Text()
}

// hitCitation is the retrieval citation of a hit: its document as the
// source, and its chunk as the anchor in Meta (document_uuid,
// section_uuid, section_index, variant_uuid, score, and, when set, the
// widened section window and the highlighted term spans as byte offsets
// into the variant text). quote, when not empty, is the cited text, whose
// offsets in the variant text are Meta's start and end.
func hitCitation(producer string, h ragtypes.SearchHit, quote string) agenttypes.Citation {
	pv := h.Provenance
	title := pv.DocumentTitle
	if title == "" {
		title = pv.SectionHeading
	}
	if title == "" && pv.SourceURI == "" {
		title = "document " + pv.DocumentUUID
	}
	c := agenttypes.NewCitation(agenttypes.CitationRetrieval, pv.SourceURI, title)
	c.Producer = producer
	c.Meta = map[string]any{
		"document_uuid": pv.DocumentUUID,
		"section_uuid":  pv.SectionUUID,
		"section_index": pv.SectionIndex,
		"variant_uuid":  h.Variant.UUID,
	}
	if h.Score != 0 {
		c.Meta["score"] = h.Score
	}
	if pv.SectionHeading != "" {
		c.Meta["section_heading"] = pv.SectionHeading
	}
	if pv.Window != nil {
		c.Meta["window"] = map[string]int{"first": pv.Window.First, "last": pv.Window.Last}
	}
	if h.Highlight != nil && len(h.Highlight.Spans) > 0 {
		spans := make([][2]int, len(h.Highlight.Spans))
		for i, sp := range h.Highlight.Spans {
			spans[i] = [2]int{sp.Start, sp.End}
		}
		c.Meta["spans"] = spans
	}
	if quote != "" {
		c.Quote = quote
		c.Meta["start"], c.Meta["end"] = 0, len(quote)
	}
	return c
}

// maxQuoteBytes bounds the quote a lookup citation carries.
const maxQuoteBytes = 1 << 10

// Default media caps for one rag_search or rag_lookup call.
const (
	defaultMaxMediaBytes = 4 << 20
	defaultMaxMediaParts = 4
)

// mediaLimits bounds the media parts one call returns: total bytes and
// part count. A negative bytes value returns none.
type mediaLimits struct {
	bytes int
	parts int
}

func (m mediaLimits) withDefaults() *mediaLimits {
	if m.bytes == 0 {
		m.bytes = defaultMaxMediaBytes
	}
	if m.parts <= 0 {
		m.parts = defaultMaxMediaParts
	}
	return &m
}

// attach appends v's media part to parts when v is a media variant with
// bytes that fit in what remains of the caps.
func (m *mediaLimits) attach(parts []agenttypes.ToolOutputPart, v ragtypes.ContentVariant) []agenttypes.ToolOutputPart {
	if len(v.Data) == 0 || m.bytes <= 0 || m.parts <= 0 || len(v.Data) > m.bytes {
		return parts
	}
	switch v.ContentType {
	case ragtypes.ContentImage, ragtypes.ContentAudio, ragtypes.ContentVideo, ragtypes.ContentDocument:
	default:
		return parts
	}
	part, err := v.Part()
	if err != nil {
		return parts
	}
	out, ok := part.(agenttypes.ToolOutputPart)
	if !ok || !agenttypes.IsMedia(part) {
		return parts
	}
	m.bytes -= len(v.Data)
	m.parts--
	return append(parts, agenttypes.Text("[media of variant "+v.UUID+"]"), out)
}

// --- LookupTool ---

// LookupTool retrieves full content for a specific variant by UUID.
//
// As a RichTool it also returns a retrieval citation anchored to the
// variant, quoting the start of its text, and the variant's media as a part
// when it is an image, audio, video or document variant within the media
// cap.
type LookupTool struct {
	pipeline ragtypes.Pipeline
	limits   outputLimits
	media    mediaLimits
}

// Definition implements agenttypes.Tool.
func (t *LookupTool) Definition() agenttypes.ToolDef {
	return agenttypes.ToolDef{
		Name:        "rag_lookup",
		Description: "Look up a specific content variant by UUID. Returns its text with provenance; binary data is described by MIME type and size, and long text is truncated. Set section to read the whole parent section, as search does when parent context is enabled.",
		Parameters:  agenttypes.SchemaFrom[lookupParams](),
		Capability:  agenttypes.ToolCapabilityRead,
	}
}

// Execute implements agenttypes.Tool.
func (t *LookupTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	r, err := t.ExecuteRich(ctx, args)
	if err != nil {
		return "", err
	}
	return textOnly(r), nil
}

// ExecuteRich implements agenttypes.RichTool: the variant as JSON text, its
// media, and its citation.
func (t *LookupTool) ExecuteRich(ctx context.Context, args map[string]any) (agenttypes.ToolResult, error) {
	uuid, _ := args["variant_uuid"].(string)
	if uuid == "" {
		return agenttypes.ToolResult{}, fmt.Errorf("variant_uuid is required")
	}

	hit, err := t.pipeline.Lookup(ctx, uuid)
	if err != nil {
		return agenttypes.ToolResult{}, err
	}

	if section, _ := args["section"].(bool); section {
		doc, err := t.pipeline.Reconstruct(ctx, hit.Provenance.DocumentUUID)
		if err != nil {
			return agenttypes.ToolResult{}, fmt.Errorf("load parent section: %w", err)
		}
		if text, ok := sectionText(doc, hit.Provenance.SectionUUID); ok {
			hit.Variant.Text = text
			hit.Provenance.ExpandedFromVariantUUID = hit.Variant.UUID
		}
	}

	// Charge the provenance and the object wrapper before the variant.
	budget := t.limits.withDefaults().total - encodedLen(hit.Provenance) - len(`,"provenance":`)
	vo, _ := t.limits.variant(hit.Variant, &budget)
	out := lookupOutput{variantOutput: vo, Provenance: hit.Provenance}

	data, err := json.Marshal(out)
	if err != nil {
		return agenttypes.ToolResult{}, fmt.Errorf("marshal hit: %w", err)
	}
	quote, _ := truncateUTF8(vo.Text, maxQuoteBytes)
	res := agenttypes.ToolResult{
		Parts:     []agenttypes.ToolOutputPart{agenttypes.Text(string(data))},
		Citations: []agenttypes.Citation{hitCitation("rag_lookup", *hit, quote)},
	}
	res.Parts = t.media.withDefaults().attach(res.Parts, hit.Variant)
	return res, nil
}

// sectionText joins the text of every variant in the named section.
func sectionText(doc *ragtypes.Document, sectionUUID string) (string, bool) {
	if doc == nil {
		return "", false
	}
	for _, sec := range doc.Sections {
		if sec.UUID != sectionUUID {
			continue
		}
		var texts []string
		for _, v := range sec.Variants {
			if v.Text != "" {
				texts = append(texts, v.Text)
			}
		}
		return strings.Join(texts, "\n\n"), len(texts) > 0
	}
	return "", false
}

// --- Output shapes for lookup and reconstruct ---

// outputLimits bounds what rag_lookup and rag_reconstruct return: perVariant
// caps the text bytes of one variant, and total caps the encoded output of
// one call, including field names, metadata, and entries for variants with
// no text. Embeddings and raw bytes are never returned.
type outputLimits struct {
	perVariant int
	total      int
}

func (l outputLimits) withDefaults() outputLimits {
	if l.perVariant <= 0 {
		l.perVariant = defaultMaxVariantText
	}
	if l.total <= 0 {
		l.total = defaultMaxOutputText
	}
	return l
}

// variantOutput is a variant as shown to an agent: text, a description of
// any binary payload, and metadata, without the embedding.
type variantOutput struct {
	UUID          string               `json:"variant_uuid"`
	ContentType   ragtypes.ContentType `json:"content_type"`
	MIMEType      string               `json:"mime_type,omitempty"`
	Text          string               `json:"text,omitempty"`
	TextTruncated bool                 `json:"text_truncated,omitempty"`
	DataBytes     int                  `json:"data_bytes,omitempty"`
	Metadata      map[string]string    `json:"metadata,omitempty"`
	// MetadataTruncated reports that metadata entries were left out to stay
	// within the output cap.
	MetadataTruncated bool `json:"metadata_truncated,omitempty"`
}

// entryOverhead is charged for each variant or section entry to cover the
// comma that separates it from its neighbours in the encoded output.
const entryOverhead = 1

// textFieldOverhead is the encoded size of the text field name and colon.
var textFieldOverhead = len(`,"text":`)

// encodedLen returns the size of v encoded as JSON.
func encodedLen(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b)
}

// fitMetadata keeps the entries of meta, in key order, whose encoded size
// fits in limit bytes, and reports whether any entry was left out.
func fitMetadata(meta map[string]string, limit int) (map[string]string, bool) {
	if len(meta) == 0 || encodedLen(meta) <= limit {
		return meta, false
	}
	keys := make([]string, 0, len(meta))
	for k := range meta {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	kept := make(map[string]string)
	used := len("{}")
	for _, k := range keys {
		// Key, colon, value, and separating comma.
		n := encodedLen(k) + 1 + encodedLen(meta[k]) + 1
		if used+n > limit {
			continue
		}
		kept[k] = meta[k]
		used += n
	}
	if len(kept) == 0 {
		return nil, true
	}
	return kept, true
}

// fitText cuts s to at most n bytes and then until its encoded form fits in
// limit bytes, without splitting a rune. It reports whether anything was
// removed.
func fitText(s string, n, limit int) (string, bool) {
	text, cut := truncateUTF8(s, max(n, 0))
	for text != "" {
		over := encodedLen(text) - limit
		if over <= 0 {
			break
		}
		// Every source byte encodes to at least one output byte, so
		// dropping the excess always makes progress.
		text, _ = truncateUTF8(text, max(len(text)-over, 0))
		cut = true
	}
	return text, cut
}

// variant converts v within what remains of *budget, which it decreases by
// the encoded size of the entry. Metadata may use up to half of the room
// and text gets the rest, capped at the per-variant limit. It reports false,
// leaving *budget unchanged, when not even the entry without text fits.
func (l outputLimits) variant(v ragtypes.ContentVariant, budget *int) (variantOutput, bool) {
	l = l.withDefaults()
	room := max(*budget, 0)
	meta, metaCut := fitMetadata(v.Metadata, room/2)
	vo := variantOutput{
		UUID:              v.UUID,
		ContentType:       v.ContentType,
		MIMEType:          v.MIMEType,
		DataBytes:         len(v.Data),
		Metadata:          meta,
		MetadataTruncated: metaCut,
	}
	// Measure the entry as if text were cut, so the flag is paid for.
	shellProbe := vo
	shellProbe.TextTruncated = true
	shell := encodedLen(shellProbe) + entryOverhead
	if shell > room {
		return vo, false
	}
	*budget -= shell
	if v.Text == "" {
		return vo, true
	}
	if *budget-textFieldOverhead <= len(`""`) {
		vo.TextTruncated = true
		return vo, true
	}
	text, cut := fitText(v.Text, l.perVariant, *budget-textFieldOverhead)
	vo.Text = text
	vo.TextTruncated = cut
	if text != "" {
		*budget -= textFieldOverhead + encodedLen(text)
	}
	return vo, true
}

type lookupOutput struct {
	variantOutput
	Provenance ragtypes.Provenance `json:"provenance"`
}

// truncateUTF8 returns at most n bytes of s without splitting a rune, and
// whether anything was removed.
func truncateUTF8(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}

// --- UpdateTool ---

// UpdateTool re-ingests a document with new content.
type UpdateTool struct {
	pipeline ragtypes.Pipeline
}

// Definition implements agenttypes.Tool.
func (t *UpdateTool) Definition() agenttypes.ToolDef {
	return agenttypes.ToolDef{
		Name:        "rag_update",
		Description: "Replace a document's content and keep its document_uuid. The old version stays intact if the update fails.",
		Parameters:  agenttypes.SchemaFrom[updateParams](),
		Capability:  agenttypes.ToolCapabilityWrite,
	}
}

// Execute implements agenttypes.Tool.
func (t *UpdateTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	docUUID, _ := args["document_uuid"].(string)
	if docUUID == "" {
		return "", fmt.Errorf("document_uuid is required")
	}
	sourceURI, _ := args["source_uri"].(string)
	mimeType, _ := args["mime_type"].(string)
	rawData, _ := args["data"].(string)

	result, err := t.pipeline.Update(ctx, docUUID, &ragtypes.RawDocument{
		SourceURI: sourceURI,
		MIMEType:  mimeType,
		Data:      []byte(rawData),
	})
	if err != nil {
		return "", err
	}

	data, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("marshal result: %w", err)
	}
	return string(data), nil
}

// --- DeleteTool ---

// DeleteTool deletes a document from the pipeline.
type DeleteTool struct {
	pipeline ragtypes.Pipeline
}

// Definition implements agenttypes.Tool.
func (t *DeleteTool) Definition() agenttypes.ToolDef {
	return agenttypes.ToolDef{
		Name:        "rag_delete",
		Description: "Delete a document from the knowledge base by UUID.",
		Parameters:  agenttypes.SchemaFrom[deleteParams](),
		Capability:  agenttypes.ToolCapabilityDestructive,
	}
}

// Execute implements agenttypes.Tool.
func (t *DeleteTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	docUUID, _ := args["document_uuid"].(string)
	if docUUID == "" {
		return "", fmt.Errorf("document_uuid is required")
	}

	if err := t.pipeline.Delete(ctx, docUUID); err != nil {
		return "", err
	}
	return `{"status":"deleted"}`, nil
}

// --- ReconstructTool ---

// ReconstructTool reconstructs a full document structure.
type ReconstructTool struct {
	pipeline ragtypes.Pipeline
	limits   outputLimits
}

// Definition implements agenttypes.Tool.
func (t *ReconstructTool) Definition() agenttypes.ToolDef {
	return agenttypes.ToolDef{
		Name:        "rag_reconstruct",
		Description: "Reconstruct the document structure: sections and the text of their variants. Binary data is described by MIME type and size. Long output is truncated and marked with truncated: true.",
		Parameters:  agenttypes.SchemaFrom[reconstructParams](),
		Capability:  agenttypes.ToolCapabilityRead,
	}
}

type sectionOutput struct {
	UUID     string          `json:"section_uuid"`
	Index    int             `json:"index"`
	Heading  string          `json:"heading,omitempty"`
	Variants []variantOutput `json:"variants"`
}

type documentOutput struct {
	UUID      string            `json:"document_uuid"`
	SourceURI string            `json:"source_uri,omitempty"`
	Title     string            `json:"title,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
	Sections  []sectionOutput   `json:"sections"`
	// Truncated reports that text was cut or that later sections or
	// variants were left out to stay within the output cap.
	Truncated bool `json:"truncated,omitempty"`
}

// Execute implements agenttypes.Tool.
func (t *ReconstructTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	docUUID, _ := args["document_uuid"].(string)
	if docUUID == "" {
		return "", fmt.Errorf("document_uuid is required")
	}

	doc, err := t.pipeline.Reconstruct(ctx, docUUID)
	if err != nil {
		return "", err
	}

	data, err := json.Marshal(t.limits.document(doc))
	if err != nil {
		return "", fmt.Errorf("marshal document: %w", err)
	}
	return string(data), nil
}

// document converts doc to its agent-facing shape within the output cap.
// Every entry, including variants with no text and their metadata, is
// charged at its encoded size. Once the budget is spent, remaining sections
// and variants are omitted and the document is marked truncated.
func (l outputLimits) document(doc *ragtypes.Document) documentOutput {
	l = l.withDefaults()
	meta, metaCut := fitMetadata(doc.Metadata, l.total/4)
	out := documentOutput{
		UUID:      doc.UUID,
		SourceURI: doc.SourceURI,
		Title:     doc.Title,
		Metadata:  meta,
		CreatedAt: doc.CreatedAt,
		UpdatedAt: doc.UpdatedAt,
		Sections:  make([]sectionOutput, 0, len(doc.Sections)),
		Truncated: true, // measured with the flag set so it is paid for
	}
	budget := l.total - encodedLen(out)
	out.Truncated = metaCut
	for _, sec := range doc.Sections {
		so := sectionOutput{UUID: sec.UUID, Index: sec.Index, Heading: sec.Heading, Variants: []variantOutput{}}
		cost := encodedLen(so) + entryOverhead
		if cost > budget {
			break
		}
		budget -= cost
		for _, v := range sec.Variants {
			vo, ok := l.variant(v, &budget)
			if !ok {
				break
			}
			out.Truncated = out.Truncated || vo.TextTruncated || vo.MetadataTruncated
			so.Variants = append(so.Variants, vo)
		}
		out.Sections = append(out.Sections, so)
		if len(so.Variants) < len(sec.Variants) {
			out.Truncated = true
			break
		}
	}
	if len(out.Sections) < len(doc.Sections) {
		out.Truncated = true
	}
	return out
}

// --- NewTools ---

// config controls how NewTools assembles the tool set.
type config struct {
	readOnly bool
	limits   outputLimits
	media    mediaLimits
}

// Option configures NewTools.
type Option func(*config)

// ReadOnly omits every mutating tool (rag_update, rag_delete) from the returned
// set, exposing only safe read tools (rag_search, rag_lookup, rag_reconstruct).
// Use this for untrusted or query-only agents.
func ReadOnly() Option {
	return func(c *config) { c.readOnly = true }
}

// WithOutputLimits caps what rag_lookup and rag_reconstruct return:
// perVariant bytes of text for any one variant and total bytes of encoded
// output for one call. The total covers every entry, including variants with
// no text and all metadata. Values <= 0 keep the defaults of 16 KiB and
// 64 KiB. Cut text is marked text_truncated, dropped metadata entries are
// marked metadata_truncated, and rag_reconstruct marks an incomplete document
// truncated.
func WithOutputLimits(perVariant, total int) Option {
	return func(c *config) { c.limits = outputLimits{perVariant: perVariant, total: total} }
}

// WithMediaLimits caps the media rag_search and rag_lookup return as parts
// beside their text: at most maxBytes bytes and maxParts parts in one call.
// A variant larger than what remains is described in the text only (its
// mime_type and data_bytes), as before. Zero keeps the defaults of 4 MiB and
// 4 parts; a negative maxBytes returns no media. Media a serving model
// cannot take natively is handled by the agent's conversion policy, which
// rejects it by default.
func WithMediaLimits(maxBytes, maxParts int) Option {
	return func(c *config) { c.media = mediaLimits{bytes: maxBytes, parts: maxParts} }
}

// mutatingMarker is the human-approval gate attached to mutating RAG tools.
// When the agent loop encounters a tool carrying this marker, it pauses and
// waits for explicit approval before executing.
func mutatingMarker(tool string) agenttypes.Marker {
	return agenttypes.Marker{
		Kind:    "human_approval",
		Message: "Mutating RAG operation requires human approval: " + tool,
		Meta:    map[string]any{"tool": tool, "mutating": true},
	}
}

// NewTools returns rag tools for use with an agent.
//
// By default the read tools (rag_search, rag_lookup, rag_reconstruct) are
// returned as-is and the mutating tools (rag_update, rag_delete) are wrapped in
// a "human_approval" marker so the agent loop pauses for approval before they
// run. Pass ReadOnly() to omit the mutating tools entirely instead.
func NewTools(pipeline ragtypes.Pipeline, opts ...Option) []agenttypes.Tool {
	cfg := config{}
	for _, o := range opts {
		o(&cfg)
	}

	tools := []agenttypes.Tool{
		&SearchTool{pipeline: pipeline, media: cfg.media},
		&LookupTool{pipeline: pipeline, limits: cfg.limits, media: cfg.media},
	}

	if !cfg.readOnly {
		tools = append(tools,
			agenttypes.WithMarkers(&UpdateTool{pipeline: pipeline}, mutatingMarker("rag_update")),
			agenttypes.WithMarkers(&DeleteTool{pipeline: pipeline}, mutatingMarker("rag_delete")),
		)
	}

	tools = append(tools, &ReconstructTool{pipeline: pipeline, limits: cfg.limits})

	return tools
}

// toInt converts a JSON number (float64) to int.
func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	default:
		return 0, false
	}
}
