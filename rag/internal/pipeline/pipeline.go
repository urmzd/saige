// Package pipeline implements the RAG Pipeline interface.
//
// # Write atomicity
//
// The RAG store write is atomic per store implementation (pgstore wraps the
// document/section/variant inserts in one transaction; memstore applies them
// under a single lock). Knowledge graph enrichment, however, targets a
// separate store and CANNOT be made atomic with the RAG write: the pipeline
// therefore commits the RAG write first and runs graph enrichment afterwards.
// A graph-stage failure never destroys the committed document; it surfaces as
// an error wrapping ragtypes.ErrPartialIngest alongside the (valid) ingest
// result. Failures to store the original bytes or to update a retriever index
// after the commit are reported the same way. This means a document can exist in the RAG store without its
// derived graph facts (or, on delete, graph facts can briefly outlive the
// document if the graph deletion fails); callers needing strict consistency
// must reconcile the two stores themselves.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/urmzd/saige/rag/fusion"
	"github.com/urmzd/saige/rag/internal/textclean"
	knowledgetypes "github.com/urmzd/saige/rag/knowledge/types"
	ragtypes "github.com/urmzd/saige/rag/types"
)

// Config holds the pipeline's dependencies.
type Config struct {
	Store            ragtypes.Store
	ContentExtractor ragtypes.ContentExtractor
	Chunker          ragtypes.Chunker
	Embedders        ragtypes.EmbedderRegistry
	Graph            knowledgetypes.Graph
	// GraphNamespace is the knowledge-graph group every document is
	// ingested into. Empty means the default group.
	GraphNamespace   string
	DedupBehavior    ragtypes.DedupBehavior
	StoreOriginals   bool
	Logger           *slog.Logger
	QueryTransformer ragtypes.QueryTransformer
	Retrievers       []ragtypes.Retriever
	Reranker         ragtypes.Reranker
	ContextAssembler ragtypes.ContextAssembler
	// Scope, when non-empty, is the only scope this pipeline serves: ingests
	// and searches default to it, requests naming another scope fail with
	// ragtypes.ErrScopeMismatch, and UUID-addressed calls (Lookup, Update,
	// Delete, Reconstruct) treat documents of other scopes as not found.
	Scope string
	// Fuser merges retriever lists. Nil means fusion.RRF.
	Fuser ragtypes.Fuser
	// FusionK is the rank constant for searches that set none. Zero means
	// the fuser's own.
	FusionK int
	// FusionWeights are per-retriever fusion weights, keyed by retriever
	// name, for every search. A search's weights override them key by key.
	FusionWeights map[string]float64
	// Observer receives spans and metrics. Nil means ragtypes.NoopObserver.
	Observer ragtypes.Observer
}

var (
	_ ragtypes.IndexRebuilder = (*pipelineImpl)(nil)
	_ ragtypes.SourceSyncer   = (*pipelineImpl)(nil)
)

type pipelineImpl struct {
	cfg Config
	// names labels each retriever for stats, telemetry, and weighted fusion.
	names []string
}

// New creates a new pipeline with the given configuration.
func New(cfg Config) ragtypes.Pipeline {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Fuser == nil {
		cfg.Fuser = fusion.RRF{}
	}
	if cfg.Observer == nil {
		cfg.Observer = ragtypes.NoopObserver{}
	}
	names := make([]string, len(cfg.Retrievers))
	for i, r := range cfg.Retrievers {
		names[i] = componentName(r, fmt.Sprintf("retriever-%d", i))
	}
	return &pipelineImpl{cfg: cfg, names: names}
}

// scopeFor resolves the scope of a request that names requested. A pipeline
// with a fixed scope accepts only that scope (or none, meaning it).
func (p *pipelineImpl) scopeFor(requested string) (string, error) {
	if p.cfg.Scope == "" || requested == p.cfg.Scope {
		return requested, nil
	}
	if requested == "" {
		return p.cfg.Scope, nil
	}
	return "", fmt.Errorf("%w: pipeline serves scope %q, request names %q", ragtypes.ErrScopeMismatch, p.cfg.Scope, requested)
}

// visible reports whether a document of scope may be addressed by UUID
// through this pipeline. Only a pipeline with a fixed scope restricts it.
func (p *pipelineImpl) visible(scope string) bool {
	return p.cfg.Scope == "" || scope == p.cfg.Scope
}

func (p *pipelineImpl) Ingest(ctx context.Context, raw *ragtypes.RawDocument) (*ragtypes.IngestResult, error) {
	return p.ingest(ctx, raw, p.cfg.DedupBehavior)
}

// ingest is Ingest with an explicit dedup behavior, so callers that must not
// replace another document's content can force DedupSkip.
func (p *pipelineImpl) ingest(ctx context.Context, raw *ragtypes.RawDocument, dedup ragtypes.DedupBehavior) (result *ragtypes.IngestResult, err error) {
	scope, err := p.scopeFor(raw.Scope)
	if err != nil {
		return nil, err
	}
	ctx, span := p.cfg.Observer.StartSpan(ctx, ragtypes.SpanIngest, ragtypes.Attr(ragtypes.AttrScope, scope))
	defer func() { endIngestSpan(span, result, err) }()

	fingerprint := ragtypes.Fingerprint(scope, raw.Data)

	existing, err := p.cfg.Store.FindByFingerprint(ctx, fingerprint)
	if err != nil && !errors.Is(err, ragtypes.ErrDocumentNotFound) {
		return nil, fmt.Errorf("fingerprint lookup: %w", err)
	}
	if existing != nil && dedup == ragtypes.DedupSkip {
		return &ragtypes.IngestResult{
			DocumentUUID: existing.UUID,
			Deduplicated: true,
		}, nil
	}
	// DedupReplace: the existing document is NOT deleted here. All fallible
	// preparation (extract, chunk, embed) runs first; the replace happens as a
	// single store-level operation below so a mid-stage failure leaves the
	// prior document intact.

	doc, err := p.prepare(ctx, raw, scope, fingerprint)
	if err != nil {
		return nil, err
	}

	replaceUUID := ""
	if existing != nil {
		replaceUUID = existing.UUID
	}
	result, err = p.commit(ctx, raw, doc, replaceUUID)
	if errors.Is(err, ragtypes.ErrDuplicateDocument) {
		// A concurrent ingest of the same content committed first. Resolve to
		// the winning document, as a sequential DedupSkip ingest would.
		winner, ferr := p.cfg.Store.FindByFingerprint(ctx, fingerprint)
		if ferr != nil {
			return nil, fmt.Errorf("%w (fingerprint lookup: %w)", err, ferr)
		}
		return &ragtypes.IngestResult{DocumentUUID: winner.UUID, Deduplicated: true}, nil
	}
	return result, err
}

// prepare runs every fallible stage that precedes the store write: extract,
// clean, chunk, and embed. Nothing is persisted, so a failure here leaves the
// store untouched. Embedding calls carry ragtypes.PurposeDocument.
func (p *pipelineImpl) prepare(ctx context.Context, raw *ragtypes.RawDocument, scope, fingerprint string) (*ragtypes.Document, error) {
	ctx = ragtypes.WithEmbedPurpose(ctx, ragtypes.PurposeDocument)

	stageCtx, span := p.cfg.Observer.StartSpan(ctx, ragtypes.SpanExtract)
	doc, err := p.cfg.ContentExtractor.Extract(stageCtx, raw)
	if err == nil && doc == nil {
		err = errors.New("extractor returned no document")
	}
	endSpan(span, err)
	if err != nil {
		return nil, fmt.Errorf("extract content: %w", err)
	}
	// Clean at the pipeline boundary as well as in the built-in extractors,
	// so custom extractors cannot hand NUL bytes or invalid UTF-8 to a store.
	textclean.Document(doc)
	doc.Fingerprint = fingerprint
	doc.Scope = scope
	if doc.SourceModifiedAt.IsZero() {
		doc.SourceModifiedAt = raw.SourceModifiedAt
	}

	if p.cfg.Chunker != nil {
		stageCtx, span := p.cfg.Observer.StartSpan(ctx, ragtypes.SpanChunk)
		doc, err = p.cfg.Chunker.Chunk(stageCtx, doc)
		endSpan(span, err)
		if err != nil {
			return nil, fmt.Errorf("chunk: %w", err)
		}
		textclean.Document(doc)
		// A chunker builds a new document; keep the identity set above.
		doc.Fingerprint = fingerprint
		doc.Scope = scope
		if doc.SourceModifiedAt.IsZero() {
			doc.SourceModifiedAt = raw.SourceModifiedAt
		}
	}

	if err := p.embedVariants(ctx, doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// commit writes doc, replacing replaceUUID when it is non-empty, then runs the
// post-commit stages: removing the replaced document from indexers and the
// graph, storing the original bytes, indexing, and graph enrichment. A
// failure in any post-commit stage returns the valid result together with an
// error wrapping ragtypes.ErrPartialIngest.
func (p *pipelineImpl) commit(ctx context.Context, raw *ragtypes.RawDocument, doc *ragtypes.Document, replaceUUID string) (*ragtypes.IngestResult, error) {
	writeCtx, span := p.cfg.Observer.StartSpan(ctx, ragtypes.SpanWrite,
		ragtypes.Attr(ragtypes.AttrDocumentUUID, doc.UUID))
	err := p.writeDocument(writeCtx, replaceUUID, doc)
	endSpan(span, err)
	if err != nil {
		return nil, err
	}

	if replaceUUID != "" {
		// The replaced document's postings and graph episodes are stale. They
		// are removed only now that the new document is committed, so a failed
		// write keeps them. Removal runs before indexing because doc may reuse
		// replaceUUID.
		p.removeFromIndexers(ctx, replaceUUID)
		p.deleteGraphEpisodes(ctx, replaceUUID)
	}

	var partial []error
	if p.cfg.StoreOriginals {
		if err := p.cfg.Store.StoreOriginal(ctx, doc.UUID, raw.Data); err != nil {
			partial = append(partial, fmt.Errorf("store original: %w", err))
		}
	}

	for i, retriever := range p.cfg.Retrievers {
		indexer, ok := retriever.(ragtypes.Indexer)
		if !ok {
			continue
		}
		indexCtx, span := p.cfg.Observer.StartSpan(ctx, ragtypes.SpanIndex,
			ragtypes.Attr(ragtypes.AttrRetriever, p.names[i]))
		err := indexer.Index(indexCtx, doc)
		endSpan(span, err)
		if err != nil {
			p.cfg.Logger.WarnContext(ctx, "retriever index failed",
				"document", doc.UUID, "retriever", p.names[i], "error", err)
			partial = append(partial, fmt.Errorf("index for retriever %s: %w", p.names[i], err))
		}
	}

	if p.cfg.Graph != nil {
		graphCtx, span := p.cfg.Observer.StartSpan(ctx, ragtypes.SpanGraph)
		graphErrs := p.enrichGraph(graphCtx, doc)
		endSpan(span, errors.Join(graphErrs...))
		partial = append(partial, graphErrs...)
	}

	variantCount := 0
	for _, sec := range doc.Sections {
		variantCount += len(sec.Variants)
	}

	result := &ragtypes.IngestResult{
		DocumentUUID: doc.UUID,
		Sections:     len(doc.Sections),
		Variants:     variantCount,
	}
	if len(partial) > 0 {
		return result, fmt.Errorf("%w: %w", ragtypes.ErrPartialIngest, errors.Join(partial...))
	}
	return result, nil
}

// embedVariants computes and attaches embeddings for every variant in doc. It
// fails with an error wrapping ragtypes.ErrEmbeddingShape when the registry
// returns the wrong number of vectors or an empty vector, instead of storing a
// variant that vector search can never find. Dimensions are not compared
// here because a registry may route content types to different embedders.
func (p *pipelineImpl) embedVariants(ctx context.Context, doc *ragtypes.Document) error {
	if p.cfg.Embedders == nil {
		return nil
	}
	var allVariants []ragtypes.ContentVariant
	for _, sec := range doc.Sections {
		allVariants = append(allVariants, sec.Variants...)
	}
	if len(allVariants) == 0 {
		return nil
	}
	embedCtx, span := p.cfg.Observer.StartSpan(ctx, ragtypes.SpanEmbed,
		ragtypes.Attr(ragtypes.AttrInputs, len(allVariants)))
	start := time.Now()
	embeddings, err := p.cfg.Embedders.Embed(embedCtx, allVariants)
	endSpan(span, err)
	p.cfg.Observer.RecordEmbedding(ctx, ragtypes.EmbeddingRecord{
		Embedder: componentName(p.cfg.Embedders, "ingest"),
		Purpose:  ragtypes.EmbedPurposeFrom(ctx),
		Inputs:   len(allVariants),
		Duration: time.Since(start),
		Err:      err,
	})
	if err != nil {
		return fmt.Errorf("embed variants: %w", err)
	}
	if len(embeddings) != len(allVariants) {
		return fmt.Errorf("embed variants: %w: got %d vectors for %d variants",
			ragtypes.ErrEmbeddingShape, len(embeddings), len(allVariants))
	}
	for i, vec := range embeddings {
		if len(vec) == 0 {
			return fmt.Errorf("embed variants: %w: variant %s has an empty vector",
				ragtypes.ErrEmbeddingShape, allVariants[i].UUID)
		}
	}
	idx := 0
	for i := range doc.Sections {
		for j := range doc.Sections[i].Variants {
			doc.Sections[i].Variants[j].Embedding = embeddings[idx]
			idx++
		}
	}
	return nil
}

// writeDocument commits doc to the store, replacing replaceUUID when it is
// non-empty. All fallible preparation has already run, so with a
// DocumentReplacer store the prior document survives any failure here.
func (p *pipelineImpl) writeDocument(ctx context.Context, replaceUUID string, doc *ragtypes.Document) error {
	if replaceUUID == "" {
		if err := p.cfg.Store.CreateDocument(ctx, doc); err != nil {
			return fmt.Errorf("create document: %w", err)
		}
		return nil
	}
	if replacer, ok := p.cfg.Store.(ragtypes.DocumentReplacer); ok {
		if err := replacer.ReplaceDocument(ctx, replaceUUID, doc); err != nil {
			return fmt.Errorf("replace document: %w", err)
		}
		return nil
	}
	// Fallback for stores without atomic replace: the unprotected window
	// is limited to the store write itself.
	if err := p.cfg.Store.DeleteDocument(ctx, replaceUUID); err != nil {
		return fmt.Errorf("delete existing document: %w", err)
	}
	if err := p.cfg.Store.CreateDocument(ctx, doc); err != nil {
		return fmt.Errorf("create document: %w", err)
	}
	return nil
}

// removeFromIndexers removes documentUUID from every retriever that
// implements ragtypes.Indexer. Failures are logged, not fatal: the store is
// the source of truth and indexes are derived from it.
func (p *pipelineImpl) removeFromIndexers(ctx context.Context, documentUUID string) {
	for _, retriever := range p.cfg.Retrievers {
		if indexer, ok := retriever.(ragtypes.Indexer); ok {
			if err := indexer.Remove(ctx, documentUUID); err != nil {
				p.cfg.Logger.WarnContext(ctx, "retriever index remove failed",
					"document", documentUUID, "error", err)
			}
		}
	}
}

// enrichGraph ingests doc's text variants into the knowledge graph. It runs
// against a separate store after the RAG write has committed (see package
// doc); failures surface as ErrPartialIngest alongside the valid result
// rather than destroying the committed document.
//
// Episodes go into the graph namespace with DocumentID = doc.UUID, so
// entities are shared across documents. A graph that cannot delete by
// document and has no namespace configured gets the document UUID as group
// instead, so deleting the document can still remove its facts.
func (p *pipelineImpl) enrichGraph(ctx context.Context, doc *ragtypes.Document) []error {
	if p.cfg.Graph == nil {
		return nil
	}
	groupID := p.graphGroup(doc.UUID)
	var graphErrs []error
	for _, sec := range doc.Sections {
		for _, v := range sec.Variants {
			if v.ContentType != ragtypes.ContentText || v.Text == "" {
				continue
			}
			name := sec.Heading
			if name == "" {
				name = fmt.Sprintf("section-%d", sec.Index)
			}
			_, err := p.cfg.Graph.IngestEpisode(ctx, &knowledgetypes.EpisodeInput{
				Name:       name,
				Body:       v.Text,
				Source:     doc.SourceURI,
				GroupID:    groupID,
				DocumentID: doc.UUID,
				Metadata: map[string]string{
					"content_type": string(v.ContentType),
					"section_uuid": sec.UUID,
					"variant_uuid": v.UUID,
				},
			})
			if err != nil {
				p.cfg.Logger.WarnContext(ctx, "kg ingest failed",
					"section", sec.UUID, "error", err)
				graphErrs = append(graphErrs, fmt.Errorf("kg ingest section %s: %w", sec.UUID, err))
			}
		}
	}
	return graphErrs
}

// graphGroup returns the graph group a document's episodes belong to.
func (p *pipelineImpl) graphGroup(documentUUID string) string {
	if _, ok := p.documentDeleter(); ok || p.cfg.GraphNamespace != "" {
		return p.cfg.GraphNamespace
	}
	return documentUUID
}

// documentDeleter returns the graph's document deleter when the graph can
// really delete by document. A graph that implements
// ragtypes.GraphDocumentDeletionReporter is asked, because its
// DeleteDocumentEpisodes may exist without a store able to serve it.
func (p *pipelineImpl) documentDeleter() (ragtypes.GraphDocumentDeleter, bool) {
	dd, ok := p.cfg.Graph.(ragtypes.GraphDocumentDeleter)
	if !ok {
		return nil, false
	}
	if r, ok := p.cfg.Graph.(ragtypes.GraphDocumentDeletionReporter); ok && !r.SupportsDocumentDeletion() {
		return nil, false
	}
	return dd, true
}

// deleteGraphEpisodes removes the graph episodes derived from documentUUID
// when the configured graph supports deletion. Failures (and graphs without a
// deletion API) are logged, not fatal: the graph is a derived, best-effort
// store (see package doc).
func (p *pipelineImpl) deleteGraphEpisodes(ctx context.Context, documentUUID string) {
	if p.cfg.Graph == nil {
		return
	}
	docDeleter, byDocument := p.documentDeleter()
	if byDocument {
		if err := docDeleter.DeleteDocumentEpisodes(ctx, p.cfg.GraphNamespace, documentUUID); err != nil {
			p.cfg.Logger.WarnContext(ctx, "graph document episode delete failed",
				"document", documentUUID, "error", err)
		}
	}
	groupDeleter, byGroup := p.cfg.Graph.(ragtypes.GraphEpisodeDeleter)
	switch {
	case byGroup && documentUUID != p.cfg.GraphNamespace:
		// Removes episodes grouped under the document UUID: the layout for
		// graphs without document deletion, and for documents ingested by
		// older versions.
		if err := groupDeleter.DeleteEpisodes(ctx, documentUUID); err != nil {
			p.cfg.Logger.WarnContext(ctx, "graph episode delete failed",
				"document", documentUUID, "error", err)
		}
		if !byDocument && p.cfg.GraphNamespace != "" {
			p.cfg.Logger.WarnContext(ctx, "graph cannot delete by document; facts in the namespace retained",
				"document", documentUUID, "namespace", p.cfg.GraphNamespace)
		}
	case !byDocument:
		p.cfg.Logger.WarnContext(ctx, "graph does not support episode deletion; derived facts retained",
			"document", documentUUID)
	}
}

// Lookup returns one variant with its provenance. A pipeline with a fixed
// scope reports variants of other scopes as ragtypes.ErrVariantNotFound.
func (p *pipelineImpl) Lookup(ctx context.Context, variantUUID string) (*ragtypes.SearchHit, error) {
	if p.cfg.Scope != "" {
		if rg, ok := p.cfg.Store.(ragtypes.VariantRecordGetter); ok {
			rec, err := rg.GetVariantRecord(ctx, variantUUID)
			if err != nil {
				return nil, fmt.Errorf("get variant: %w", err)
			}
			if !p.visible(rec.Scope) {
				return nil, fmt.Errorf("get variant: %w", ragtypes.ErrVariantNotFound)
			}
			return &ragtypes.SearchHit{Variant: rec.Variant, Score: 1.0, Provenance: rec.Provenance, Timestamp: rec.Timestamp}, nil
		}
	}
	variant, prov, err := p.cfg.Store.GetVariant(ctx, variantUUID)
	if err != nil {
		return nil, fmt.Errorf("get variant: %w", err)
	}
	if p.cfg.Scope != "" {
		if _, err := p.visibleDocument(ctx, prov.DocumentUUID); err != nil {
			return nil, fmt.Errorf("get variant: %w", ragtypes.ErrVariantNotFound)
		}
	}
	return &ragtypes.SearchHit{
		Variant:    *variant,
		Score:      1.0,
		Provenance: *prov,
	}, nil
}

// visibleDocument loads documentUUID and reports ragtypes.ErrDocumentNotFound
// when it belongs to a scope this pipeline does not serve.
func (p *pipelineImpl) visibleDocument(ctx context.Context, documentUUID string) (*ragtypes.Document, error) {
	doc, err := p.cfg.Store.GetDocument(ctx, documentUUID)
	if err != nil {
		return nil, err
	}
	if !p.visible(doc.Scope) {
		return nil, ragtypes.ErrDocumentNotFound
	}
	return doc, nil
}

// Update replaces the content of documentUUID with raw and keeps the same
// document UUID, so callers' handles stay valid.
//
// Every fallible stage (extract, chunk, embed) runs before the store is
// touched, and the swap uses DocumentReplacer when the store implements it,
// so a failed update leaves the prior document, its index entries, and its
// graph episodes intact. If documentUUID does not exist, raw is ingested as a
// new document.
//
// When raw's content is identical to the current content of documentUUID,
// DedupSkip returns the document unchanged with Deduplicated set, and
// DedupReplace re-ingests it. When raw's content already belongs to a
// different document, Update returns an error wrapping
// ragtypes.ErrDuplicateDocument and changes nothing.
//
// The document keeps its scope. When raw names no scope and the pipeline has
// no fixed scope, the document's scope is used. A raw scope that differs from
// the document's fails with ragtypes.ErrScopeMismatch. A pipeline with a fixed
// scope treats documents of other scopes as missing.
func (p *pipelineImpl) Update(ctx context.Context, documentUUID string, raw *ragtypes.RawDocument) (*ragtypes.IngestResult, error) {
	scope, err := p.scopeFor(raw.Scope)
	if err != nil {
		return nil, err
	}
	old, err := p.visibleDocument(ctx, documentUUID)
	if err != nil && !errors.Is(err, ragtypes.ErrDocumentNotFound) {
		return nil, fmt.Errorf("get document: %w", err)
	}
	if old == nil {
		return p.Ingest(ctx, raw)
	}
	if raw.Scope == "" && p.cfg.Scope == "" {
		scope = old.Scope
	}
	if scope != old.Scope {
		return nil, fmt.Errorf("%w: document %s is in scope %q, update names %q",
			ragtypes.ErrScopeMismatch, documentUUID, old.Scope, scope)
	}

	ctx, span := p.cfg.Observer.StartSpan(ctx, ragtypes.SpanIngest,
		ragtypes.Attr(ragtypes.AttrScope, scope), ragtypes.Attr(ragtypes.AttrDocumentUUID, documentUUID))
	result, err := p.update(ctx, old, raw, scope)
	endIngestSpan(span, result, err)
	return result, err
}

func (p *pipelineImpl) update(ctx context.Context, old *ragtypes.Document, raw *ragtypes.RawDocument, scope string) (*ragtypes.IngestResult, error) {
	documentUUID := old.UUID
	fingerprint := ragtypes.Fingerprint(scope, raw.Data)
	owner, err := p.cfg.Store.FindByFingerprint(ctx, fingerprint)
	if err != nil && !errors.Is(err, ragtypes.ErrDocumentNotFound) {
		return nil, fmt.Errorf("fingerprint lookup: %w", err)
	}
	if owner != nil {
		if owner.UUID != documentUUID {
			return nil, fmt.Errorf("%w: content is already stored as document %s", ragtypes.ErrDuplicateDocument, owner.UUID)
		}
		if p.cfg.DedupBehavior == ragtypes.DedupSkip {
			return &ragtypes.IngestResult{DocumentUUID: documentUUID, Deduplicated: true}, nil
		}
	}

	doc, err := p.prepare(ctx, raw, scope, fingerprint)
	if err != nil {
		return nil, err
	}
	doc.UUID = documentUUID
	for i := range doc.Sections {
		doc.Sections[i].DocumentUUID = documentUUID
	}
	if !old.CreatedAt.IsZero() {
		doc.CreatedAt = old.CreatedAt
	}
	return p.commit(ctx, raw, doc, documentUUID)
}

// Delete removes a document, its index entries, and its graph episodes.
// Deleting a missing document is a no-op. A pipeline with a fixed scope
// treats a document of another scope as missing and leaves it untouched.
func (p *pipelineImpl) Delete(ctx context.Context, documentUUID string) error {
	if p.cfg.Scope != "" {
		if _, err := p.visibleDocument(ctx, documentUUID); err != nil {
			if errors.Is(err, ragtypes.ErrDocumentNotFound) {
				return nil
			}
			return fmt.Errorf("get document: %w", err)
		}
	}
	// Delete the document first: if the store delete fails, its index
	// postings and graph facts must survive with it. Derived state may
	// briefly outlive the document (retrieval skips variants the store no
	// longer has), never the reverse. Removing postings only after the store
	// delete also keeps a concurrent RebuildIndex from re-adding them.
	if err := p.cfg.Store.DeleteDocument(ctx, documentUUID); err != nil {
		return err
	}
	p.removeFromIndexers(ctx, documentUUID)
	p.deleteGraphEpisodes(ctx, documentUUID)
	return nil
}

func (p *pipelineImpl) Reconstruct(ctx context.Context, documentUUID string) (*ragtypes.Document, error) {
	doc, err := p.visibleDocument(ctx, documentUUID)
	if err != nil {
		return nil, fmt.Errorf("get document: %w", err)
	}
	return doc, nil
}

// RebuildIndex rebuilds every retriever index that implements
// ragtypes.IndexRebuilder from the store. It returns the joined errors of the
// retrievers that failed; the others are still rebuilt.
func (p *pipelineImpl) RebuildIndex(ctx context.Context) error {
	var errs []error
	for i, retriever := range p.cfg.Retrievers {
		rebuilder, ok := retriever.(ragtypes.IndexRebuilder)
		if !ok {
			continue
		}
		if err := rebuilder.RebuildIndex(ctx); err != nil {
			errs = append(errs, fmt.Errorf("rebuild retriever %d: %w", i, err))
		}
	}
	return errors.Join(errs...)
}

func (p *pipelineImpl) Close(ctx context.Context) error {
	return p.cfg.Store.Close(ctx)
}
