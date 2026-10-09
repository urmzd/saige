package types

import (
	"context"
	"time"
)

// SourceDocument is the identity of a stored document as a source sees it:
// enough to decide whether fetched content is new, changed, or gone without
// loading the document's sections.
type SourceDocument struct {
	UUID             string
	Scope            string
	SourceURI        string
	Fingerprint      string
	SourceModifiedAt time.Time
	UpdatedAt        time.Time
}

// SourceFinder is an optional Store interface that finds documents by source
// URI within one scope. It returns every match, most recently updated first,
// and an empty slice (not an error) when none exists.
type SourceFinder interface {
	FindBySourceURI(ctx context.Context, scope, sourceURI string) ([]SourceDocument, error)
}

// SourceLister is an optional Store interface that lists the documents of
// one scope whose source URI starts with uriPrefix (every document of the
// scope when uriPrefix is empty), ordered by source URI and then most
// recently updated first. SyncSource needs it.
type SourceLister interface {
	ListSourceDocuments(ctx context.Context, scope, uriPrefix string) ([]SourceDocument, error)
}

// SyncOptions configures a source sync.
type SyncOptions struct {
	// Scope is the scope documents are synced into. Empty means the
	// pipeline's scope, or the default scope.
	Scope string
	// Since is the cursor returned by the previous sync (SyncResult.Cursor).
	// A fetched document whose SourceModifiedAt is known and not after Since
	// is counted as unchanged without hashing or re-ingesting it, provided a
	// document for its URI already exists. Zero compares every document.
	Since time.Time
	// Prune deletes documents of the scope whose source URI starts with
	// PrunePrefix but was not returned by the source, and older duplicates
	// of a URI that the source did return. Without it, nothing is deleted.
	Prune       bool
	PrunePrefix string
}

// SyncError is the failure of one source URI during a sync.
type SyncError struct {
	SourceURI string
	Err       error
}

func (e *SyncError) Error() string { return e.SourceURI + ": " + e.Err.Error() }
func (e *SyncError) Unwrap() error { return e.Err }

// SyncResult reports what a sync changed. Each list holds document UUIDs.
type SyncResult struct {
	Created   []string
	Updated   []string
	Unchanged []string
	Pruned    []string
	// Failed lists the URIs whose ingest, update, or prune failed. A URI
	// that was written but failed a later stage (ErrPartialIngest) appears
	// both here and in Created or Updated.
	Failed []SyncError
	// Cursor is the latest SourceModifiedAt among documents this sync
	// created, updated, or found unchanged, or Since when nothing newer was
	// seen. When an ingest or update failed, Cursor is held just before the
	// earliest failed document's SourceModifiedAt, so passing it as Since to
	// the next sync retries that document. Pass it as Since to the next sync.
	Cursor time.Time
}

// SourceSyncer is implemented by pipelines that can reconcile a Source with
// the store by source URI: new URIs are ingested, changed content replaces
// the URI's document in place (keeping its UUID), unchanged content is left
// alone, and with Prune, URIs the source no longer returns are deleted. A
// new URI whose content another URI already holds is not ingested again,
// whatever the dedup behavior: it is reported as Unchanged with the existing
// document's UUID, so the sync settles instead of moving that document
// between the two URIs.
//
// It returns a non-nil result whenever the fetch succeeded, together with
// an error joining every SyncError when some URIs failed.
type SourceSyncer interface {
	SyncSource(ctx context.Context, src Source, opts SyncOptions) (*SyncResult, error)
}
