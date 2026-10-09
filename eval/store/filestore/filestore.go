// Package filestore is an eval/store.Store backed by a directory of files.
//
// Each run lives in its own subdirectory named by the run ID:
//
//	<root>/<run-id>/run.json       the run record
//	<root>/<run-id>/units.jsonl    one unit per line; the last line per key is current
//	<root>/<run-id>/attempts.jsonl prior attempts archived before a replacement
//
// run.json is written to a temporary file, synced, and renamed into place,
// so a crash leaves the old record or the new one, never a partial file.
// Unit lines are appended and synced one at a time. A crash during an append
// can leave a partial last line; it is ignored on read and cut off before
// the next append, so the files stay readable as JSON Lines.
//
// A directory supports one writing process at a time. Other processes may
// read it concurrently.
package filestore

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/urmzd/saige/eval"
	"github.com/urmzd/saige/eval/store"
)

const (
	runFile      = "run.json"
	unitsFile    = "units.jsonl"
	attemptsFile = "attempts.jsonl"
)

// maxLine bounds one JSON line, which holds one unit with its observation.
const maxLine = 64 << 20

// Store persists runs under a root directory.
type Store struct {
	root string

	mu sync.Mutex
	// index caches, per run, the current attempt of each key so PutUnit
	// does not reread the whole unit log. It is rebuilt when the log's
	// size no longer matches what this process last wrote.
	index map[string]*unitIndex
}

type unitIndex struct {
	size    int64
	attempt map[string]int
	current map[string][]byte
}

var _ store.Store = (*Store)(nil)

// Open returns a store rooted at dir, creating the directory if needed.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("filestore: create %s: %w", dir, err)
	}
	return &Store{root: dir, index: map[string]*unitIndex{}}, nil
}

// Root returns the store's root directory.
func (s *Store) Root() string { return s.root }

func (s *Store) runDir(id string) string { return filepath.Join(s.root, id) }

// CreateRun implements store.Store.
func (s *Store) CreateRun(_ context.Context, run eval.RunRecord) error {
	if err := store.ValidateRunID(run.ID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := s.runDir(run.ID)
	if err := os.Mkdir(dir, 0o750); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%w: %q", store.ErrRunExists, run.ID)
		}
		return fmt.Errorf("filestore: create run %q: %w", run.ID, err)
	}
	if err := writeFileAtomic(filepath.Join(dir, runFile), run); err != nil {
		_ = os.RemoveAll(dir)
		return err
	}
	return syncDir(s.root)
}

// UpdateRun implements store.Store.
func (s *Store) UpdateRun(_ context.Context, run eval.RunRecord) error {
	if err := store.ValidateRunID(run.ID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.runDir(run.ID), runFile)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: run %q", store.ErrNotFound, run.ID)
		}
		return fmt.Errorf("filestore: stat run %q: %w", run.ID, err)
	}
	return writeFileAtomic(path, run)
}

// GetRun implements store.Store.
func (s *Store) GetRun(_ context.Context, id string) (eval.RunRecord, error) {
	if err := store.ValidateRunID(id); err != nil {
		return eval.RunRecord{}, fmt.Errorf("%w: run %q", store.ErrNotFound, id)
	}
	return readRun(filepath.Join(s.runDir(id), runFile), id)
}

func readRun(path, id string) (eval.RunRecord, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return eval.RunRecord{}, fmt.Errorf("%w: run %q", store.ErrNotFound, id)
		}
		return eval.RunRecord{}, fmt.Errorf("filestore: read run %q: %w", id, err)
	}
	var run eval.RunRecord
	if err := json.Unmarshal(data, &run); err != nil {
		return eval.RunRecord{}, fmt.Errorf("filestore: decode run %q: %w", id, err)
	}
	return run, nil
}

// ListRuns implements store.Store. Subdirectories without a run.json, such
// as one left by a crash inside CreateRun, are skipped.
func (s *Store) ListRuns(_ context.Context, filter store.RunFilter) ([]eval.RunRecord, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, fmt.Errorf("filestore: list %s: %w", s.root, err)
	}
	var runs []eval.RunRecord
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		run, err := readRun(filepath.Join(s.root, e.Name(), runFile), e.Name())
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if filter.Match(run) {
			runs = append(runs, run)
		}
	}
	if runs == nil {
		runs = []eval.RunRecord{}
	}
	return store.SortRuns(runs, filter.Limit), nil
}

// PutUnit implements store.Store.
func (s *Store) PutUnit(_ context.Context, unit eval.Unit) (eval.Unit, error) {
	if err := store.ValidateRunID(unit.RunID); err != nil {
		return eval.Unit{}, fmt.Errorf("%w: run %q", store.ErrNotFound, unit.RunID)
	}
	if unit.Key == "" {
		unit.Key = eval.UnitKey(unit.Observation)
	}
	if unit.RecordedAt.IsZero() {
		unit.RecordedAt = time.Now().UTC()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	dir := s.runDir(unit.RunID)
	if _, err := os.Stat(filepath.Join(dir, runFile)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return eval.Unit{}, fmt.Errorf("%w: run %q", store.ErrNotFound, unit.RunID)
		}
		return eval.Unit{}, fmt.Errorf("filestore: stat run %q: %w", unit.RunID, err)
	}
	idx, err := s.loadIndex(unit.RunID)
	if err != nil {
		return eval.Unit{}, err
	}

	unit.Attempt = idx.attempt[unit.Key] + 1
	if prior, ok := idx.current[unit.Key]; ok {
		if _, err := appendLine(filepath.Join(dir, attemptsFile), prior); err != nil {
			return eval.Unit{}, err
		}
	}
	line, err := json.Marshal(unit)
	if err != nil {
		return eval.Unit{}, fmt.Errorf("filestore: encode unit: %w", err)
	}
	size, err := appendLine(filepath.Join(dir, unitsFile), line)
	if err != nil {
		delete(s.index, unit.RunID)
		return eval.Unit{}, err
	}
	idx.size = size
	idx.attempt[unit.Key] = unit.Attempt
	idx.current[unit.Key] = line

	var out eval.Unit
	if err := json.Unmarshal(line, &out); err != nil {
		return eval.Unit{}, fmt.Errorf("filestore: decode unit: %w", err)
	}
	return out, nil
}

// loadIndex returns the cached index for a run, rebuilding it from the unit
// log when the log changed size since this process last wrote it.
func (s *Store) loadIndex(runID string) (*unitIndex, error) {
	path := filepath.Join(s.runDir(runID), unitsFile)
	size := int64(0)
	if info, err := os.Stat(path); err == nil {
		size = info.Size()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("filestore: stat units: %w", err)
	}
	if idx, ok := s.index[runID]; ok && idx.size == size {
		return idx, nil
	}
	idx := &unitIndex{attempt: map[string]int{}, current: map[string][]byte{}}
	err := readLines(path, func(line []byte) error {
		var u eval.Unit
		if err := json.Unmarshal(line, &u); err != nil {
			return err
		}
		idx.attempt[u.Key] = max(idx.attempt[u.Key], u.Attempt)
		idx.current[u.Key] = append([]byte(nil), line...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	idx.size = size
	s.index[runID] = idx
	return idx, nil
}

// Units implements store.Store. It reads the unit log on every call, so it
// sees units appended by the writing process.
func (s *Store) Units(_ context.Context, runID string, filter store.UnitFilter) ([]eval.Unit, error) {
	if err := s.requireRun(runID); err != nil {
		return nil, err
	}
	units, order, err := readUnits(filepath.Join(s.runDir(runID), unitsFile))
	if err != nil {
		return nil, err
	}
	out := make([]eval.Unit, 0, len(order))
	for _, key := range order {
		if u := units[key]; filter.Match(u) {
			out = append(out, u)
		}
	}
	return out, nil
}

// Attempts implements store.Store. An archived attempt is listed only when
// it is older than the current attempt, so an attempt archived just before a
// crash that kept the replacement from being written is not shown twice.
func (s *Store) Attempts(_ context.Context, runID, key string) ([]eval.Unit, error) {
	if err := s.requireRun(runID); err != nil {
		return nil, err
	}
	dir := s.runDir(runID)
	current, _, err := readUnits(filepath.Join(dir, unitsFile))
	if err != nil {
		return nil, err
	}
	cur, ok := current[key]
	if !ok {
		return []eval.Unit{}, nil
	}
	seen := map[int]int{}
	var out []eval.Unit
	err = readLines(filepath.Join(dir, attemptsFile), func(line []byte) error {
		var u eval.Unit
		if err := json.Unmarshal(line, &u); err != nil {
			return err
		}
		if u.Key != key || u.Attempt >= cur.Attempt {
			return nil
		}
		if i, dup := seen[u.Attempt]; dup {
			out[i] = u
			return nil
		}
		seen[u.Attempt] = len(out)
		out = append(out, u)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []eval.Unit{}
	}
	return out, nil
}

func (s *Store) requireRun(runID string) error {
	if err := store.ValidateRunID(runID); err != nil {
		return fmt.Errorf("%w: run %q", store.ErrNotFound, runID)
	}
	if _, err := os.Stat(filepath.Join(s.runDir(runID), runFile)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: run %q", store.ErrNotFound, runID)
		}
		return fmt.Errorf("filestore: stat run %q: %w", runID, err)
	}
	return nil
}

// readUnits returns the last record per key and the keys in first-seen
// order.
func readUnits(path string) (map[string]eval.Unit, []string, error) {
	units := map[string]eval.Unit{}
	var order []string
	err := readLines(path, func(line []byte) error {
		var u eval.Unit
		if err := json.Unmarshal(line, &u); err != nil {
			return err
		}
		if _, seen := units[u.Key]; !seen {
			order = append(order, u.Key)
		}
		units[u.Key] = u
		return nil
	})
	return units, order, err
}

// readLines calls fn for each complete line of a JSON Lines file. A missing
// file has no lines. A final line without a newline is a partial append and
// is skipped; any other line that fn rejects is an error.
func readLines(path string, fn func([]byte) error) error {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("filestore: open %s: %w", filepath.Base(path), err)
	}
	defer func() { _ = f.Close() }()

	r := bufio.NewReaderSize(f, 1<<16)
	for n := 1; ; n++ {
		line, err := readLine(r)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil // partial last line from an interrupted append
		}
		if err != nil {
			return fmt.Errorf("filestore: read %s line %d: %w", filepath.Base(path), n, err)
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if err := fn(line); err != nil {
			return fmt.Errorf("filestore: decode %s line %d: %w", filepath.Base(path), n, err)
		}
	}
}

// readLine returns one newline-terminated line without the newline. It
// returns io.EOF at a clean end and io.ErrUnexpectedEOF for trailing bytes
// with no newline.
func readLine(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > maxLine {
			return nil, fmt.Errorf("line longer than %d bytes", maxLine)
		}
		switch {
		case err == nil:
			return buf[:len(buf)-1], nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if len(buf) == 0 {
				return nil, io.EOF
			}
			return nil, io.ErrUnexpectedEOF
		default:
			return nil, err
		}
	}
}

// appendLine appends data and a newline to path and syncs the file,
// first cutting off a partial last line left by an interrupted append. It
// returns the file size after the append.
func appendLine(path string, data []byte) (int64, error) {
	f, err := os.OpenFile(filepath.Clean(path), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return 0, fmt.Errorf("filestore: open %s: %w", filepath.Base(path), err)
	}
	defer func() { _ = f.Close() }()

	end, err := completeEnd(f)
	if err != nil {
		return 0, fmt.Errorf("filestore: scan %s: %w", filepath.Base(path), err)
	}
	if err := f.Truncate(end); err != nil {
		return 0, fmt.Errorf("filestore: truncate %s: %w", filepath.Base(path), err)
	}
	line := make([]byte, 0, len(data)+1)
	line = append(line, data...)
	line = append(line, '\n')
	if _, err := f.WriteAt(line, end); err != nil {
		return 0, fmt.Errorf("filestore: append %s: %w", filepath.Base(path), err)
	}
	if err := f.Sync(); err != nil {
		return 0, fmt.Errorf("filestore: sync %s: %w", filepath.Base(path), err)
	}
	return end + int64(len(line)), nil
}

// completeEnd returns the offset just past the last newline in f, which is
// where the next record must start.
func completeEnd(f *os.File) (int64, error) {
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	size := info.Size()
	const block = 4096
	buf := make([]byte, block)
	for end := size; end > 0; {
		start := max(0, end-block)
		n, err := f.ReadAt(buf[:end-start], start)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			return start + int64(i) + 1, nil
		}
		end = start
	}
	return 0, nil
}

// writeFileAtomic writes v as indented JSON to a temporary file beside path,
// syncs it, and renames it over path.
func writeFileAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("filestore: encode %s: %w", filepath.Base(path), err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("filestore: create temp: %w", err)
	}
	tmpPath := tmp.Name()
	fail := func(step string, err error) error {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("filestore: %s %s: %w", step, filepath.Base(path), err)
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		return fail("write", err)
	}
	if err := tmp.Sync(); err != nil {
		return fail("sync", err)
	}
	if err := tmp.Close(); err != nil {
		return fail("close", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("filestore: rename %s: %w", filepath.Base(path), err)
	}
	return syncDir(dir)
}

// syncDir makes a rename or create in dir durable. Platforms that cannot
// sync a directory report an error that is ignored.
func syncDir(dir string) error {
	d, err := os.Open(filepath.Clean(dir))
	if err != nil {
		return nil
	}
	_ = d.Sync()
	return d.Close()
}
