package local

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/urmzd/saige/durable/adapters/file/fileledger"
	"github.com/urmzd/saige/durable/adapters/file/filequeue"
	"github.com/urmzd/saige/durable/run"

	"github.com/urmzd/saige/agent/types"
)

// legacySnapshot is the state.json an earlier release kept for each run:
// one snapshot, rewritten on every change.
type legacySnapshot struct {
	History []struct {
		At time.Time `json:"at"`
	} `json:"history"`
	Version  int    `json:"version"`
	RunID    string `json:"run_id"`
	Revision string `json:"revision"`
	Status   string `json:"status"`
	Input    []byte `json:"input"`
	Inputs   []struct {
		Sequence int       `json:"sequence"`
		Key      string    `json:"key"`
		At       time.Time `json:"at"`
		Messages []byte    `json:"messages"`
	} `json:"inputs"`
	Steps map[string]struct {
		Status      string    `json:"status"`
		CompletedAt time.Time `json:"completed_at"`
		Result      []byte    `json:"result"`
		Error       string    `json:"error"`
		Idempotent  bool      `json:"idempotent"`
	} `json:"steps"`
	Interrupts map[string]struct {
		Request     types.ApprovalRequest   `json:"request"`
		CreatedAt   time.Time               `json:"created_at"`
		ExpiresAt   time.Time               `json:"expires_at"`
		Decision    *types.ApprovalDecision `json:"decision"`
		DecisionKey string                  `json:"decision_key"`
		DecidedAt   time.Time               `json:"decided_at"`
		Posted      *types.Interrupt        `json:"posted"`
		Answer      json.RawMessage         `json:"answer"`
	} `json:"interrupts"`
	UpdatedAt time.Time `json:"updated_at"`
}

const (
	legacyFile   = "state.json"
	migratedFile = "state.json.migrated"
)

// migrate turns every run directory under dir that still holds an earlier
// release's state.json into the durable engine's records, then keeps the
// snapshot as state.json.migrated. Each record is written once, so a
// migration interrupted part way finishes on the next open.
//
// The records are the ones the run's replay reads: a completed step's
// result, an uncertainty marker for a step left without an outcome (a step
// marked idempotent runs again instead), each interrupt with its reply, and
// each appended input segment. A cancelled run is cancelled again.
func migrate(dir string, lgr *fileledger.Ledger, q *filequeue.Queue) error {
	paths, err := filepath.Glob(filepath.Join(dir, "*", legacyFile))
	if err != nil {
		return err
	}
	ctx := context.Background()
	for _, path := range paths {
		raw, err := os.ReadFile(path) // #nosec G304 -- a run directory under the host's private directory.
		if err != nil {
			return fmt.Errorf("migrate %s: %w", path, err)
		}
		var s legacySnapshot
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		if err := d.Decode(&s); err != nil || s.Version != 1 || s.RunID == "" {
			continue // not a snapshot this release can read; leave it
		}
		if fileledger.RunDir(dir, s.RunID) != filepath.Dir(path) {
			continue
		}
		if err := migrateRun(ctx, lgr, q, s); err != nil {
			return fmt.Errorf("migrate run %s: %w", s.RunID, err)
		}
		if err := os.Rename(path, filepath.Join(filepath.Dir(path), migratedFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("migrate run %s: %w", s.RunID, err)
		}
		_ = os.Remove(filepath.Join(filepath.Dir(path), "worker.lock"))
	}
	return nil
}

func migrateRun(ctx context.Context, lgr *fileledger.Ledger, q *filequeue.Queue, s legacySnapshot) error {
	created := s.UpdatedAt
	if len(s.History) > 0 {
		created = s.History[0].At
	}
	input, err := json.Marshal(struct {
		Messages []byte `json:"messages"`
	}{s.Input})
	if err != nil {
		return err
	}
	if err := lgr.Accept(ctx, run.Run{
		ID: s.RunID, Name: workflowPrefix + s.Revision, Input: input, Status: run.StatusPending,
		MaxAttempts: math.MaxInt32, CreatedAt: created,
	}); err != nil {
		return err
	}
	rec := func(key, name string, kind run.Kind, output any, at time.Time) error {
		raw, err := json.Marshal(output)
		if err != nil {
			return err
		}
		_, err = lgr.Record(ctx, run.Record{
			RunID: s.RunID, Key: key, Kind: kind, Name: name, Codec: run.JSONCodec{}.ContentType(),
			Status: run.RecordOK, Output: raw, RecordedAt: at,
		})
		if errors.Is(err, run.ErrAlreadyRecorded) {
			return nil
		}
		return err
	}
	for name, st := range s.Steps {
		key := "step:" + name + "#0"
		switch {
		case st.Status == statusCompleted:
			if err := rec(key, "step:"+name, run.KindStep, st.Result, st.CompletedAt); err != nil {
				return err
			}
		case st.Idempotent:
			// Runs again on the next attempt, as before.
		default:
			if err := rec(key+"/dispatch#0", "dispatch", run.KindStep, 1, s.UpdatedAt); err != nil {
				return err
			}
			var pending types.StepResult
			if len(st.Result) > 0 && decode(st.Result, &pending) == nil && pending.Receipt != nil {
				receipt, err := encode(*pending.Receipt)
				if err != nil {
					return err
				}
				if err := rec(key+"/reserve#0", "reserve", run.KindStep, receipt, s.UpdatedAt); err != nil {
					return err
				}
			}
			msg := st.Error
			if msg == "" {
				msg = "an earlier attempt stopped during the step"
			}
			if err := rec("uncertain:"+name+"#0", "uncertain:"+name, run.KindStep, msg, s.UpdatedAt); err != nil {
				return err
			}
		}
	}
	for _, seg := range s.Inputs {
		name := fmt.Sprintf("input-%d", seg.Sequence)
		payload := struct {
			Key      string    `json:"key,omitempty"`
			At       time.Time `json:"at"`
			Messages []byte    `json:"messages"`
		}{seg.Key, seg.At, seg.Messages}
		if err := rec("event:"+name+"#0", name, run.KindEvent, payload, seg.At); err != nil {
			return err
		}
	}
	for id, in := range s.Interrupts {
		var posted struct {
			Interrupt types.Interrupt        `json:"interrupt"`
			Request   *types.ApprovalRequest `json:"request,omitempty"`
		}
		if in.Posted != nil {
			posted.Interrupt = *in.Posted
		} else {
			payload, err := json.Marshal(in.Request.ToolCall)
			if err != nil {
				return err
			}
			req := in.Request
			posted.Interrupt = types.Interrupt{ID: id, RunID: s.RunID, Kind: types.InterruptApproval, Payload: payload, Markers: req.Markers}
			posted.Request = &req
		}
		posted.Interrupt.ID, posted.Interrupt.RunID = id, s.RunID
		posted.Interrupt.CreatedAt, posted.Interrupt.ExpiresAt = in.CreatedAt, in.ExpiresAt
		if err := rec("interrupt:"+id+"#0", "interrupt:"+id, run.KindStep, posted, in.CreatedAt); err != nil {
			return err
		}
		if in.Decision == nil && len(in.Answer) == 0 {
			continue
		}
		var decision json.RawMessage
		if in.Decision != nil {
			if decision, err = json.Marshal(in.Decision); err != nil {
				return err
			}
		}
		reply := struct {
			Key      string          `json:"key"`
			Decision json.RawMessage `json:"decision,omitempty"`
			Answer   json.RawMessage `json:"answer,omitempty"`
		}{in.DecisionKey, decision, in.Answer}
		if err := rec("event:reply:"+id+"#0", "reply:"+id, run.KindEvent, reply, in.DecidedAt); err != nil {
			return err
		}
	}
	if s.Status == statusCancelled {
		err := lgr.Complete(ctx, s.RunID, 0, run.Result{Status: run.StatusFailed, Error: run.ErrCancelled.Error()})
		if err != nil && !errors.Is(err, run.ErrAlreadyTerminal) {
			return err
		}
		return nil
	}
	return q.Enqueue(ctx, s.RunID, 0)
}
