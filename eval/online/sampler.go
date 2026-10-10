package online

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"maps"
	"slices"
	"sync/atomic"
	"time"

	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/eval"
	"github.com/urmzd/saige/eval/store"
)

// DefaultSuite is the suite name of online runs when [Sampler.Suite] is
// empty.
const DefaultSuite = "online"

// DefaultChannel is the notifier channel [Announce] and [Sampler.Watch]
// use when none is given.
const DefaultChannel = "saige.eval.online"

// Filter selects the records a [Sampler] considers. Zero fields match
// everything; set fields must all match.
type Filter struct {
	// Models and Presets keep records served by one of the listed models
	// or presets.
	Models  []string
	Presets []string
	// Where keeps records whose observation labels match, including the
	// labels the sampler adds such as LabelConversation and LabelScope.
	Where eval.Where
	// Tools keeps records that called at least one of the listed tools.
	Tools []string
	// Errored, when set, keeps only records that did (true) or did not
	// (false) record an error.
	Errored *bool
}

// Match reports whether r passes the filter.
func (f Filter) Match(r Record) bool {
	if len(f.Models) > 0 && !slices.Contains(f.Models, r.Model) {
		return false
	}
	if len(f.Presets) > 0 && !slices.Contains(f.Presets, r.Preset) {
		return false
	}
	if f.Errored != nil && *f.Errored != (r.Error != "") {
		return false
	}
	if len(f.Tools) > 0 && !slices.ContainsFunc(r.Tools(), func(t string) bool { return slices.Contains(f.Tools, t) }) {
		return false
	}
	if len(f.Where) > 0 {
		obs, err := r.Observation()
		if err != nil || !f.Where.Match(obs) {
			return false
		}
	}
	return true
}

// Sampler scores a sample of production runs with eval scorers and records
// the results in an eval store. A Sampler is safe for concurrent use once
// configured.
type Sampler struct {
	// Store receives one run per sweep or watch. Required.
	Store store.Store
	// Suite names the runs; empty means DefaultSuite.
	Suite string
	// Rate is the fraction of matching records to score, from 0 to 1. Zero
	// scores every matching record.
	Rate float64
	// Seed makes sampling reproducible: a record is sampled or not by a
	// hash of the seed, its conversation, and its node, so the same seed
	// picks the same records in every sweep, in any order, and in watch
	// mode.
	Seed uint64
	// Filter selects the records to consider before sampling.
	Filter Filter
	// Scorers run on every sampled record. Deterministic checks belong
	// here.
	Scorers []eval.Scorer
	// Judges are LLM scorers. Each runs only while Budget has room: once
	// it is exhausted, or a call is refused admission, the judge declines
	// the record and the refusal is counted in Report.JudgesSkipped. Build
	// them on a [BudgetedGenerator] over a cheap model charged to Budget.
	Judges []eval.Scorer
	// Budget, when set, is checked before every judge call.
	Budget *types.Budget
	// Labels are added to every run's labels, after LabelSource.
	Labels eval.Labels
	// Provenance is stamped on every run.
	Provenance eval.Provenance
	// Concurrency is how many records are scored at once in a sweep.
	Concurrency int
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Report summarizes one sweep or watch.
type Report struct {
	Run eval.RunRecord `json:"run"`
	// Seen counts records the source returned; Matched those that passed
	// the filter; Scored those sampled and scored.
	Seen    int `json:"seen"`
	Matched int `json:"matched"`
	Scored  int `json:"scored"`
	// JudgesSkipped counts judge scores left out because the budget
	// refused them.
	JudgesSkipped int `json:"judges_skipped,omitempty"`
}

// Sampled reports whether r falls in the sample. It depends only on Seed,
// Rate, and r's conversation and node.
func (s *Sampler) Sampled(r Record) bool {
	if s.Rate <= 0 || s.Rate >= 1 {
		return true
	}
	key := binary.BigEndian.AppendUint64(nil, s.Seed)
	key = append(append(append(key, r.Ref.Conversation...), 0), r.Ref.Node...)
	h := fnv.New64a()
	_, _ = h.Write(key) // a hash.Hash never returns an error
	// Use the top 53 bits as a uniform fraction in [0, 1).
	return float64(h.Sum64()>>11)/(1<<53) < s.Rate
}

func (s *Sampler) suite() string {
	if s.Suite == "" {
		return DefaultSuite
	}
	return s.Suite
}

func (s *Sampler) logger() *slog.Logger {
	if s.Logger == nil {
		return slog.Default()
	}
	return s.Logger
}

func (s *Sampler) runLabels() eval.Labels {
	labels := eval.Labels{LabelSource: SourceOnline}
	maps.Copy(labels, s.Labels)
	labels[LabelSource] = SourceOnline
	return labels
}

// scorers returns the scorers for one pass, with the judges wrapped to
// decline once the budget refuses them.
func (s *Sampler) scorers(skipped *atomic.Int64) []eval.Scorer {
	out := slices.Clone(s.Scorers)
	for _, j := range s.Judges {
		out = append(out, &budgetedJudge{inner: j, budget: s.Budget, skipped: skipped})
	}
	return out
}

type budgetedJudge struct {
	inner   eval.Scorer
	budget  *types.Budget
	skipped *atomic.Int64
}

func (j *budgetedJudge) Name() string { return j.inner.Name() }

func (j *budgetedJudge) Score(ctx context.Context, obs eval.Observation) (eval.Score, error) {
	if j.budget != nil && j.budget.Exceeded() {
		j.skipped.Add(1)
		return eval.Score{}, nil
	}
	sc, err := j.inner.Score(ctx, obs)
	if err != nil && overBudget(err) {
		j.skipped.Add(1)
		return eval.Score{}, nil
	}
	return sc, err
}

// Sweep scores the records src returns for w once and records them as one
// run. The run is recorded even when nothing was sampled, so a quiet window
// is visible as an empty run rather than a gap.
func (s *Sampler) Sweep(ctx context.Context, src Source, w Window) (Report, error) {
	if s.Store == nil {
		return Report{}, errors.New("online: sampler has no store")
	}
	recs, err := src.Records(ctx, w)
	if err != nil {
		return Report{}, err
	}
	rep := Report{Seen: len(recs)}
	var obs []eval.Observation
	seen := map[string]bool{}
	for _, r := range recs {
		if !s.Filter.Match(r) {
			continue
		}
		rep.Matched++
		if !s.Sampled(r) {
			continue
		}
		o, err := r.Observation()
		if err != nil {
			return rep, fmt.Errorf("online: record %s: %w", r.Ref.Node, err)
		}
		if key := eval.UnitKey(o); !seen[key] {
			seen[key] = true
			obs = append(obs, o)
		}
	}
	rep.Scored = len(obs)

	var skipped atomic.Int64
	opts := []eval.Option{eval.WithLogger(s.logger())}
	if s.Concurrency > 0 {
		opts = append(opts, eval.WithConcurrency(s.Concurrency))
	}
	suite, runErr := eval.Run(ctx, s.suite(), obs, s.scorers(&skipped), opts...)
	rep.JudgesSkipped = int(skipped.Load())

	prov := s.Provenance
	prov.Extra = maps.Clone(prov.Extra)
	if prov.Extra == nil {
		prov.Extra = map[string]string{}
	}
	prov.Extra["window_from"] = w.From.UTC().Format(time.RFC3339Nano)
	if !w.To.IsZero() {
		prov.Extra["window_to"] = w.To.UTC().Format(time.RFC3339Nano)
	}
	run, err := s.save(ctx, eval.NewRunID(), suite, runErr, prov)
	rep.Run = run
	if err != nil {
		return rep, err
	}
	return rep, runErr
}

// save records a finished suite as a run, as store.SaveSuite does, with the
// run labels and a trace link on every unit that has one.
func (s *Sampler) save(ctx context.Context, id string, suite *eval.SuiteResult, runErr error, prov eval.Provenance) (eval.RunRecord, error) {
	final := eval.NewRunRecord(id, suite, runErr, prov)
	final.Labels = s.runLabels()
	run := final
	run.Status, run.FinishedAt = eval.RunRunning, time.Time{}
	if err := s.Store.CreateRun(ctx, run); err != nil {
		return final, err
	}
	if suite != nil {
		for _, u := range suite.Units(id) {
			if _, err := s.Store.PutUnit(ctx, withTrace(u)); err != nil {
				run.Status, run.Error = eval.RunErrored, fmt.Sprintf("save unit %q: %v", u.Key, err)
				_ = s.Store.UpdateRun(context.WithoutCancel(ctx), run)
				return run, fmt.Errorf("save unit %q: %w", u.Key, err)
			}
		}
	}
	return final, s.Store.UpdateRun(context.WithoutCancel(ctx), final)
}

// withTrace sets the unit's trace link from the record ref annotation.
func withTrace(u eval.Unit) eval.Unit {
	var ref Ref
	if raw, ok := u.Observation.Annotations[AnnotationRef]; ok && json.Unmarshal(raw, &ref) == nil && ref.TraceID != "" {
		u.Trace = &eval.TraceRef{TraceID: ref.TraceID, SpanID: ref.SpanID}
	}
	return u
}
