package batch

import (
	"context"
	"errors"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// JobState is where a job is in its life, as the store records it.
type JobState string

const (
	// JobSubmitting: the record is saved and the vendor submit is in flight,
	// or the process stopped before the batch ID was saved.
	JobSubmitting JobState = "submitting"
	// JobSubmitted: the vendor accepted the batch; Handle is set.
	JobSubmitted JobState = "submitted"
	// JobEnded: the batch reached a terminal vendor state; Status holds it.
	JobEnded JobState = "ended"
	// JobCollected: every result was read and its budget settled.
	JobCollected JobState = "collected"
	// JobFailed: the submit was rejected or the batch failed as a whole.
	JobFailed JobState = "failed"
	// JobIndeterminate: a submit was interrupted and the vendor lookup could
	// not tell whether the batch exists. Reconcile resolves it.
	JobIndeterminate JobState = "indeterminate"
)

// Open reports whether the job still waits on the vendor.
func (s JobState) Open() bool { return s == JobSubmitting || s == JobSubmitted }

// Job is the durable record of one batch job. It holds what a restarted
// process needs to resume: the vendor handle, the manifest hash that proves
// a resubmission sends the same work, and the custom IDs that map results
// back to requests. It never holds the prompts.
type Job struct {
	// ID is the caller's job ID, the idempotency key of Submit.
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
	// Manifest is the SHA-256 of the requests (see Manifest).
	Manifest string `json:"manifest"`
	// CustomIDs are the requests' IDs in submission order. Request i is sent
	// as Prefix-i.
	CustomIDs []string `json:"custom_ids"`
	// Prefix is the vendor-safe request ID prefix, also sent as the tag.
	Prefix string             `json:"prefix"`
	State  JobState           `json:"state"`
	Handle *types.BatchHandle `json:"handle,omitempty"`
	// Status is the last vendor status seen.
	Status types.BatchStatus `json:"status"`
	// Owner names whoever waits on the job, such as a durable run ID, so a
	// watcher can wake it.
	Owner string `json:"owner,omitempty"`
	// Resubmit permits the next Submit to send the batch again without a
	// lookup, set by Reconcile.
	Resubmit bool `json:"resubmit,omitempty"`
	// Attempts counts vendor submits.
	Attempts int `json:"attempts,omitempty"`
	// SubmitStartedAt is when the latest vendor submit started; a lookup
	// after a crash searches from it.
	SubmitStartedAt time.Time `json:"submit_started_at,omitzero"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	Error           string    `json:"error,omitempty"`
	// Version increases with each update; Update rejects a stale one.
	Version int64 `json:"version"`
}

func (j Job) clone() Job {
	j.CustomIDs = slices.Clone(j.CustomIDs)
	if j.Handle != nil {
		h := *j.Handle
		if h.Meta != nil {
			m := make(map[string]string, len(h.Meta))
			for k, v := range h.Meta {
				m[k] = v
			}
			h.Meta = m
		}
		j.Handle = &h
	}
	return j
}

// Store persists job records. Implementations: MemoryStore, FileStore and
// postgres.BatchStore.
type Store interface {
	// Create saves j when no job with its ID exists and reports true. When
	// one exists it returns that job unchanged and false.
	Create(ctx context.Context, j Job) (Job, bool, error)
	// Get returns the job, or an error matching ErrJobNotFound.
	Get(ctx context.Context, id string) (Job, error)
	// Update replaces the job when its Version matches the stored one and
	// returns it with Version incremented. A stale Version matches
	// ErrJobConflict.
	Update(ctx context.Context, j Job) (Job, error)
	// List returns jobs in the given states, all jobs when none is given,
	// oldest first.
	List(ctx context.Context, states ...JobState) ([]Job, error)
}

var (
	// ErrJobNotFound reports a job ID the store does not hold.
	ErrJobNotFound = errors.New("batch: job not found")
	// ErrJobConflict reports an update from a stale read.
	ErrJobConflict = errors.New("batch: job was updated concurrently")
)

// MemoryStore is a Store in process memory, for tests and short-lived jobs.
type MemoryStore struct {
	mu   sync.Mutex
	jobs map[string]Job
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{jobs: map[string]Job{}} }

var _ Store = (*MemoryStore)(nil)

// Create implements Store.
func (s *MemoryStore) Create(_ context.Context, j Job) (Job, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.jobs[j.ID]; ok {
		return cur.clone(), false, nil
	}
	j.Version = 1
	s.jobs[j.ID] = j.clone()
	return j.clone(), true, nil
}

// Get implements Store.
func (s *MemoryStore) Get(_ context.Context, id string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return Job{}, ErrJobNotFound
	}
	return j.clone(), nil
}

// Update implements Store.
func (s *MemoryStore) Update(_ context.Context, j Job) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.jobs[j.ID]
	if !ok {
		return Job{}, ErrJobNotFound
	}
	if cur.Version != j.Version {
		return Job{}, ErrJobConflict
	}
	j.Version++
	s.jobs[j.ID] = j.clone()
	return j.clone(), nil
}

// List implements Store.
func (s *MemoryStore) List(_ context.Context, states ...JobState) ([]Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Job
	for _, j := range s.jobs {
		if len(states) == 0 || slices.Contains(states, j.State) {
			out = append(out, j.clone())
		}
	}
	sortJobs(out)
	return out, nil
}

func sortJobs(jobs []Job) {
	sort.Slice(jobs, func(a, b int) bool {
		if !jobs[a].CreatedAt.Equal(jobs[b].CreatedAt) {
			return jobs[a].CreatedAt.Before(jobs[b].CreatedAt)
		}
		return jobs[a].ID < jobs[b].ID
	})
}
