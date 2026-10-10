package agent

import (
	"errors"
	"fmt"
	"sync"

	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
)

// ErrRunActive means another run is already active on the requested branch
// of the same tree. Two runs on one branch would interleave their messages
// and break tool_use and tool_result pairing, so the second is refused rather
// than queued.
var ErrRunActive = errors.New("a run is already active on this branch")

// runGuard records which trees and branches are in use. The claims are keyed
// by the tree rather than the Agent because separate Agents, handoff members,
// and hosts can share one tree. One mutex covers both maps so a branch claim
// and a whole-tree claim can never be granted at the same time.
type runGuard struct {
	mu       sync.Mutex
	branches map[*tree.Tree]map[types.BranchID]struct{}
	trees    map[*tree.Tree]struct{}
	// streams maps a claimed branch to the stream of its run, when the run
	// accepts submitted messages.
	streams map[*tree.Tree]map[types.BranchID]*EventStream
}

var activeRuns = runGuard{
	branches: map[*tree.Tree]map[types.BranchID]struct{}{},
	trees:    map[*tree.Tree]struct{}{},
	streams:  map[*tree.Tree]map[types.BranchID]*EventStream{},
}

// claimBranch reserves branch of t for one run. It fails while another run
// holds the branch or while the whole tree is claimed. The caller must call
// release when the run ends; release is safe to call more than once.
func claimBranch(t *tree.Tree, branch types.BranchID) (release func(), err error) {
	c, err := newRunClaim(t, branch, nil)
	if err != nil {
		return nil, err
	}
	return c.release, nil
}

// runClaim is the set of branches one run holds. When the run's stream takes
// submitted messages, each held branch maps to it until release, which also
// closes s.released. Claim and registration change under one lock, so a held
// branch always maps to its current run. A run that moves to a new
// branch, as compaction does, adds it to the claim, so the branch it now
// writes is never handed to a second run. release frees every branch.
type runClaim struct {
	t        *tree.Tree
	s        *EventStream
	branches []types.BranchID
	once     sync.Once
}

func newRunClaim(t *tree.Tree, branch types.BranchID, s *EventStream) (*runClaim, error) {
	g := &activeRuns
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, held := g.trees[t]; held {
		return nil, fmt.Errorf("%w: %s", ErrRunActive, branch)
	}
	c := &runClaim{t: t, s: s}
	if err := c.addLocked(g, branch); err != nil {
		return nil, err
	}
	return c, nil
}

// add extends the claim to branch. It fails when another run holds it.
// Claiming a branch the run already holds is a no-op.
func (c *runClaim) add(branch types.BranchID) error {
	if c == nil {
		return nil
	}
	g := &activeRuns
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, b := range c.branches {
		if b == branch {
			return nil
		}
	}
	return c.addLocked(g, branch)
}

func (c *runClaim) addLocked(g *runGuard, branch types.BranchID) error {
	held := g.branches[c.t]
	if _, busy := held[branch]; busy {
		return fmt.Errorf("%w: %s", ErrRunActive, branch)
	}
	if held == nil {
		held = map[types.BranchID]struct{}{}
		g.branches[c.t] = held
	}
	held[branch] = struct{}{}
	if c.s != nil {
		if g.streams[c.t] == nil {
			g.streams[c.t] = map[types.BranchID]*EventStream{}
		}
		g.streams[c.t][branch] = c.s
	}
	c.branches = append(c.branches, branch)
	return nil
}

// release frees every branch of the claim. It is safe to call more than once.
func (c *runClaim) release() {
	c.once.Do(func() {
		g := &activeRuns
		g.mu.Lock()
		defer g.mu.Unlock()
		for _, b := range c.branches {
			delete(g.branches[c.t], b)
			if c.s != nil {
				delete(g.streams[c.t], b)
			}
		}
		if len(g.branches[c.t]) == 0 {
			delete(g.branches, c.t)
		}
		if c.s != nil {
			if len(g.streams[c.t]) == 0 {
				delete(g.streams, c.t)
			}
			close(c.s.released)
		}
	})
}

// holder reports whether branch of t is held, by a branch claim or a claim
// of the whole tree, and returns the stream of the holding run when it takes
// submitted messages.
func (g *runGuard) holder(t *tree.Tree, branch types.BranchID) (s *EventStream, held bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.trees[t]; ok {
		return nil, true
	}
	if _, ok := g.branches[t][branch]; !ok {
		return nil, false
	}
	return g.streams[t][branch], true
}

// claimTree reserves all of t, for work that replaces the tree's contents.
// It fails while any branch of t has a run or the tree is already claimed,
// and blocks new branch claims until release is called.
func claimTree(t *tree.Tree) (release func(), err error) {
	g := &activeRuns
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, held := g.trees[t]; held || len(g.branches[t]) > 0 {
		return nil, ErrRunActive
	}
	g.trees[t] = struct{}{}
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			delete(g.trees, t)
		})
	}, nil
}
