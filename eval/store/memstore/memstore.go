// Package memstore is an in-memory eval/store.Store for tests and short lived
// tools. Nothing survives the process.
package memstore

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/urmzd/saige/eval"
	"github.com/urmzd/saige/eval/store"
)

// Store keeps runs and units in memory. Records are held as encoded JSON, so
// every read returns a fresh copy and no caller shares memory with the store.
type Store struct {
	mu   sync.RWMutex
	runs map[string]*runState
}

type runState struct {
	record   []byte
	order    []string            // unit keys in first-put order
	current  map[string][]byte   // key -> current attempt
	attempts map[string][][]byte // key -> archived attempts, oldest first
}

var _ store.Store = (*Store)(nil)

// New returns an empty store.
func New() *Store {
	return &Store{runs: map[string]*runState{}}
}

// CreateRun implements store.Store.
func (s *Store) CreateRun(_ context.Context, run eval.RunRecord) error {
	if err := store.ValidateRunID(run.ID); err != nil {
		return err
	}
	data, err := json.Marshal(run)
	if err != nil {
		return fmt.Errorf("memstore: encode run: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.runs[run.ID]; ok {
		return fmt.Errorf("%w: %q", store.ErrRunExists, run.ID)
	}
	s.runs[run.ID] = &runState{
		record:   data,
		current:  map[string][]byte{},
		attempts: map[string][][]byte{},
	}
	return nil
}

// UpdateRun implements store.Store.
func (s *Store) UpdateRun(_ context.Context, run eval.RunRecord) error {
	data, err := json.Marshal(run)
	if err != nil {
		return fmt.Errorf("memstore: encode run: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.runs[run.ID]
	if !ok {
		return fmt.Errorf("%w: run %q", store.ErrNotFound, run.ID)
	}
	st.record = data
	return nil
}

// GetRun implements store.Store.
func (s *Store) GetRun(_ context.Context, id string) (eval.RunRecord, error) {
	s.mu.RLock()
	st, ok := s.runs[id]
	var data []byte
	if ok {
		data = st.record
	}
	s.mu.RUnlock()
	if !ok {
		return eval.RunRecord{}, fmt.Errorf("%w: run %q", store.ErrNotFound, id)
	}
	var run eval.RunRecord
	if err := json.Unmarshal(data, &run); err != nil {
		return eval.RunRecord{}, fmt.Errorf("memstore: decode run: %w", err)
	}
	return run, nil
}

// ListRuns implements store.Store.
func (s *Store) ListRuns(_ context.Context, filter store.RunFilter) ([]eval.RunRecord, error) {
	s.mu.RLock()
	records := make([][]byte, 0, len(s.runs))
	for _, st := range s.runs {
		records = append(records, st.record)
	}
	s.mu.RUnlock()

	runs := make([]eval.RunRecord, 0, len(records))
	for _, data := range records {
		var run eval.RunRecord
		if err := json.Unmarshal(data, &run); err != nil {
			return nil, fmt.Errorf("memstore: decode run: %w", err)
		}
		if filter.Match(run) {
			runs = append(runs, run)
		}
	}
	return store.SortRuns(runs, filter.Limit), nil
}

// PutUnit implements store.Store.
func (s *Store) PutUnit(_ context.Context, unit eval.Unit) (eval.Unit, error) {
	if unit.Key == "" {
		unit.Key = eval.UnitKey(unit.Observation)
	}
	if unit.RecordedAt.IsZero() {
		unit.RecordedAt = time.Now().UTC()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.runs[unit.RunID]
	if !ok {
		return eval.Unit{}, fmt.Errorf("%w: run %q", store.ErrNotFound, unit.RunID)
	}
	unit.Attempt = 1
	if prior, exists := st.current[unit.Key]; exists {
		var p eval.Unit
		if err := json.Unmarshal(prior, &p); err != nil {
			return eval.Unit{}, fmt.Errorf("memstore: decode unit: %w", err)
		}
		unit.Attempt = p.Attempt + 1
		st.attempts[unit.Key] = append(st.attempts[unit.Key], prior)
	} else {
		st.order = append(st.order, unit.Key)
	}
	data, err := json.Marshal(unit)
	if err != nil {
		return eval.Unit{}, fmt.Errorf("memstore: encode unit: %w", err)
	}
	st.current[unit.Key] = data

	var out eval.Unit
	if err := json.Unmarshal(data, &out); err != nil {
		return eval.Unit{}, fmt.Errorf("memstore: decode unit: %w", err)
	}
	return out, nil
}

// Units implements store.Store.
func (s *Store) Units(_ context.Context, runID string, filter store.UnitFilter) ([]eval.Unit, error) {
	s.mu.RLock()
	st, ok := s.runs[runID]
	var records [][]byte
	if ok {
		records = make([][]byte, 0, len(st.order))
		for _, key := range st.order {
			records = append(records, st.current[key])
		}
	}
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: run %q", store.ErrNotFound, runID)
	}
	return decodeUnits(records, filter)
}

// Attempts implements store.Store.
func (s *Store) Attempts(_ context.Context, runID, key string) ([]eval.Unit, error) {
	s.mu.RLock()
	st, ok := s.runs[runID]
	var records [][]byte
	if ok {
		records = append(records, st.attempts[key]...)
	}
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: run %q", store.ErrNotFound, runID)
	}
	return decodeUnits(records, store.UnitFilter{})
}

func decodeUnits(records [][]byte, filter store.UnitFilter) ([]eval.Unit, error) {
	units := make([]eval.Unit, 0, len(records))
	for _, data := range records {
		var u eval.Unit
		if err := json.Unmarshal(data, &u); err != nil {
			return nil, fmt.Errorf("memstore: decode unit: %w", err)
		}
		if filter.Match(u) {
			units = append(units, u)
		}
	}
	return units, nil
}
