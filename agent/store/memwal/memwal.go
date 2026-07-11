// Package memwal implements agent/types.WAL with in-memory maps. It is
// intended for tests and offers no crash durability; production deployments
// should use agent/store/filewal instead.
package memwal

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/urmzd/saige/agent/types"
)

// txState tracks the state of an in-flight transaction.
type txState struct {
	ops       []types.TxOp
	committed bool
	aborted   bool
	applied   bool
}

// WAL is a WAL implementation backed by an in-memory map.
// Suitable for testing; offers no crash durability. For production use a
// durable implementation such as agent/store/filewal (append-only JSONL,
// fsync on commit).
type WAL struct {
	mu     sync.Mutex
	txns   map[types.TxID]*txState
	nextID uint64
}

// New creates a new in-memory WAL.
func New() *WAL {
	return &WAL{
		txns: make(map[types.TxID]*txState),
	}
}

func (w *WAL) Begin(_ context.Context) (types.TxID, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.nextID++
	id := types.TxID(fmt.Sprintf("tx-%d", w.nextID))
	w.txns[id] = &txState{}
	return id, nil
}

func (w *WAL) Append(_ context.Context, txID types.TxID, op types.TxOp) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	tx, ok := w.txns[txID]
	if !ok {
		return fmt.Errorf("unknown transaction: %s", txID)
	}
	if tx.committed || tx.aborted {
		return fmt.Errorf("transaction %s is already finalized", txID)
	}
	tx.ops = append(tx.ops, op)
	return nil
}

func (w *WAL) Commit(_ context.Context, txID types.TxID) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	tx, ok := w.txns[txID]
	if !ok {
		return fmt.Errorf("unknown transaction: %s", txID)
	}
	if tx.aborted {
		return fmt.Errorf("transaction %s was aborted", txID)
	}
	tx.committed = true
	return nil
}

func (w *WAL) Abort(_ context.Context, txID types.TxID) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	tx, ok := w.txns[txID]
	if !ok {
		return fmt.Errorf("unknown transaction: %s", txID)
	}
	tx.aborted = true
	return nil
}

// Recover returns committed transactions that have not been marked applied,
// in commit order.
func (w *WAL) Recover(_ context.Context) ([]types.TxID, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var committed []types.TxID
	for id, tx := range w.txns {
		if tx.committed && !tx.applied {
			committed = append(committed, id)
		}
	}
	// Map iteration order is random; tx IDs are sequential ("tx-N"), so sort
	// numerically to replay in commit order.
	sort.Slice(committed, func(i, j int) bool {
		return txSeq(committed[i]) < txSeq(committed[j])
	})
	return committed, nil
}

func txSeq(id types.TxID) uint64 {
	n, _ := strconv.ParseUint(strings.TrimPrefix(string(id), "tx-"), 10, 64)
	return n
}

// MarkApplied records that a committed transaction's ops were applied to the
// store, excluding it from future Recover results.
func (w *WAL) MarkApplied(_ context.Context, txID types.TxID) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	tx, ok := w.txns[txID]
	if !ok {
		return fmt.Errorf("unknown transaction: %s", txID)
	}
	if !tx.committed {
		return fmt.Errorf("transaction %s is not committed", txID)
	}
	tx.applied = true
	return nil
}

func (w *WAL) Replay(_ context.Context, txID types.TxID) ([]types.TxOp, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	tx, ok := w.txns[txID]
	if !ok {
		return nil, fmt.Errorf("unknown transaction: %s", txID)
	}
	ops := make([]types.TxOp, len(tx.ops))
	copy(ops, tx.ops)
	return ops, nil
}
