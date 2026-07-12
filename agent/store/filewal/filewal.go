// Package filewal implements agent/types.WAL as an append-only JSONL file,
// giving tree mutations crash durability (unlike the test-only
// agent/store/memwal).
//
// # File format
//
// One JSON record per line. Two record kinds are written:
//
//	{"kind":"commit","tx":"<id>","ops":[<op>...]} : a committed transaction
//	{"kind":"applied","tx":"<id>"}                : the tx was applied to a Store
//
// Ops buffer in memory between Begin and Commit; Commit writes the whole
// transaction as a single line and fsyncs before returning, so a record on
// disk is always a fully committed transaction. Abort simply drops the
// buffer: uncommitted transactions never touch the file. A crash mid-Commit
// leaves at most one truncated final line, which readers tolerate by ignoring
// an unparsable record at EOF.
//
// Recover returns transactions that have a commit record but no applied
// record, in file (= commit) order. Pair with
// agent/store/walrecover.RecoverWAL to heal a store after a crash between WAL
// commit and store write; RecoverWAL calls MarkApplied so healed transactions
// are not replayed again.
//
// Node messages are serialized with the same envelope mechanism the tree and
// pgstore use (tree.MarshalMessage / tree.UnmarshalMessage).
//
// # Growth and compaction
//
// The log grows for the lifetime of a session: the tree's normal-path writes
// append commit records but never remove them or mark them applied, so the
// file accumulates the full mutation history. Compact rewrites the log in
// place (temp file + fsync + rename), keeping only committed transactions
// that have not been marked applied. agent/store/walrecover.RecoverWAL calls
// Compact (via its optional Compactor interface) after a successful recovery
// pass, so each startup shrinks the log back to the transactions still
// awaiting application: typically none.
package filewal

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
)

var _ types.WAL = (*WAL)(nil)

// record is one JSONL line.
type record struct {
	Kind string  `json:"kind"` // "commit" | "applied"
	Tx   string  `json:"tx"`
	Ops  []walOp `json:"ops,omitempty"`
}

const (
	recordCommit  = "commit"
	recordApplied = "applied"
)

// walOp is the wire form of types.TxOp.
type walOp struct {
	Kind       string         `json:"kind"`
	NodeID     string         `json:"node_id,omitempty"`
	ParentID   string         `json:"parent_id,omitempty"`
	BranchID   string         `json:"branch_id,omitempty"`
	TipID      string         `json:"tip_id,omitempty"`
	Node       *walNode       `json:"node,omitempty"`
	Checkpoint *walCheckpoint `json:"checkpoint,omitempty"`
}

// walNode mirrors the tree's serialized node shape: the Message interface is
// stored as a role plus the JSON envelope from tree.MarshalMessage.
type walNode struct {
	ID         string          `json:"id"`
	ParentID   string          `json:"parent_id,omitempty"`
	Role       string          `json:"role"`
	Message    json.RawMessage `json:"message"`
	State      int             `json:"state"`
	Version    uint64          `json:"version"`
	Depth      int             `json:"depth"`
	BranchID   string          `json:"branch_id"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
	ArchivedAt *time.Time      `json:"archived_at,omitempty"`
	ArchivedBy string          `json:"archived_by,omitempty"`
	SummaryOf  []string        `json:"summary_of,omitempty"`
}

type walCheckpoint struct {
	ID        string    `json:"id"`
	Branch    string    `json:"branch"`
	NodeID    string    `json:"node_id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

func encodeOp(op types.TxOp) (walOp, error) {
	out := walOp{
		Kind:     string(op.Kind),
		NodeID:   string(op.NodeID),
		ParentID: string(op.ParentID),
		BranchID: string(op.BranchID),
		TipID:    string(op.TipID),
	}
	if op.Node != nil {
		msg, err := tree.MarshalMessage(op.Node.Message)
		if err != nil {
			return walOp{}, fmt.Errorf("marshal node %s message: %w", op.Node.ID, err)
		}
		summaryOf := make([]string, len(op.Node.SummaryOf))
		for i, s := range op.Node.SummaryOf {
			summaryOf[i] = string(s)
		}
		out.Node = &walNode{
			ID:         string(op.Node.ID),
			ParentID:   string(op.Node.ParentID),
			Role:       string(op.Node.Message.Role()),
			Message:    msg,
			State:      int(op.Node.State),
			Version:    op.Node.Version,
			Depth:      op.Node.Depth,
			BranchID:   string(op.Node.BranchID),
			CreatedAt:  op.Node.CreatedAt,
			UpdatedAt:  op.Node.UpdatedAt,
			ArchivedAt: op.Node.ArchivedAt,
			ArchivedBy: op.Node.ArchivedBy,
			SummaryOf:  summaryOf,
		}
	}
	if op.Checkpoint != nil {
		out.Checkpoint = &walCheckpoint{
			ID:        string(op.Checkpoint.ID),
			Branch:    string(op.Checkpoint.Branch),
			NodeID:    string(op.Checkpoint.NodeID),
			Name:      op.Checkpoint.Name,
			CreatedAt: op.Checkpoint.CreatedAt,
		}
	}
	return out, nil
}

func decodeOp(in walOp) (types.TxOp, error) {
	out := types.TxOp{
		Kind:     types.TxOpKind(in.Kind),
		NodeID:   types.NodeID(in.NodeID),
		ParentID: types.NodeID(in.ParentID),
		BranchID: types.BranchID(in.BranchID),
		TipID:    types.NodeID(in.TipID),
	}
	if in.Node != nil {
		msg, err := tree.UnmarshalMessage(types.Role(in.Node.Role), in.Node.Message)
		if err != nil {
			return types.TxOp{}, fmt.Errorf("unmarshal node %s message: %w", in.Node.ID, err)
		}
		summaryOf := make([]types.NodeID, len(in.Node.SummaryOf))
		for i, s := range in.Node.SummaryOf {
			summaryOf[i] = types.NodeID(s)
		}
		out.Node = &types.Node{
			ID:         types.NodeID(in.Node.ID),
			ParentID:   types.NodeID(in.Node.ParentID),
			Message:    msg,
			State:      types.NodeState(in.Node.State),
			Version:    in.Node.Version,
			Depth:      in.Node.Depth,
			BranchID:   types.BranchID(in.Node.BranchID),
			CreatedAt:  in.Node.CreatedAt,
			UpdatedAt:  in.Node.UpdatedAt,
			ArchivedAt: in.Node.ArchivedAt,
			ArchivedBy: in.Node.ArchivedBy,
			SummaryOf:  summaryOf,
		}
	}
	if in.Checkpoint != nil {
		out.Checkpoint = &types.Checkpoint{
			ID:        types.CheckpointID(in.Checkpoint.ID),
			Branch:    types.BranchID(in.Checkpoint.Branch),
			NodeID:    types.NodeID(in.Checkpoint.NodeID),
			Name:      in.Checkpoint.Name,
			CreatedAt: in.Checkpoint.CreatedAt,
		}
	}
	return out, nil
}

// walFile is the subset of *os.File the WAL uses for its append handle. It
// exists as a seam so tests can inject write failures; production code always
// uses an *os.File.
type walFile interface {
	io.Writer
	Sync() error
	Truncate(size int64) error
	Stat() (os.FileInfo, error)
	Close() error
}

// WAL is a file-backed write-ahead log. Safe for concurrent use within one
// process; the file must not be shared across processes concurrently.
type WAL struct {
	mu      sync.Mutex
	path    string
	f       walFile
	pending map[types.TxID][]types.TxOp
	// failed is set when the log could not be rolled back to a clean record
	// boundary (a partial record is stuck at EOF). Once set, every write
	// errors: appending after the corruption would poison the whole log,
	// because readRecords rejects bad lines anywhere but the tail.
	failed error
}

// New opens (creating if necessary) the JSONL WAL at path. A torn final line
// left by a crash mid-Commit is truncated away so later appends start on a
// fresh line; the torn transaction was never acknowledged, so nothing is lost.
func New(path string) (*WAL, error) {
	if err := repairTail(path); err != nil {
		return nil, fmt.Errorf("filewal: repair %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // path is the caller-chosen WAL location
	if err != nil {
		return nil, fmt.Errorf("filewal: open %s: %w", path, err)
	}
	return &WAL{
		path:    path,
		f:       f,
		pending: make(map[types.TxID][]types.TxOp),
	}, nil
}

// repairTail truncates a partial final line (one not terminated by '\n').
// Records are written newline-last in a single write, so a missing terminator
// means the commit never completed.
func repairTail(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0) //nolint:gosec // path is the caller-chosen WAL location
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	keep := int64(bytes.LastIndexByte(data, '\n') + 1)
	if keep == int64(len(data)) {
		return nil
	}
	return f.Truncate(keep)
}

// Close closes the underlying file. In-flight (uncommitted) transactions are
// discarded, matching crash semantics.
func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Close()
}

func (w *WAL) Begin(_ context.Context) (types.TxID, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	id := types.TxID(types.NewID())
	w.pending[id] = []types.TxOp{}
	return id, nil
}

func (w *WAL) Append(_ context.Context, txID types.TxID, op types.TxOp) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	ops, ok := w.pending[txID]
	if !ok {
		return fmt.Errorf("filewal: unknown or finalized transaction: %s", txID)
	}
	w.pending[txID] = append(ops, op)
	return nil
}

// Commit writes the transaction's ops as a single JSONL record and fsyncs.
func (w *WAL) Commit(_ context.Context, txID types.TxID) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	ops, ok := w.pending[txID]
	if !ok {
		return fmt.Errorf("filewal: unknown or finalized transaction: %s", txID)
	}

	rec := record{Kind: recordCommit, Tx: string(txID), Ops: make([]walOp, 0, len(ops))}
	for _, op := range ops {
		enc, err := encodeOp(op)
		if err != nil {
			return err
		}
		rec.Ops = append(rec.Ops, enc)
	}
	if err := w.writeRecord(rec); err != nil {
		return err
	}
	delete(w.pending, txID)
	return nil
}

func (w *WAL) Abort(_ context.Context, txID types.TxID) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.pending[txID]; !ok {
		return fmt.Errorf("filewal: unknown or finalized transaction: %s", txID)
	}
	delete(w.pending, txID)
	return nil
}

// MarkApplied records that a committed transaction's ops were applied to the
// store, excluding it from future Recover results.
func (w *WAL) MarkApplied(_ context.Context, txID types.TxID) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writeRecord(record{Kind: recordApplied, Tx: string(txID)})
}

// writeRecord appends one JSONL record and fsyncs. A short or failed write is
// rolled back by truncating to the pre-write size, so the log always ends at
// a record boundary; without the rollback the next successful append would
// land after the partial record, corrupting the log mid-file where neither
// repairTail nor readRecords can heal it. If the rollback truncate itself
// fails, the WAL is marked failed and refuses all further writes. Caller must
// hold the lock.
func (w *WAL) writeRecord(rec record) error {
	if w.failed != nil {
		return fmt.Errorf("filewal: wal disabled by unrecoverable write failure: %w", w.failed)
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("filewal: marshal record: %w", err)
	}
	line = append(line, '\n')

	info, err := w.f.Stat()
	if err != nil {
		return fmt.Errorf("filewal: stat before write: %w", err)
	}
	goodSize := info.Size()

	n, werr := w.f.Write(line)
	if werr == nil && n < len(line) {
		werr = io.ErrShortWrite
	}
	if werr != nil {
		if terr := w.f.Truncate(goodSize); terr != nil {
			w.failed = fmt.Errorf("write: %v; rollback truncate to %d: %v", werr, goodSize, terr)
			return fmt.Errorf("filewal: write: %w (rollback truncate failed: %v; wal disabled)", werr, terr)
		}
		// The handle is O_APPEND, so the next write lands at the restored end
		// of file: no seek needed.
		return fmt.Errorf("filewal: write: %w", werr)
	}
	if err := w.f.Sync(); err != nil {
		return fmt.Errorf("filewal: fsync: %w", err)
	}
	return nil
}

// Compact atomically rewrites the log, keeping only commit records for
// transactions that have not been marked applied; applied markers and the
// commit records they cover are dropped. After a successful
// walrecover.RecoverWAL every committed transaction has been applied, so
// compaction typically shrinks the log to empty.
//
// The rewrite goes to a temp file in the log's directory, which is fsynced
// and renamed over the log, so a crash during Compact leaves either the old
// or the new log: never a mix. A stray temp file left by such a crash is
// inert: New only ever opens the exact WAL path.
func (w *WAL) Compact(_ context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failed != nil {
		return fmt.Errorf("filewal: wal disabled by unrecoverable write failure: %w", w.failed)
	}

	recs, err := w.readRecords()
	if err != nil {
		return err
	}
	applied := make(map[string]bool)
	for _, r := range recs {
		if r.Kind == recordApplied {
			applied[r.Tx] = true
		}
	}

	dir := filepath.Dir(w.path)
	tmp, err := os.CreateTemp(dir, filepath.Base(w.path)+".compact-*")
	if err != nil {
		return fmt.Errorf("filewal: create compact temp: %w", err)
	}
	tmpPath := tmp.Name()
	fail := func(step string, err error) error {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("filewal: %s: %w", step, err)
	}
	for _, r := range recs {
		if r.Kind != recordCommit || applied[r.Tx] {
			continue
		}
		line, err := json.Marshal(r)
		if err != nil {
			return fail("marshal record", err)
		}
		line = append(line, '\n')
		if _, err := tmp.Write(line); err != nil {
			return fail("write compact temp", err)
		}
	}
	if err := tmp.Sync(); err != nil {
		return fail("fsync compact temp", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("filewal: close compact temp: %w", err)
	}
	if err := os.Rename(tmpPath, w.path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("filewal: rename compact temp: %w", err)
	}
	// Best-effort directory sync so the rename itself is durable.
	if d, err := os.Open(dir); err == nil { //nolint:gosec // dir derives from the caller-chosen WAL location
		_ = d.Sync()
		_ = d.Close()
	}

	// Swap the append handle onto the new file: the old handle points at the
	// now-unlinked inode, so writes through it would be lost.
	_ = w.f.Close()
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // path is the caller-chosen WAL location
	if err != nil {
		w.failed = fmt.Errorf("reopen after compact: %v", err)
		return fmt.Errorf("filewal: reopen after compact: %w (wal disabled)", err)
	}
	w.f = f
	return nil
}

// Recover returns committed transactions with no applied record, in commit
// (file) order.
func (w *WAL) Recover(_ context.Context) ([]types.TxID, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	recs, err := w.readRecords()
	if err != nil {
		return nil, err
	}

	applied := make(map[string]bool)
	for _, r := range recs {
		if r.Kind == recordApplied {
			applied[r.Tx] = true
		}
	}

	var out []types.TxID
	for _, r := range recs {
		if r.Kind == recordCommit && !applied[r.Tx] {
			out = append(out, types.TxID(r.Tx))
		}
	}
	return out, nil
}

// Replay returns the ops of a committed transaction.
func (w *WAL) Replay(_ context.Context, txID types.TxID) ([]types.TxOp, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	recs, err := w.readRecords()
	if err != nil {
		return nil, err
	}
	for _, r := range recs {
		if r.Kind != recordCommit || r.Tx != string(txID) {
			continue
		}
		ops := make([]types.TxOp, 0, len(r.Ops))
		for _, enc := range r.Ops {
			op, err := decodeOp(enc)
			if err != nil {
				return nil, err
			}
			ops = append(ops, op)
		}
		return ops, nil
	}
	return nil, fmt.Errorf("filewal: transaction not found: %s", txID)
}

// readRecords parses the log. A record that fails to parse at EOF is treated
// as a torn write from a crash mid-Commit and ignored; corruption anywhere
// else is an error. Caller must hold the lock.
func (w *WAL) readRecords() ([]record, error) {
	f, err := os.Open(w.path)
	if err != nil {
		return nil, fmt.Errorf("filewal: open for read: %w", err)
	}
	defer func() { _ = f.Close() }()

	var recs []record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	sawBadLine := false
	for sc.Scan() {
		if sawBadLine {
			// A parsable record after a bad line means real corruption, not a
			// torn final write.
			return nil, fmt.Errorf("filewal: corrupt record mid-log in %s", w.path)
		}
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec record
		if err := json.Unmarshal(line, &rec); err != nil {
			sawBadLine = true
			continue
		}
		recs = append(recs, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("filewal: scan: %w", err)
	}
	return recs, nil
}
