package github

import (
	"context"
	"fmt"
	"github.com/urmzd/saige/flow/address"
	"slices"
)

func (f *Fake) ListReactions(_ context.Context, target address.Address, page int) ([]NativeReaction, error) {
	key, err := reactionPath(target)
	if err != nil {
		return nil, err
	}
	if page < 1 || page > 100 {
		return nil, fmt.Errorf("github: invalid reaction page")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	all := f.reactions[key]
	start := (page - 1) * 100
	if start >= len(all) {
		return nil, nil
	}
	return slices.Clone(all[start:min(start+100, len(all))]), nil
}
func (f *Fake) AddReaction(_ context.Context, target address.Address, content string) (int64, error) {
	return f.SeedReaction(target, content, "humagen[bot]", "Bot")
}
func (f *Fake) SeedReaction(target address.Address, content, login, kind string) (int64, error) {
	key, err := reactionPath(target)
	if err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return 0, f.Err
	}
	if f.reactions == nil {
		f.reactions = map[string][]NativeReaction{}
	}
	for _, old := range f.reactions[key] {
		if old.Content == content && old.User.Login == login {
			return old.ID, nil
		}
	}
	f.nextID++
	v := NativeReaction{ID: f.nextID, Content: content}
	v.User.Login = login
	v.User.Type = kind
	f.reactions[key] = append(f.reactions[key], v)
	return v.ID, nil
}
func (f *Fake) RemoveReaction(_ context.Context, target address.Address, id int64) error {
	key, err := reactionPath(target)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	f.reactions[key] = slices.DeleteFunc(f.reactions[key], func(v NativeReaction) bool { return v.ID == id })
	return nil
}

var _ ReactionAPI = (*Fake)(nil)
var _ ReactionAPI = (*HTTPAPI)(nil)
