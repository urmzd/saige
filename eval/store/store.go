// Package store persists evaluation runs and their units.
//
// A [Store] holds [eval.RunRecord] rows and the [eval.Unit] rows that belong
// to them. Units are keyed by [eval.UnitKey] within a run: putting a unit
// whose key already exists archives the stored one as a prior attempt and
// replaces it, so a retried case never loses the attempt it replaces and
// never appears twice. Stores copy everything they accept and return, so a
// caller cannot change stored data through a slice or map it still holds.
//
// The results store is an evaluation record. It is not a durable step log:
// a rerun writes a new attempt rather than skipping work because a unit
// exists.
//
// Implementations: eval/store/memstore (in memory, for tests and short
// lived tools), eval/store/filestore (a directory of JSON and JSONL files),
// and eval/pgstore (PostgreSQL, tenant scoped). eval/store/storetest holds
// the shared conformance suite.
package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/urmzd/saige/eval"
)

var (
	// ErrNotFound is returned for a run or unit that does not exist.
	ErrNotFound = errors.New("eval store: not found")
	// ErrRunExists is returned by CreateRun for an ID already in use.
	ErrRunExists = errors.New("eval store: run already exists")
	// ErrInvalidID is returned for a run ID that is empty or unsafe to
	// use as a file name.
	ErrInvalidID = errors.New("eval store: invalid run id")
	// ErrDuplicateUnit is returned by SaveSuite when two results of one
	// suite share a unit key, which would make the second overwrite the
	// first.
	ErrDuplicateUnit = errors.New("eval store: duplicate unit key")
)

// RunFilter selects runs for [Store.ListRuns]. Zero fields match everything.
type RunFilter struct {
	// Suite matches runs of one suite.
	Suite string
	// Status matches runs in any of the listed states.
	Status []eval.RunStatus
	// Limit caps the result; zero means no limit.
	Limit int
}

// Match reports whether run passes the filter, ignoring Limit.
func (f RunFilter) Match(run eval.RunRecord) bool {
	if f.Suite != "" && run.Suite != f.Suite {
		return false
	}
	if len(f.Status) == 0 {
		return true
	}
	for _, s := range f.Status {
		if run.Status == s {
			return true
		}
	}
	return false
}

// UnitFilter selects units for [Store.Units]. Zero fields match everything.
type UnitFilter struct {
	// Where matches units whose observation labels match.
	Where eval.Where
	// ObservationID matches units of one observation.
	ObservationID string
}

// Match reports whether unit passes the filter.
func (f UnitFilter) Match(u eval.Unit) bool {
	if f.ObservationID != "" && u.Observation.ID != f.ObservationID {
		return false
	}
	return f.Where.Match(u.Observation)
}

// Store persists runs and units. Implementations must be safe for
// concurrent use.
type Store interface {
	// CreateRun records a new run. It fails with ErrRunExists when the ID
	// is taken and ErrInvalidID when the ID is unusable.
	CreateRun(ctx context.Context, run eval.RunRecord) error
	// UpdateRun replaces a run's record, such as to finish it. It fails
	// with ErrNotFound when the run does not exist.
	UpdateRun(ctx context.Context, run eval.RunRecord) error
	// GetRun returns one run, or ErrNotFound.
	GetRun(ctx context.Context, id string) (eval.RunRecord, error)
	// ListRuns returns matching runs, newest StartedAt first.
	ListRuns(ctx context.Context, filter RunFilter) ([]eval.RunRecord, error)

	// PutUnit records a unit of an existing run and returns it as stored.
	// An empty Key is set to [eval.UnitKey] of the observation, a zero
	// RecordedAt to the current time, and Attempt to one more than the
	// stored unit with the same key, which is archived first. It fails
	// with ErrNotFound when the run does not exist.
	PutUnit(ctx context.Context, unit eval.Unit) (eval.Unit, error)
	// Units returns the current attempt of each matching unit of a run, in
	// the order their keys were first put. It fails with ErrNotFound when
	// the run does not exist.
	Units(ctx context.Context, runID string, filter UnitFilter) ([]eval.Unit, error)
	// Attempts returns the archived prior attempts of one unit, oldest
	// first, without the current attempt. A key with no prior attempts
	// returns an empty slice.
	Attempts(ctx context.Context, runID, key string) ([]eval.Unit, error)
}

// SortRuns orders runs newest StartedAt first, breaking ties by ID, and
// truncates the result to limit when limit is positive. Store
// implementations use it so every store lists runs in the same order.
func SortRuns(runs []eval.RunRecord, limit int) []eval.RunRecord {
	sort.SliceStable(runs, func(i, j int) bool {
		a, b := runs[i], runs[j]
		if !a.StartedAt.Equal(b.StartedAt) {
			return a.StartedAt.After(b.StartedAt)
		}
		return a.ID > b.ID
	})
	if limit > 0 && len(runs) > limit {
		runs = runs[:limit]
	}
	return runs
}

// maxIDLength bounds run IDs so they stay usable as file names.
const maxIDLength = 200

// ValidateRunID rejects IDs that are empty, too long, or not safe as a single
// path element: path separators, NUL, and "." or "..".
func ValidateRunID(id string) error {
	switch {
	case id == "", id == ".", id == "..":
		return fmt.Errorf("%w %q", ErrInvalidID, id)
	case len(id) > maxIDLength:
		return fmt.Errorf("%w: longer than %d bytes", ErrInvalidID, maxIDLength)
	case strings.ContainsAny(id, "/\\\x00"):
		return fmt.Errorf("%w %q: contains a path separator or NUL", ErrInvalidID, id)
	}
	return nil
}

// SaveSuite records a finished suite as one run: it creates the run in the
// running state, puts every unit, and then updates the run with the summary
// from [eval.NewRunRecord]. runErr is the error the suite's run returned.
// When a unit fails to save, the run is marked errored and the error is
// returned.
//
// Two results with the same [eval.UnitKey] (for example a repeated case ID,
// or variants told apart by a label other than [eval.LabelVariant]) would
// collapse into one stored unit, so SaveSuite checks the keys first and
// returns ErrDuplicateUnit, naming the key, without writing anything.
func SaveSuite(ctx context.Context, s Store, id string, suite *eval.SuiteResult, runErr error, prov eval.Provenance) (eval.RunRecord, error) {
	run := eval.NewRunRecord(id, suite, runErr, prov)
	var units []eval.Unit
	if suite != nil {
		units = suite.Units(id)
		seen := make(map[string]bool, len(units))
		for _, u := range units {
			if seen[u.Key] {
				return run, fmt.Errorf("%w %q in suite %q", ErrDuplicateUnit, u.Key, suite.Name)
			}
			seen[u.Key] = true
		}
	}
	final := run
	run.Status = eval.RunRunning
	run.FinishedAt = time.Time{}
	if err := s.CreateRun(ctx, run); err != nil {
		return run, err
	}
	for _, u := range units {
		if _, err := s.PutUnit(ctx, u); err != nil {
			run.Status = eval.RunErrored
			run.Error = fmt.Sprintf("save unit %q: %v", u.Key, err)
			_ = s.UpdateRun(ctx, run)
			return run, fmt.Errorf("save unit %q: %w", u.Key, err)
		}
	}
	if err := s.UpdateRun(ctx, final); err != nil {
		return final, err
	}
	return final, nil
}

// LoadSuite rebuilds a stored run as an [eval.SuiteResult], so it can be
// gated with Check or compared with CompareSuites.
func LoadSuite(ctx context.Context, s Store, id string) (*eval.SuiteResult, error) {
	run, err := s.GetRun(ctx, id)
	if err != nil {
		return nil, err
	}
	units, err := s.Units(ctx, id, UnitFilter{})
	if err != nil {
		return nil, err
	}
	return eval.SuiteFromUnits(run, units), nil
}

// LatestSucceeded returns the newest succeeded run of a suite, the usual
// baseline for a regression check. Errored and canceled runs never count.
// It returns ErrNotFound when the suite has no succeeded run.
func LatestSucceeded(ctx context.Context, s Store, suite string) (eval.RunRecord, error) {
	runs, err := s.ListRuns(ctx, RunFilter{Suite: suite, Status: []eval.RunStatus{eval.RunSucceeded}, Limit: 1})
	if err != nil {
		return eval.RunRecord{}, err
	}
	if len(runs) == 0 {
		return eval.RunRecord{}, fmt.Errorf("%w: no succeeded run of suite %q", ErrNotFound, suite)
	}
	return runs[0], nil
}
