package types

import (
	"context"
	"strings"
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
	// A fetched document that carries its bytes is always compared by
	// fingerprint, because a modification time can move backwards (cp -p,
	// rsync -a, tar x). Only a document without bytes (nil Data) whose
	// SourceModifiedAt is known and not after Since is counted as unchanged
	// on its time alone, provided a document for its URI already exists.
	Since time.Time
	// Prune deletes documents of the scope whose source URI starts with
	// PrunePrefix but was not returned by the source, and older duplicates
	// of a URI that the source did return. A URI the source reported as
	// skipped (see FilteringSource) is not pruned. Without it, nothing is
	// deleted.
	Prune       bool
	PrunePrefix string
}

// SkippedSource is a URI that a source deliberately left out of a fetch,
// for example because an exclude rule matched it. With Prefix, it stands for
// every URI that starts with SourceURI, such as the files of a skipped
// directory, which the source did not list.
type SkippedSource struct {
	SourceURI string `json:"source_uri"`
	Prefix    bool   `json:"prefix,omitempty"`
	// Reason names the rule that skipped it.
	Reason string `json:"reason"`
}

// Covers reports whether uri is the skipped URI or, with Prefix, under it.
func (s SkippedSource) Covers(uri string) bool {
	if s.Prefix {
		return strings.HasPrefix(uri, s.SourceURI)
	}
	return uri == s.SourceURI
}

// FilteringSource is an optional Source interface for sources that leave
// items out by rule. SyncSource calls FetchWithSkips instead of Fetch, so
// that skipped items are reported in SyncResult.Skipped and never pruned as
// absent: a changed rule is not evidence that the item is gone.
type FilteringSource interface {
	Source
	FetchWithSkips(ctx context.Context) ([]RawDocument, []SkippedSource, error)
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
	// Skipped lists what a FilteringSource left out by rule. Stored
	// documents under these URIs are kept, even with Prune.
	Skipped []SkippedSource
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
