package pgstore

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// IterativeScan selects how pgvector continues an HNSW index scan when
// filters reject candidates. Without iterative scans, pgvector filters only
// the first hnsw.ef_search candidates (40 by default), so a selective filter
// can return fewer rows than the requested limit, or none.
type IterativeScan string

const (
	// IterativeScanAuto enables strict_order iterative scans for filtered
	// searches when the installed pgvector supports them (0.8.0 or later),
	// and leaves them off otherwise. It is the default.
	IterativeScanAuto IterativeScan = ""
	// IterativeScanOff never changes hnsw.iterative_scan.
	IterativeScanOff IterativeScan = "off"
	// IterativeScanStrict returns rows in exact distance order. It requires
	// pgvector 0.8.0 or later; older versions fail the search.
	IterativeScanStrict IterativeScan = "strict_order"
	// IterativeScanRelaxed may scan fewer tuples but can return rows slightly
	// out of distance order; the store re-sorts them by score. It requires
	// pgvector 0.8.0 or later.
	IterativeScanRelaxed IterativeScan = "relaxed_order"
)

// Option configures a Store.
type Option func(*Store)

// WithIterativeScan sets the iterative scan mode used for filtered vector
// searches. See IterativeScan.
func WithIterativeScan(mode IterativeScan) Option {
	return func(s *Store) {
		switch mode {
		case IterativeScanAuto, IterativeScanOff, IterativeScanStrict, IterativeScanRelaxed:
			s.iterativeScan = mode
		default:
			s.setOptionErr(fmt.Errorf("pgstore: unknown iterative scan mode %q", mode))
		}
	}
}

// WithEFSearch sets hnsw.ef_search, the candidate list size of an HNSW scan,
// for every vector search. pgvector accepts 1 to 1000.
func WithEFSearch(n int) Option {
	return func(s *Store) {
		if n < 1 || n > 1000 {
			s.setOptionErr(fmt.Errorf("pgstore: ef_search %d out of range [1, 1000]", n))
			return
		}
		s.efSearch = n
	}
}

// WithMaxScanTuples sets hnsw.max_scan_tuples, the upper bound on tuples an
// iterative scan visits before it stops. It requires pgvector 0.8.0 or later.
func WithMaxScanTuples(n int) Option {
	return func(s *Store) {
		if n < 1 {
			s.setOptionErr(fmt.Errorf("pgstore: max_scan_tuples %d must be positive", n))
			return
		}
		s.maxScanTuples = n
	}
}

// setOptionErr records err unless an earlier option already failed, so the
// reported error names the first invalid option.
func (s *Store) setOptionErr(err error) {
	if s.optionErr == nil {
		s.optionErr = err
	}
}

// setting is one transaction-local configuration parameter.
type setting struct {
	name, value string
}

// searchSettings returns the parameters to apply with SET LOCAL semantics
// for one search. filtered reports whether the query has any WHERE filter
// beyond the non-null embedding check.
func (s *Store) searchSettings(ctx context.Context, filtered bool) ([]setting, IterativeScan, error) {
	if s.optionErr != nil {
		return nil, "", s.optionErr
	}
	var out []setting
	if s.efSearch > 0 {
		out = append(out, setting{"hnsw.ef_search", strconv.Itoa(s.efSearch)})
	}

	mode := s.iterativeScan
	if mode == IterativeScanAuto {
		if !filtered {
			return out, IterativeScanOff, nil
		}
		supported, err := s.iterativeScanSupported(ctx)
		if err != nil {
			return nil, "", err
		}
		mode = IterativeScanOff
		if supported {
			mode = IterativeScanStrict
		}
	}
	if mode == IterativeScanStrict || mode == IterativeScanRelaxed {
		out = append(out, setting{"hnsw.iterative_scan", string(mode)})
		if s.maxScanTuples > 0 {
			out = append(out, setting{"hnsw.max_scan_tuples", strconv.Itoa(s.maxScanTuples)})
		}
	}
	return out, mode, nil
}

// iterativeScanSupported reports whether the installed pgvector extension is
// 0.8.0 or later. A successful lookup is cached for the life of the store; a
// failed one is retried on the next search.
func (s *Store) iterativeScanSupported(ctx context.Context) (bool, error) {
	s.versionMu.Lock()
	defer s.versionMu.Unlock()
	if s.versionKnown {
		return s.iterativeOK, nil
	}
	var version string
	err := s.pool.QueryRow(ctx, `SELECT extversion FROM pg_extension WHERE extname = 'vector'`).Scan(&version)
	if err != nil {
		return false, fmt.Errorf("pgstore: read pgvector version: %w", err)
	}
	s.iterativeOK = versionAtLeast(version, 0, 8)
	s.versionKnown = true
	return s.iterativeOK, nil
}

// versionAtLeast reports whether a dotted version string such as "0.8.0" is
// at least major.minor. Unparseable versions report false.
func versionAtLeast(version string, major, minor int) bool {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return false
	}
	gotMajor, err1 := strconv.Atoi(parts[0])
	gotMinor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false
	}
	if gotMajor != major {
		return gotMajor > major
	}
	return gotMinor >= minor
}
