package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/deliverystate"
	"github.com/urmzd/saige/flow/event"
)

// ReviewThreadAPI proves current resolution state and comment membership. The
// webhook has no resolution timestamp, and its delivery GUID is not an order.
type ReviewThreadAPI interface {
	ReviewThread(context.Context, Ref, string) (ReviewThreadState, error)
}

type ReviewThreadState struct {
	Resolved   bool
	ResolvedBy string
	Comments   []int64
}

func (a *HTTPAPI) ReviewThread(ctx context.Context, ref Ref, node string) (ReviewThreadState, error) {
	if err := validReadRef(ref, 1); err != nil {
		return ReviewThreadState{}, err
	}
	if node == "" || len(node) > 256 {
		return ReviewThreadState{}, fmt.Errorf("github: invalid review thread")
	}
	request := map[string]any{"query": `query($id:ID!){node(id:$id){... on PullRequestReviewThread{id isResolved resolvedBy{login} pullRequest{number repository{name owner{login}}} comments(first:100){nodes{databaseId}}}}}`, "variables": map[string]string{"id": node}}
	var response struct {
		Data struct {
			Node *struct {
				ID         string `json:"id"`
				Resolved   bool   `json:"isResolved"`
				ResolvedBy struct {
					Login string `json:"login"`
				} `json:"resolvedBy"`
				Pull struct {
					Number     int        `json:"number"`
					Repository repository `json:"repository"`
				} `json:"pullRequest"`
				Comments struct {
					Nodes []struct {
						ID int64 `json:"databaseId"`
					} `json:"nodes"`
				} `json:"comments"`
			} `json:"node"`
		} `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	client := *a.client
	if strings.HasSuffix(client.Base, "/api/v3") {
		client.Base = strings.TrimSuffix(client.Base, "/v3")
	}
	if err := client.Do(ctx, http.MethodPost, "/graphql", request, &response); err != nil {
		return ReviewThreadState{}, err
	}
	n := response.Data.Node
	if len(response.Errors) > 0 || n == nil || n.ID != node || n.Pull.Number != ref.Number || !strings.EqualFold(n.Pull.Repository.Owner.Login, ref.Owner) || !strings.EqualFold(n.Pull.Repository.Name, ref.Repo) || len(n.Comments.Nodes) > 100 {
		return ReviewThreadState{}, fmt.Errorf("github: review thread proof unavailable or outside pull request")
	}
	out := ReviewThreadState{Resolved: n.Resolved, ResolvedBy: n.ResolvedBy.Login}
	for _, comment := range n.Comments.Nodes {
		out.Comments = append(out.Comments, comment.ID)
	}
	return out, nil
}

func (f *Fake) SeedReviewThread(ref Ref, node string, state ReviewThreadState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reviewThreads == nil {
		f.reviewThreads = map[string]ReviewThreadState{}
	}
	state.Comments = slices.Clone(state.Comments)
	f.reviewThreads[ref.String()+"/"+node] = state
}

func (f *Fake) ReviewThread(_ context.Context, ref Ref, node string) (ReviewThreadState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return ReviewThreadState{}, f.Err
	}
	state, ok := f.reviewThreads[ref.String()+"/"+node]
	if !ok {
		return ReviewThreadState{}, fmt.Errorf("github: unknown review thread")
	}
	state.Comments = slices.Clone(state.Comments)
	return state, nil
}

type resolutionState struct {
	Source string         `json:"source,omitempty"`
	Value  event.Feedback `json:"value"`
	Seen   []string       `json:"seen"`
}

func (c *Connector) recordResolution(ctx context.Context, target address.Address, node string, comment int64, id string, value event.Feedback) error {
	api, ok := c.cfg.API.(ReviewThreadAPI)
	if !ok {
		return fmt.Errorf("github: resolution feedback needs current thread proof")
	}
	ref, err := c.ref(target)
	if err != nil {
		return err
	}
	// A push changes head_sha without changing the native review thread.
	canonical := refAddress(Ref{Owner: strings.ToLower(ref.Owner), Repo: strings.ToLower(ref.Repo), Number: ref.Number}, "pull").String()
	key := deliverystate.Key{Tenant: c.cfg.Tenant, Scope: "resolution:" + deliveryScope(c.cfg.Tenant, c.cfg.DeliveryScope), Run: value.RunID, Channel: canonical, Target: fmt.Sprintf("%s:%d", node, comment)}
	return c.feedbackState.Update(ctx, key, func(raw []byte) ([]byte, bool, error) {
		var state resolutionState
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &state); err != nil {
				return nil, false, err
			}
		}
		if slices.Contains(state.Seen, id) {
			return raw, false, nil
		}
		if len(state.Seen) >= 1024 {
			return nil, false, fmt.Errorf("github: resolution history exceeds 1024 deliveries")
		}
		current, err := api.ReviewThread(ctx, ref, node)
		if err != nil {
			return nil, false, err
		}
		state.Seen = append(state.Seen, id)
		if slices.Contains(current.Comments, comment) && current.Resolved != value.Removed {
			if value.Removed && state.Source != "" {
				// A different person may reopen the thread. Withdraw the original
				// resolver's assessment, never an assessment belonging to the reopener.
				withdrawn := state.Value
				withdrawn.Removed = true
				if err := c.cfg.Feedback.Withdraw(ctx, c.cfg.Tenant, id, state.Source, withdrawn); err != nil {
					return nil, false, err
				}
				state.Source = ""
			} else if !value.Removed && state.Source == "" && strings.EqualFold(current.ResolvedBy, value.Actor.ID) {
				if err := c.cfg.Feedback.Record(ctx, c.cfg.Tenant, id, value); err != nil {
					return nil, false, err
				}
				state.Source, state.Value = id, value
			}
		}
		data, err := json.Marshal(state)
		return data, false, err
	})
}
