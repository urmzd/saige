package knowledge

import (
	"context"
	"errors"
	"time"

	"github.com/urmzd/saige/rag/knowledge/types"
	ragtypes "github.com/urmzd/saige/rag/types"
)

// Span names an observed graph opens.
const (
	SpanIngestEpisode = "knowledge.ingest_episode"
	SpanSearchFacts   = "knowledge.search_facts"
	SpanDelete        = "knowledge.delete"
)

// Attribute keys an observed graph sets.
const (
	AttrEntities  = "knowledge.entities"
	AttrRelations = "knowledge.relations"
	AttrFacts     = "knowledge.facts"
)

// errDeleteUnsupported is returned by an observed graph's delete methods
// when the wrapped graph cannot delete.
var errDeleteUnsupported = errors.New("knowledge: graph does not support deletion")

// observedGraph wraps a Graph with spans and retrieval metrics. It forwards
// the optional deletion interfaces the RAG pipeline looks for, so wrapping
// never hides them.
type observedGraph struct {
	types.Graph
	observer ragtypes.Observer
}

var (
	_ ragtypes.GraphEpisodeDeleter           = (*observedGraph)(nil)
	_ ragtypes.GraphDocumentDeleter          = (*observedGraph)(nil)
	_ ragtypes.GraphDocumentDeletionReporter = (*observedGraph)(nil)
	_ types.GroupScopedGraphReader           = (*observedGraph)(nil)
)

// Observe wraps g so episode ingest, fact search, and deletion open spans on
// observer, and each fact search is reported to Observer.RecordRetrieval
// under the retriever name "knowledge". A nil observer returns g unchanged.
// NewGraph applies it when WithObserver is set.
func Observe(g types.Graph, observer ragtypes.Observer) types.Graph {
	if observer == nil || g == nil {
		return g
	}
	return &observedGraph{Graph: g, observer: observer}
}

// Unwrap returns the wrapped graph.
func (o *observedGraph) Unwrap() types.Graph { return o.Graph }

func (o *observedGraph) IngestEpisode(ctx context.Context, input *types.EpisodeInput) (*types.IngestResult, error) {
	ctx, span := o.observer.StartSpan(ctx, SpanIngestEpisode)
	res, err := o.Graph.IngestEpisode(ctx, input)
	if res != nil {
		span.SetAttributes(
			ragtypes.Attr(AttrEntities, len(res.EntityNodes)),
			ragtypes.Attr(AttrRelations, len(res.EpisodicEdges)),
		)
	}
	end(span, err)
	return res, err
}

func (o *observedGraph) SearchFacts(ctx context.Context, query string, opts ...types.SearchOption) (*types.SearchFactsResult, error) {
	ctx, span := o.observer.StartSpan(ctx, SpanSearchFacts)
	start := time.Now()
	res, err := o.Graph.SearchFacts(ctx, query, opts...)
	elapsed := time.Since(start)
	facts := 0
	if res != nil {
		facts = len(res.Facts)
	}
	span.SetAttributes(ragtypes.Attr(AttrFacts, facts))
	if errors.Is(err, types.ErrPartialSearch) {
		span.SetAttributes(ragtypes.Attr(ragtypes.AttrPartial, true))
	}
	end(span, err)
	o.observer.RecordRetrieval(ctx, ragtypes.RetrievalRecord{
		Retriever: "knowledge", Hits: facts, Duration: elapsed, Err: err,
	})
	return res, err
}

func (o *observedGraph) DeleteEpisodes(ctx context.Context, groupID string) error {
	d, ok := o.Graph.(ragtypes.GraphEpisodeDeleter)
	if !ok {
		return errDeleteUnsupported
	}
	ctx, span := o.observer.StartSpan(ctx, SpanDelete)
	err := d.DeleteEpisodes(ctx, groupID)
	end(span, err)
	return err
}

func (o *observedGraph) DeleteDocumentEpisodes(ctx context.Context, groupID, documentID string) error {
	d, ok := o.Graph.(ragtypes.GraphDocumentDeleter)
	if !ok {
		return errDeleteUnsupported
	}
	ctx, span := o.observer.StartSpan(ctx, SpanDelete, ragtypes.Attr(ragtypes.AttrDocumentUUID, documentID))
	err := d.DeleteDocumentEpisodes(ctx, groupID, documentID)
	end(span, err)
	return err
}

// SupportsDocumentDeletion reports whether the wrapped graph can delete by
// document.
func (o *observedGraph) SupportsDocumentDeletion() bool {
	if _, ok := o.Graph.(ragtypes.GraphDocumentDeleter); !ok {
		return false
	}
	if r, ok := o.Graph.(ragtypes.GraphDocumentDeletionReporter); ok {
		return r.SupportsDocumentDeletion()
	}
	return true
}

// GetGraphInGroup forwards types.GroupScopedGraphReader so wrapping never
// downgrades a scoped read to an unscoped one.
func (o *observedGraph) GetGraphInGroup(ctx context.Context, groupID string, limit int64) (*types.GraphData, error) {
	gs, ok := o.Graph.(types.GroupScopedGraphReader)
	if !ok {
		return nil, types.ErrGroupScopeUnsupported
	}
	return gs.GetGraphInGroup(ctx, groupID, limit)
}

// GetNodeInGroup forwards types.GroupScopedGraphReader.
func (o *observedGraph) GetNodeInGroup(ctx context.Context, groupID, id string, depth int) (*types.NodeDetail, error) {
	gs, ok := o.Graph.(types.GroupScopedGraphReader)
	if !ok {
		return nil, types.ErrGroupScopeUnsupported
	}
	return gs.GetNodeInGroup(ctx, groupID, id, depth)
}

func end(span ragtypes.Span, err error) {
	if err != nil {
		span.RecordError(err)
	}
	span.End()
}
