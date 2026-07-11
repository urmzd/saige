package engine

import (
	"context"
	"testing"

	"github.com/urmzd/saige/knowledge/types"
)

var _ types.EpisodeDeleter = (*GraphEngine)(nil)

// deletingMockStore extends mockStore with types.EpisodeDeleter.
type deletingMockStore struct {
	*mockStore
	deleted []string
}

func (m *deletingMockStore) DeleteEpisodes(_ context.Context, groupID string) error {
	m.deleted = append(m.deleted, groupID)
	return nil
}

func TestDeleteEpisodesDelegatesToStore(t *testing.T) {
	store := &deletingMockStore{mockStore: newMockStore()}
	e := New(WithStore(store))

	if err := e.DeleteEpisodes(context.Background(), "doc-123"); err != nil {
		t.Fatalf("DeleteEpisodes: %v", err)
	}
	if len(store.deleted) != 1 || store.deleted[0] != "doc-123" {
		t.Errorf("deleted groups = %v, want [doc-123]", store.deleted)
	}
}

// A store without EpisodeDeleter must error, never silently no-op.
func TestDeleteEpisodesUnsupportedStoreErrors(t *testing.T) {
	e := New(WithStore(newMockStore()))
	if err := e.DeleteEpisodes(context.Background(), "doc-123"); err == nil {
		t.Fatal("DeleteEpisodes with non-deleting store = nil, want error")
	}
}

func TestDeleteEpisodesNoStore(t *testing.T) {
	e := New()
	if err := e.DeleteEpisodes(context.Background(), "doc-123"); err == nil {
		t.Fatal("DeleteEpisodes without store = nil, want error")
	}
}
