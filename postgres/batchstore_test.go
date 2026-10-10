package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/batch"
	"github.com/urmzd/saige/agent/batch/batchtest"
)

func batchDatabase(t *testing.T) *BatchStore {
	t.Helper()
	pool := freshDatabase(t)
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if err := execScript(ctx, conn.Conn(), batchSQL); err != nil {
		t.Fatal(err)
	}
	return NewBatchStore(pool)
}

func TestBatchStoreConformance(t *testing.T) {
	batchtest.StoreConformance(t, batchDatabase(t))
}

// TestBatchStoreOneWinner checks that of several processes updating the
// same version of a job, exactly one wins.
func TestBatchStoreOneWinner(t *testing.T) {
	s := batchDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC()
	j, _, err := s.Create(ctx, batch.Job{ID: "race", Provider: "p", Manifest: "m", State: batch.JobSubmitting, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, conflicts := 0, 0
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := j
			c.State = batch.JobSubmitted
			_, err := s.Update(ctx, c)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, batch.ErrJobConflict):
				conflicts++
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if wins != 1 || conflicts != 7 {
		t.Fatalf("wins = %d, conflicts = %d", wins, conflicts)
	}
}

// TestRunMigrationsCreatesBatchJobs checks that RunMigrations creates the
// batch job table, twice without error.
func TestRunMigrationsCreatesBatchJobs(t *testing.T) {
	pool := freshDatabase(t)
	ctx := context.Background()
	for range 2 {
		if err := RunMigrations(ctx, pool, MigrationOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('saige_batch_jobs') IS NOT NULL`).Scan(&exists); err != nil || !exists {
		t.Fatalf("saige_batch_jobs exists = %v, %v", exists, err)
	}
	batchtest.StoreConformance(t, NewBatchStore(pool))
}
