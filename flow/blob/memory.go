package blob

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
)

// Memory is a Store held in process memory. It is a complete implementation
// of the contract, not a stub: the single-binary deployment and the tests use
// it, and it passes the same suite the filesystem store does.
//
// Its contents do not survive a restart. That matches the in-memory run
// ledger it is deployed beside: a run that cannot be replayed after a restart
// has no use for its blobs either.
type Memory struct {
	mu      sync.RWMutex
	tenants map[string]map[Digest][]byte
	refs    map[string]map[string]Digest
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{tenants: make(map[string]map[Digest][]byte), refs: make(map[string]map[string]Digest)}
}

var _ Store = (*Memory)(nil)

// Put implements Store.
func (m *Memory) Put(ctx context.Context, tenant string, r io.Reader, opts PutOptions) (Info, error) {
	if err := checkTenant(ctx, tenant); err != nil {
		return Info{}, err
	}
	// The content is buffered outside the lock: a slow uploader must not
	// stall every reader of the store.
	var buf bytes.Buffer
	info, err := ingest(ctx, &buf, r, opts)
	if err != nil {
		return Info{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	blobs := m.tenants[tenant]
	if blobs == nil {
		blobs = make(map[Digest][]byte)
		m.tenants[tenant] = blobs
	}
	if _, ok := blobs[info.Digest]; !ok {
		blobs[info.Digest] = buf.Bytes()
	}
	return info, nil
}

// Get implements Store. The stored slice is never written after Put, so
// readers share it without copying.
func (m *Memory) Get(ctx context.Context, tenant string, d Digest) (io.ReadCloser, Info, error) {
	if err := check(ctx, tenant, d); err != nil {
		return nil, Info{}, err
	}
	m.mu.RLock()
	b, ok := m.tenants[tenant][d]
	m.mu.RUnlock()
	if !ok {
		return nil, Info{}, notFound(d)
	}
	return io.NopCloser(bytes.NewReader(b)), Info{Digest: d, Size: int64(len(b))}, nil
}

// Stat implements Store.
func (m *Memory) Stat(ctx context.Context, tenant string, d Digest) (Info, error) {
	if err := check(ctx, tenant, d); err != nil {
		return Info{}, err
	}
	m.mu.RLock()
	b, ok := m.tenants[tenant][d]
	m.mu.RUnlock()
	if !ok {
		return Info{}, notFound(d)
	}
	return Info{Digest: d, Size: int64(len(b))}, nil
}

// Delete implements Store.
func (m *Memory) Delete(ctx context.Context, tenant string, d Digest) error {
	if err := check(ctx, tenant, d); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if blobs := m.tenants[tenant]; blobs != nil {
		delete(blobs, d)
		if len(blobs) == 0 {
			delete(m.tenants, tenant)
		}
	}
	return nil
}

// PutRef implements Store.
func (m *Memory) PutRef(ctx context.Context, tenant, key string, d Digest) (Digest, error) {
	if err := checkRef(ctx, tenant, key); err != nil {
		return "", err
	}
	if err := d.Validate(); err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	blobs := m.tenants[tenant]
	if _, ok := blobs[d]; !ok {
		return "", notFound(d)
	}
	if cur, ok := m.refs[tenant][key]; ok {
		if _, live := blobs[cur]; live {
			return cur, nil
		}
	}
	if m.refs[tenant] == nil {
		m.refs[tenant] = make(map[string]Digest)
	}
	m.refs[tenant][key] = d
	return d, nil
}

// Ref implements Store.
func (m *Memory) Ref(ctx context.Context, tenant, key string) (Digest, error) {
	if err := checkRef(ctx, tenant, key); err != nil {
		return "", err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	d, ok := m.refs[tenant][key]
	if !ok {
		return "", fmt.Errorf("%w: reference %q", ErrNotFound, key)
	}
	return d, nil
}

// DeleteRef implements Store.
func (m *Memory) DeleteRef(ctx context.Context, tenant, key string) error {
	if err := checkRef(ctx, tenant, key); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if refs := m.refs[tenant]; refs != nil {
		delete(refs, key)
		if len(refs) == 0 {
			delete(m.refs, tenant)
		}
	}
	return nil
}

func notFound(d Digest) error { return fmt.Errorf("%w: %s", ErrNotFound, d) }
