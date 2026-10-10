package batch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// FileStore is a Store with one JSON file per job in a directory. Each write
// goes to a temporary file that is synced and renamed into place, so a crash
// never leaves a partial record. It is meant for one process at a time on
// one machine; use postgres.BatchStore to share jobs between processes.
type FileStore struct {
	dir string
	mu  sync.Mutex
}

var _ Store = (*FileStore)(nil)

// NewFileStore opens (creating if needed) a file store in dir.
func NewFileStore(dir string) (*FileStore, error) {
	if dir == "" {
		return nil, errors.New("batch: file store needs a directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("batch: file store: %w", err)
	}
	return &FileStore{dir: dir}, nil
}

func (s *FileStore) path(id string) string {
	sum := sha256.Sum256([]byte(id))
	return filepath.Join(s.dir, hex.EncodeToString(sum[:16])+".json")
}

func (s *FileStore) read(id string) (Job, error) {
	raw, err := os.ReadFile(s.path(id))
	if errors.Is(err, fs.ErrNotExist) {
		return Job{}, ErrJobNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("batch: file store: %w", err)
	}
	var j Job
	if err := json.Unmarshal(raw, &j); err != nil {
		return Job{}, fmt.Errorf("batch: file store: decode %s: %w", id, err)
	}
	if j.ID != id {
		return Job{}, fmt.Errorf("batch: file store: %s holds job %q", s.path(id), j.ID)
	}
	return j, nil
}

func (s *FileStore) write(j Job) error {
	raw, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, ".job-*")
	if err != nil {
		return fmt.Errorf("batch: file store: %w", err)
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		return fmt.Errorf("batch: file store: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("batch: file store: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("batch: file store: %w", err)
	}
	if err := os.Rename(f.Name(), s.path(j.ID)); err != nil {
		return fmt.Errorf("batch: file store: %w", err)
	}
	if d, err := os.Open(s.dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// Create implements Store.
func (s *FileStore) Create(_ context.Context, j Job) (Job, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.read(j.ID)
	switch {
	case err == nil:
		return cur, false, nil
	case !errors.Is(err, ErrJobNotFound):
		return Job{}, false, err
	}
	j.Version = 1
	if err := s.write(j); err != nil {
		return Job{}, false, err
	}
	return j.clone(), true, nil
}

// Get implements Store.
func (s *FileStore) Get(_ context.Context, id string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.read(id)
}

// Update implements Store.
func (s *FileStore) Update(_ context.Context, j Job) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.read(j.ID)
	if err != nil {
		return Job{}, err
	}
	if cur.Version != j.Version {
		return Job{}, ErrJobConflict
	}
	j.Version++
	if err := s.write(j); err != nil {
		return Job{}, err
	}
	return j.clone(), nil
}

// List implements Store.
func (s *FileStore) List(_ context.Context, states ...JobState) ([]Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("batch: file store: %w", err)
	}
	var out []Job
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("batch: file store: %w", err)
		}
		var j Job
		if err := json.Unmarshal(raw, &j); err != nil {
			return nil, fmt.Errorf("batch: file store: decode %s: %w", e.Name(), err)
		}
		if len(states) == 0 || slices.Contains(states, j.State) {
			out = append(out, j)
		}
	}
	sortJobs(out)
	return out, nil
}
