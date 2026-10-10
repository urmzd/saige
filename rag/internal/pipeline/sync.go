package pipeline

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	ragtypes "github.com/urmzd/saige/rag/types"
)

// SyncSource reconciles src with the store by source URI; see
// ragtypes.SourceSyncer. The store must implement ragtypes.SourceLister.
//
// For each fetched document: a URI with no stored document is ingested, or
// recorded as unchanged when another URI already holds identical content; a
// URI whose newest document has the same fingerprint, or whose fetched
// document carries no bytes and a SourceModifiedAt not after opts.Since, is
// unchanged; any other URI is updated in place through Update, so its
// document UUID survives. With opts.Prune, stored URIs under
// opts.PrunePrefix that the source did not return are deleted, as are older
// duplicates of a returned URI. A URI the source returned is never pruned,
// even when it was rejected, and neither is one a ragtypes.FilteringSource
// reported as skipped. The cursor moves only past documents that were
// written or found unchanged; see ragtypes.SyncResult.Cursor.
//
//nolint:gocyclo // a single pass over a stream or loop state; splitting it would scatter shared state
func (p *pipelineImpl) SyncSource(ctx context.Context, src ragtypes.Source, opts ragtypes.SyncOptions) (result *ragtypes.SyncResult, err error) {
	scope, err := p.scopeFor(opts.Scope)
	if err != nil {
		return nil, err
	}
	lister, ok := p.cfg.Store.(ragtypes.SourceLister)
	if !ok {
		return nil, ragtypes.ErrSyncUnsupported
	}
	ctx, span := p.cfg.Observer.StartSpan(ctx, ragtypes.SpanSync, ragtypes.Attr(ragtypes.AttrScope, scope))
	defer func() { endSpan(span, err) }()

	raws, skipped, err := fetchSource(ctx, src)
	if err != nil {
		return nil, fmt.Errorf("fetch source: %w", err)
	}
	stored, err := lister.ListSourceDocuments(ctx, scope, "")
	if err != nil {
		return nil, fmt.Errorf("list source documents: %w", err)
	}
	byURI := make(map[string][]ragtypes.SourceDocument)
	for _, sd := range stored {
		byURI[sd.SourceURI] = append(byURI[sd.SourceURI], sd)
	}
	for _, docs := range byURI {
		sort.SliceStable(docs, func(i, j int) bool { return docs[i].UpdatedAt.After(docs[j].UpdatedAt) })
	}

	result = &ragtypes.SyncResult{Cursor: opts.Since, Skipped: skipped}
	fail := func(uri string, err error) {
		result.Failed = append(result.Failed, ragtypes.SyncError{SourceURI: uri, Err: err})
	}
	seen := make(map[string]bool, len(raws))
	// done is the latest SourceModifiedAt of a URI that was written or found
	// unchanged; failed is the earliest of a URI whose write failed.
	var done, failed time.Time
	advance := func(raw *ragtypes.RawDocument, ok bool) {
		t := raw.SourceModifiedAt
		switch {
		case t.IsZero():
		case ok && t.After(done):
			done = t
		case !ok && (failed.IsZero() || t.Before(failed)):
			failed = t
		}
	}
	for i := range raws {
		if err := ctx.Err(); err != nil {
			return result, errors.Join(err, syncErrors(result))
		}
		raw := raws[i]
		uri := raw.SourceURI
		switch {
		case uri == "":
			fail(uri, errors.New("document has no source URI"))
			continue
		case seen[uri]:
			fail(uri, errors.New("source returned the URI more than once"))
			continue
		}
		// The source still has this URI, so it is not a prune candidate even
		// if the document is rejected below.
		seen[uri] = true
		if raw.Scope != "" && raw.Scope != scope {
			fail(uri, fmt.Errorf("%w: document names scope %q, sync is for %q", ragtypes.ErrScopeMismatch, raw.Scope, scope))
			continue
		}
		raw.Scope = scope

		docs := byURI[uri]
		if len(docs) == 0 {
			// No document carries this URI, so a fingerprint match belongs to
			// another URI with the same content. Replacing it would move that
			// document to this URI and leave the other URI without one, which
			// the next sync would then undo. Skip instead: the URI is recorded
			// as unchanged with the existing document.
			res, err := p.ingest(ctx, &raw, ragtypes.DedupSkip)
			advance(&raw, p.recordWrite(result, uri, res, err, false))
			continue
		}
		current := docs[0]
		if opts.Prune {
			for _, old := range docs[1:] {
				p.prune(ctx, result, old)
			}
		}
		// With the bytes in hand the fingerprint decides: a modification
		// time can move backwards (cp -p, rsync -a), so it only stands in
		// for the bytes when the source sent none.
		var unchanged bool
		if raw.Data == nil {
			unchanged = !opts.Since.IsZero() && !raw.SourceModifiedAt.IsZero() && !raw.SourceModifiedAt.After(opts.Since)
		} else {
			unchanged = current.Fingerprint == ragtypes.Fingerprint(scope, raw.Data)
		}
		if unchanged {
			result.Unchanged = append(result.Unchanged, current.UUID)
			advance(&raw, true)
			continue
		}
		res, err := p.Update(ctx, current.UUID, &raw)
		advance(&raw, p.recordWrite(result, uri, res, err, true))
	}
	result.Cursor = syncCursor(opts.Since, done, failed)

	if opts.Prune {
		uris := make([]string, 0, len(byURI))
		for uri := range byURI {
			if !seen[uri] && strings.HasPrefix(uri, opts.PrunePrefix) && !isSkipped(skipped, uri) {
				uris = append(uris, uri)
			}
		}
		sort.Strings(uris)
		for _, uri := range uris {
			for _, doc := range byURI[uri] {
				p.prune(ctx, result, doc)
			}
		}
	}
	return result, syncErrors(result)
}

// fetchSource fetches src, together with what it skipped by rule when it is
// a ragtypes.FilteringSource.
func fetchSource(ctx context.Context, src ragtypes.Source) ([]ragtypes.RawDocument, []ragtypes.SkippedSource, error) {
	if fs, ok := src.(ragtypes.FilteringSource); ok {
		return fs.FetchWithSkips(ctx)
	}
	raws, err := src.Fetch(ctx)
	return raws, nil, err
}

// isSkipped reports whether the source skipped uri by rule.
func isSkipped(skipped []ragtypes.SkippedSource, uri string) bool {
	for _, s := range skipped {
		if s.Covers(uri) {
			return true
		}
	}
	return false
}

// syncCursor returns the cursor for the next sync: the later of since and
// done, held just before failed when a write failed at or before it, so the
// next sync's unchanged check does not skip the failed document.
func syncCursor(since, done, failed time.Time) time.Time {
	cursor := since
	if done.After(cursor) {
		cursor = done
	}
	if !failed.IsZero() && !failed.After(cursor) {
		cursor = failed.Add(-time.Nanosecond)
	}
	return cursor
}

// recordWrite files the outcome of one ingest or update and reports whether
// it succeeded.
func (p *pipelineImpl) recordWrite(result *ragtypes.SyncResult, uri string, res *ragtypes.IngestResult, err error, update bool) bool {
	if err != nil {
		result.Failed = append(result.Failed, ragtypes.SyncError{SourceURI: uri, Err: err})
		if res == nil {
			return false
		}
	}
	switch {
	case res.Deduplicated:
		result.Unchanged = append(result.Unchanged, res.DocumentUUID)
	case update:
		result.Updated = append(result.Updated, res.DocumentUUID)
	default:
		result.Created = append(result.Created, res.DocumentUUID)
	}
	return err == nil
}

// prune deletes one stored document and files the outcome.
func (p *pipelineImpl) prune(ctx context.Context, result *ragtypes.SyncResult, doc ragtypes.SourceDocument) {
	if err := p.Delete(ctx, doc.UUID); err != nil {
		result.Failed = append(result.Failed, ragtypes.SyncError{SourceURI: doc.SourceURI, Err: fmt.Errorf("prune %s: %w", doc.UUID, err)})
		return
	}
	result.Pruned = append(result.Pruned, doc.UUID)
}

// syncErrors joins the failures of result, or returns nil.
func syncErrors(result *ragtypes.SyncResult) error {
	errs := make([]error, len(result.Failed))
	for i := range result.Failed {
		errs[i] = &result.Failed[i]
	}
	return errors.Join(errs...)
}
