package eval

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"

	"github.com/urmzd/saige/agent/batch"
	"github.com/urmzd/saige/agent/types"
)

// WithBatch runs the model calls of [PopulateAll], [Run] and [Compare] as
// batches through c, at the batch price. Give c to the subjects and judges
// as their provider or generator (a [batch.Coalescer] is both), and pass it
// here so the run can tell when every case is waiting on a call: each case
// becomes a participant, every case runs at once (the concurrency limit does
// not apply), and the batch is sent as soon as all of them wait. [Compare]
// populates and scores both arms together, so base, exp and the judges of
// both share batches.
//
// Each call's request ID is its arm, case ID, turn and sample (and the
// scorer, for judge calls), so results map back to the case that asked, and
// a restarted run with the same dataset sends the same batches, which the
// coalescer's runner resumes instead of paying for again.
//
// A subject that makes several dependent calls waits for one batch per
// call; batches suit single-turn subjects and judges.
func WithBatch(c *batch.Coalescer) Option {
	return func(cfg *Config) { cfg.batch = c }
}

// task is one participant's work.
type task func(ctx context.Context)

// runTasks joins every task to c before starting any, so a batch is not
// sent until all of them are waiting or done, then runs them at once.
func runTasks(ctx context.Context, c *batch.Coalescer, tasks []task) {
	leaves := make([]func(), len(tasks))
	for i := range tasks {
		leaves[i] = c.Join()
	}
	var wg sync.WaitGroup
	for i, t := range tasks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer leaves[i]()
			t(ctx)
		}()
	}
	wg.Wait()
}

// batchKey names an observation's calls.
func batchKey(arm string, obs Observation) string {
	k := obs.ID + "/t" + strconv.Itoa(obs.Turn) + "/s" + strconv.Itoa(obs.Sample)
	if arm != "" {
		k = arm + "/" + k
	}
	return k
}

func populateTasks(arm string, observations []Observation, subject Subject, errs []error) []task {
	tasks := make([]task, len(observations))
	for i := range observations {
		tasks[i] = func(ctx context.Context) {
			if ctx.Err() != nil {
				errs[i] = ctx.Err()
				return
			}
			errs[i] = subject(batch.WithKey(ctx, batchKey(arm, observations[i])), &observations[i])
		}
	}
	return tasks
}

func scoreTasks(arm string, observations []Observation, scorers []Scorer, logger *slog.Logger, results []ObservationResult, done []bool) []task {
	tasks := make([]task, len(observations))
	for i, obs := range observations {
		tasks[i] = func(ctx context.Context) {
			if SubjectError(obs) != "" {
				results[i], done[i] = ObservationResult{Observation: obs}, true
				return
			}
			scores, interrupted := scoreKeyed(ctx, arm, obs, scorers, logger)
			if interrupted {
				return
			}
			results[i], done[i] = ObservationResult{Observation: obs, Scores: scores}, true
		}
	}
	return tasks
}

// scoreKeyed is scoreObservation with each scorer's calls keyed by the
// observation and the scorer.
func scoreKeyed(ctx context.Context, arm string, obs Observation, scorers []Scorer, logger *slog.Logger) (scores []Score, interrupted bool) {
	key := batchKey(arm, obs)
	for _, s := range scorers {
		if ctx.Err() != nil {
			return nil, true
		}
		score, err := s.Score(batch.WithKey(ctx, key+"/"+s.Name()), obs)
		if err != nil {
			if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
				return nil, true
			}
			logger.Error("scorer failed", "observation", obs.ID, "scorer", s.Name(), "error", err)
			scores = append(scores, Score{Name: s.Name(), Error: err.Error()})
			continue
		}
		if score.Name != "" {
			scores = append(scores, score)
		}
	}
	return scores, false
}

// compareBatched populates and scores both arms of a comparison together,
// so their calls share batches.
func compareBatched(ctx context.Context, cfg *Config, baseObs, expObs []Observation, base, exp Subject, scorers []Scorer) (*SuiteResult, *SuiteResult, error, error) {
	if cfg.dialPolicy != nil {
		ctx = types.ContextWithDialPolicy(ctx, *cfg.dialPolicy)
	}
	baseErrs, expErrs := make([]error, len(baseObs)), make([]error, len(expObs))
	runTasks(ctx, cfg.batch, append(populateTasks("base", baseObs, base, baseErrs), populateTasks("exp", expObs, exp, expErrs)...))
	_ = finishPopulate(cfg, baseObs, baseErrs)
	_ = finishPopulate(cfg, expObs, expErrs)

	if cfg.Sampler.N > 1 {
		sampled := make([]Scorer, len(scorers))
		for i, sc := range scorers {
			sampled[i] = Sampled(sc, cfg.Sampler)
		}
		scorers = sampled
	}
	baseRes, baseDone := make([]ObservationResult, len(baseObs)), make([]bool, len(baseObs))
	expRes, expDone := make([]ObservationResult, len(expObs)), make([]bool, len(expObs))
	runTasks(ctx, cfg.batch, append(scoreTasks("base", baseObs, scorers, cfg.Logger, baseRes, baseDone),
		scoreTasks("exp", expObs, scorers, cfg.Logger, expRes, expDone)...))
	baseSuite, baseErr := suiteFrom(ctx, cfg.Name+"/base", cfg, baseObs, baseRes, baseDone)
	expSuite, expErr := suiteFrom(ctx, cfg.Name+"/exp", cfg, expObs, expRes, expDone)
	return baseSuite, expSuite, baseErr, expErr
}
