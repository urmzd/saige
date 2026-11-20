// Package fileledger implements durable's ledger.Ledger on a local
// directory, for runs that live on one machine: a CLI, a desktop agent, a
// single-node worker. It needs a filesystem with reliable advisory locks and
// atomic rename and link, which every local Unix filesystem has.
//
// Each run is a directory named by the SHA-256 of its ID. run.json holds the
// run, replaced by an atomic rename under the directory's lock when the run
// completes. Each record is its own file under records/, named by the SHA-256
// of its key and published with link(2), which fails when the file exists:
// that is the write-once contract, safe for concurrent writers in any number
// of processes. Every file is synced before it is published, and its
// directory after.
//
// The ledger owns only the files it names (run.json, records/, ledger.lock),
// so another component, such as a file queue, can keep its own files in the
// same run directory.
package fileledger

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/urmzd/saige/durable/adapters/file/internal/fsutil"
	"github.com/urmzd/saige/durable/ledger"
	"github.com/urmzd/saige/durable/run"
)

const (
	runFile    = "run.json"
	recordsDir = "records"
	lockFile   = "ledger.lock"
)

// Ledger is a ledger.Ledger on files under Dir.
type Ledger struct {
	dir string
}

var (
	_ ledger.Ledger       = (*Ledger)(nil)
	_ ledger.RunGetter    = (*Ledger)(nil)
	_ ledger.PrefixReader = (*Ledger)(nil)
	_ ledger.Deleter      = (*Ledger)(nil)
	_ ledger.RunLister    = (*Ledger)(nil)
)

// New returns a ledger on dir, which it creates with mode 0700 if needed.
// Keep dir private to the program: run inputs and records are stored as
// written.
func New(dir string) (*Ledger, error) {
	if dir == "" {
		return nil, errors.New("fileledger: directory required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("fileledger: %w", err)
	}
	return &Ledger{dir: dir}, nil
}

// Dir is the ledger's directory.
func (l *Ledger) Dir() string { return l.dir }

// RunDir is the directory run id is stored in: Dir joined with the SHA-256 of
// id. It is exported so components sharing the directory agree on it.
func RunDir(dir, id string) string {
	sum := sha256.Sum256([]byte(id))
	return filepath.Join(dir, hex.EncodeToString(sum[:]))
}

func (l *Ledger) runDir(id string) string { return RunDir(l.dir, id) }

// storedRun is run.Run as run.json holds it.
type storedRun struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Input       []byte        `json:"input,omitempty"`
	Status      run.RunStatus `json:"status"`
	Attempt     int           `json:"attempt,omitempty"`
	MaxAttempts int           `json:"max_attempts,omitempty"`
	Output      []byte        `json:"output,omitempty"`
	Error       string        `json:"error,omitempty"`
	ParentRunID string        `json:"parent_run_id,omitempty"`
	CreatedAt   time.Time     `json:"created_at"`
	CompletedAt time.Time     `json:"completed_at,omitzero"`
}

func toStored(r run.Run) storedRun {
	return storedRun{ID: r.ID, Name: r.Name, Input: r.Input, Status: r.Status, Attempt: r.Attempt, MaxAttempts: r.MaxAttempts,
		Output: r.Output, Error: r.Error, ParentRunID: r.ParentRunID, CreatedAt: r.CreatedAt, CompletedAt: r.CompletedAt}
}

func (s storedRun) run() run.Run {
	return run.Run{ID: s.ID, Name: s.Name, Input: s.Input, Status: s.Status, Attempt: s.Attempt, MaxAttempts: s.MaxAttempts,
		Output: s.Output, Error: s.Error, ParentRunID: s.ParentRunID, CreatedAt: s.CreatedAt, CompletedAt: s.CompletedAt}
}

// storedRecord is run.Record as a record file holds it.
type storedRecord struct {
	RunID      string           `json:"run_id"`
	Key        string           `json:"key"`
	Kind       run.Kind         `json:"kind"`
	Name       string           `json:"name"`
	InputHash  string           `json:"input_hash,omitempty"`
	Codec      string           `json:"codec,omitempty"`
	Status     run.RecordStatus `json:"status"`
	Output     []byte           `json:"output,omitempty"`
	Error      string           `json:"error,omitempty"`
	Attempt    int              `json:"attempt,omitempty"`
	RecordedAt time.Time        `json:"recorded_at"`
}

func toStoredRecord(r run.Record) storedRecord {
	return storedRecord{RunID: r.RunID, Key: r.Key, Kind: r.Kind, Name: r.Name, InputHash: r.InputHash, Codec: r.Codec,
		Status: r.Status, Output: r.Output, Error: r.Error, Attempt: r.Attempt, RecordedAt: r.RecordedAt}
}

func (s storedRecord) record() run.Record {
	return run.Record{RunID: s.RunID, Key: s.Key, Kind: s.Kind, Name: s.Name, InputHash: s.InputHash, Codec: s.Codec,
		Status: s.Status, Output: s.Output, Error: s.Error, Attempt: s.Attempt, RecordedAt: s.RecordedAt}
}

// Accept implements ledger.Ledger: run.json is published with link(2), so of
// any number of concurrent accepts exactly one wins and the rest are no-ops.
func (l *Ledger) Accept(ctx context.Context, r run.Run) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.ID == "" {
		return errors.New("fileledger: accept: run ID required")
	}
	dir := l.runDir(r.ID)
	if err := os.MkdirAll(filepath.Join(dir, recordsDir), 0o700); err != nil {
		return fmt.Errorf("fileledger: accept %q: %w", r.ID, err)
	}
	raw, err := json.Marshal(toStored(r))
	if err != nil {
		return fmt.Errorf("fileledger: accept %q: %w", r.ID, err)
	}
	err = fsutil.Publish(dir, runFile, raw)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("fileledger: accept %q: %w", r.ID, err)
	}
	return nil
}

// Complete implements ledger.Ledger: a compare-and-set from non-terminal to
// res.Status under the run directory's lock, replacing run.json atomically.
func (l *Ledger) Complete(ctx context.Context, runID string, attempt int, res run.Result) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir := l.runDir(runID)
	if _, err := os.Stat(filepath.Join(dir, runFile)); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("fileledger: complete %q: %w", runID, run.ErrNotFound)
	}
	unlock, err := fsutil.Lock(filepath.Join(dir, lockFile))
	if err != nil {
		return fmt.Errorf("fileledger: complete %q: %w", runID, err)
	}
	defer unlock()
	s, err := readRun(dir)
	if err != nil {
		return fmt.Errorf("fileledger: complete %q: %w", runID, err)
	}
	if s.Status.Terminal() {
		return fmt.Errorf("fileledger: complete %q: %w", runID, run.ErrAlreadyTerminal)
	}
	s.Status, s.Output, s.Error, s.Attempt, s.CompletedAt = res.Status, res.Output, res.Error, attempt, time.Now()
	raw, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("fileledger: complete %q: %w", runID, err)
	}
	if err := fsutil.Replace(dir, runFile, raw); err != nil {
		return fmt.Errorf("fileledger: complete %q: %w", runID, err)
	}
	return nil
}

// Load implements ledger.Ledger.
func (l *Ledger) Load(ctx context.Context, runID string) (run.Run, []run.Record, error) {
	if err := ctx.Err(); err != nil {
		return run.Run{}, nil, err
	}
	dir := l.runDir(runID)
	s, err := readRun(dir)
	if err != nil {
		return run.Run{}, nil, fmt.Errorf("fileledger: load %q: %w", runID, err)
	}
	recs, err := readRecords(dir, nil)
	if err != nil {
		return run.Run{}, nil, fmt.Errorf("fileledger: load %q: %w", runID, err)
	}
	return s.run(), recs, nil
}

// Record implements ledger.Ledger: write-once on (RunID, Key). The record
// file is published with link(2); when it exists the stored record is read
// back and returned with run.ErrAlreadyRecorded.
func (l *Ledger) Record(ctx context.Context, rec run.Record) (run.Record, error) {
	if err := ctx.Err(); err != nil {
		return run.Record{}, err
	}
	dir := l.runDir(rec.RunID)
	if _, err := os.Stat(filepath.Join(dir, runFile)); errors.Is(err, os.ErrNotExist) {
		return run.Record{}, fmt.Errorf("fileledger: record %q/%q: %w", rec.RunID, rec.Key, run.ErrNotFound)
	}
	if rec.RecordedAt.IsZero() {
		rec.RecordedAt = time.Now()
	}
	raw, err := json.Marshal(toStoredRecord(rec))
	if err != nil {
		return run.Record{}, fmt.Errorf("fileledger: record %q/%q: %w", rec.RunID, rec.Key, err)
	}
	records := filepath.Join(dir, recordsDir)
	name := recordFile(rec.Key)
	if err := fsutil.Publish(records, name, raw); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return run.Record{}, fmt.Errorf("fileledger: record %q/%q: %w", rec.RunID, rec.Key, err)
		}
		stored, rerr := readRecord(filepath.Join(records, name))
		if rerr != nil {
			return run.Record{}, fmt.Errorf("fileledger: record %q/%q: %w", rec.RunID, rec.Key, rerr)
		}
		return stored, fmt.Errorf("fileledger: record %q/%q: %w", rec.RunID, rec.Key, run.ErrAlreadyRecorded)
	}
	return toStoredRecord(rec).record(), nil
}

// GetRun implements ledger.RunGetter: run.json alone.
func (l *Ledger) GetRun(ctx context.Context, runID string) (run.Run, error) {
	if err := ctx.Err(); err != nil {
		return run.Run{}, err
	}
	s, err := readRun(l.runDir(runID))
	if err != nil {
		return run.Run{}, fmt.Errorf("fileledger: get %q: %w", runID, err)
	}
	return s.run(), nil
}

// RecordsWithPrefix implements ledger.PrefixReader.
func (l *Ledger) RecordsWithPrefix(ctx context.Context, runID string, prefixes ...string) ([]run.Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir := l.runDir(runID)
	if _, err := os.Stat(filepath.Join(dir, runFile)); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("fileledger: records of %q: %w", runID, run.ErrNotFound)
	}
	if len(prefixes) == 0 {
		return nil, nil
	}
	recs, err := readRecords(dir, func(key string) bool {
		return slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(key, p) })
	})
	if err != nil {
		return nil, fmt.Errorf("fileledger: records of %q: %w", runID, err)
	}
	return recs, nil
}

// ListPending implements ledger.Ledger by reading every run.json under Dir.
// limit <= 0 means no limit.
func (l *Ledger) ListPending(ctx context.Context, before time.Time, cursor run.PendingCursor, limit int) ([]run.PendingRun, error) {
	runs, err := l.Runs(ctx)
	if err != nil {
		return nil, err
	}
	var pending []run.PendingRun
	for _, r := range runs {
		if r.Status.Terminal() || !r.CreatedAt.Before(before) {
			continue
		}
		p := run.PendingRun{ID: r.ID, CreatedAt: r.CreatedAt}
		if (cursor != run.PendingCursor{}) && !after(p, cursor) {
			continue
		}
		pending = append(pending, p)
	}
	slices.SortFunc(pending, func(a, b run.PendingRun) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	if limit > 0 && len(pending) > limit {
		pending = pending[:limit]
	}
	return pending, nil
}

func after(p run.PendingRun, c run.PendingCursor) bool {
	if c := p.CreatedAt.Compare(c.CreatedAt); c != 0 {
		return c > 0
	}
	return p.ID > c.ID
}

// Runs implements ledger.RunLister: every run under Dir, in no particular
// order. A directory without a readable run.json is skipped.
func (l *Ledger) Runs(ctx context.Context) ([]run.Run, error) {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return nil, fmt.Errorf("fileledger: list: %w", err)
	}
	var out []run.Run
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !e.IsDir() {
			continue
		}
		s, err := readRun(filepath.Join(l.dir, e.Name()))
		if err != nil {
			continue
		}
		out = append(out, s.run())
	}
	return out, nil
}

// Delete implements ledger.Deleter: it removes run.json and the run's
// records, then the run directory if nothing else is left in it.
func (l *Ledger) Delete(ctx context.Context, runID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir := l.runDir(runID)
	if _, err := os.Stat(filepath.Join(dir, runFile)); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("fileledger: delete %q: %w", runID, run.ErrNotFound)
	}
	unlock, err := fsutil.Lock(filepath.Join(dir, lockFile))
	if err != nil {
		return fmt.Errorf("fileledger: delete %q: %w", runID, err)
	}
	err = errors.Join(os.Remove(filepath.Join(dir, runFile)), os.RemoveAll(filepath.Join(dir, recordsDir)))
	unlock()
	if err != nil {
		return fmt.Errorf("fileledger: delete %q: %w", runID, err)
	}
	_ = os.Remove(filepath.Join(dir, lockFile))
	_ = os.Remove(dir) // only when empty
	return nil
}

func recordFile(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:]) + ".json"
}

func readRun(dir string) (storedRun, error) {
	var s storedRun
	raw, err := os.ReadFile(filepath.Join(dir, runFile)) // #nosec G304 -- dir is Dir plus a SHA-256 run ID.
	if errors.Is(err, os.ErrNotExist) {
		return s, run.ErrNotFound
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("decode %s: %w", runFile, err)
	}
	return s, nil
}

func readRecord(path string) (run.Record, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- path is a run directory plus a SHA-256 record key.
	if err != nil {
		return run.Record{}, err
	}
	var s storedRecord
	if err := json.Unmarshal(raw, &s); err != nil {
		return run.Record{}, fmt.Errorf("decode record %s: %w", filepath.Base(path), err)
	}
	return s.record(), nil
}

// readRecords reads the run's records whose key keep accepts (all when keep
// is nil). Temporary files from an unfinished publish are skipped.
func readRecords(dir string, keep func(string) bool) ([]run.Record, error) {
	entries, err := os.ReadDir(filepath.Join(dir, recordsDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]run.Record, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		rec, err := readRecord(filepath.Join(dir, recordsDir, e.Name()))
		if err != nil {
			return nil, err
		}
		if keep == nil || keep(rec.Key) {
			out = append(out, rec)
		}
	}
	return out, nil
}
