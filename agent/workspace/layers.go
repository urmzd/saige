package workspace

import (
	"context"
	"errors"
	"sync"
)

// Layers is a Workspace made of a writable top layer over read layers. Put
// writes to the top layer only. Stat and Read look in the top layer first and
// then in each read layer in order, so a name in the top layer shadows the
// same name below it. List and Search cover every layer.
//
// A sub-agent's default view is a Layers of its private scratch over a
// read-only view of its parent's workspace: it writes its own notes and still
// reads what the parent shared. A parent run uses one to read the scratch of
// the children it delegated to. Layers is safe for concurrent use, and
// Attach may add read layers while other calls run.
type Layers struct {
	top Workspace

	mu    sync.RWMutex
	reads []Workspace
}

var _ Workspace = (*Layers)(nil)

// NewLayers returns a Layers that writes to top and reads from top and then
// reads, in order. A nil top makes every Put fail with ErrReadOnly. Nil read
// layers are skipped.
func NewLayers(top Workspace, reads ...Workspace) *Layers {
	l := &Layers{top: top}
	for _, r := range reads {
		l.Attach(r)
	}
	return l
}

// Attach adds a read layer below the existing ones. A nil ws is ignored.
func (l *Layers) Attach(ws Workspace) {
	if ws == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reads = append(l.reads, ws)
}

// Top returns the writable layer, or nil.
func (l *Layers) Top() Workspace { return l.top }

// layers returns every layer in lookup order.
func (l *Layers) layers() []Workspace {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]Workspace, 0, len(l.reads)+1)
	if l.top != nil {
		out = append(out, l.top)
	}
	return append(out, l.reads...)
}

// Put implements Workspace.
func (l *Layers) Put(ctx context.Context, name string, data []byte, meta map[string]string) (Ref, error) {
	if l.top == nil {
		return Ref{}, ErrReadOnly
	}
	return l.top.Put(ctx, name, data, meta)
}

// find returns the first layer that holds ref, with ref resolved there.
func (l *Layers) find(ctx context.Context, ref Ref) (Workspace, Ref, error) {
	notFound := error(nil)
	for _, w := range l.layers() {
		r, err := w.Stat(ctx, ref)
		if err == nil {
			return w, r, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return nil, Ref{}, err
		}
		notFound = err
	}
	if notFound == nil {
		notFound = ErrNotFound
	}
	return nil, Ref{}, notFound
}

// Stat implements Workspace.
func (l *Layers) Stat(ctx context.Context, ref Ref) (Ref, error) {
	_, r, err := l.find(ctx, ref)
	return r, err
}

// Read implements Workspace.
func (l *Layers) Read(ctx context.Context, ref Ref, offset, limit int64) ([]byte, error) {
	w, r, err := l.find(ctx, ref)
	if err != nil {
		return nil, err
	}
	return w.Read(ctx, r, offset, limit)
}

// List implements Workspace. A name held by several layers is listed once,
// as the layer Read would use.
func (l *Layers) List(ctx context.Context) ([]Ref, error) {
	seen := map[string]bool{}
	var out []Ref
	for _, w := range l.layers() {
		refs, err := w.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, r := range refs {
			if !seen[r.Name] {
				seen[r.Name] = true
				out = append(out, r)
			}
		}
	}
	sortRefs(out)
	return out, nil
}

// Search implements Workspace. It returns the top layer's hits first, then
// each read layer's, up to k in all.
func (l *Layers) Search(ctx context.Context, query string, k int) ([]Hit, error) {
	if k <= 0 {
		k = 10
	}
	var hits []Hit
	for _, w := range l.layers() {
		found, err := w.Search(ctx, query, k-len(hits))
		if err != nil {
			return nil, err
		}
		hits = append(hits, found...)
		if len(hits) >= k {
			return hits[:k], nil
		}
	}
	return hits, nil
}

// View implements Workspace.
func (l *Layers) View(readOnly bool) Workspace { return view(l, readOnly) }
