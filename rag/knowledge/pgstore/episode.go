package pgstore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/urmzd/saige/rag/knowledge/types"
)

// CreateEpisode creates an episode and links it to entities via mentions.
// Entity UUIDs with no stored entity are skipped.
func (s *Store) CreateEpisode(ctx context.Context, input *types.EpisodeInput, entityUUIDs []string) (string, error) {
	episodeUUID := uuid.New().String()

	var episodeID int64
	err := s.pool.QueryRow(ctx, episodeCreateSQL,
		episodeUUID, input.Name, input.Body, input.Source, input.GroupID, input.DocumentID,
		encodeEpisodeMetadata(input.Metadata),
	).Scan(&episodeID)
	if err != nil {
		return "", fmt.Errorf("create episode %s: %w", input.Name, err)
	}

	if len(entityUUIDs) > 0 {
		if err := s.LinkEpisodeEntities(ctx, episodeUUID, entityUUIDs); err != nil {
			return episodeUUID, fmt.Errorf("create episode %s: %w", input.Name, err)
		}
	}

	return episodeUUID, nil
}

// LinkEpisodeEntities implements types.EpisodeLinker: it records mentions of
// the entities by the episode in one statement. Existing mentions and
// unknown entity UUIDs are ignored.
func (s *Store) LinkEpisodeEntities(ctx context.Context, episodeUUID string, entityUUIDs []string) error {
	if len(entityUUIDs) == 0 {
		return nil
	}
	if _, err := s.pool.Exec(ctx, episodeMentionSQL, episodeUUID, entityUUIDs); err != nil {
		return fmt.Errorf("link episode %s entities: %w", episodeUUID, err)
	}
	return nil
}

// LinkRelationEpisode implements types.EpisodeLinker: it records that the
// episode asserts the relation. Linking twice is a no-op.
func (s *Store) LinkRelationEpisode(ctx context.Context, relationUUID, episodeUUID string) error {
	if _, err := s.pool.Exec(ctx, relationEpisodeLinkSQL, relationUUID, episodeUUID); err != nil {
		return fmt.Errorf("link relation %s to episode %s: %w", relationUUID, episodeUUID, err)
	}
	return nil
}

// DeleteDocumentEpisodes implements types.DocumentEpisodeDeleter. In one
// transaction it removes the document's episodes in the group (mentions and
// relation links cascade), the relations no other episode asserts, and the
// entities those episodes mentioned that have no mentions or relations left.
// Relations that a removed relation had superseded, or that were backfilled
// behind it, get their end recomputed from the relations that remain, so
// another document's fact becomes current again once its only contradiction
// is gone. The default group ("") is allowed because the delete is scoped to one
// document.
func (s *Store) DeleteDocumentEpisodes(ctx context.Context, groupID, documentID string) error {
	if documentID == "" {
		return fmt.Errorf("delete document episodes: document id must not be empty")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("delete document episodes %s: %w", documentID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		deleted  int64
		entities []int64
	)
	if err := tx.QueryRow(ctx, episodeDeleteDocumentSQL, groupID, documentID).Scan(&deleted, &entities); err != nil {
		return fmt.Errorf("delete document episodes %s: %w", documentID, err)
	}
	if len(entities) > 0 {
		if _, err := tx.Exec(ctx, entityDeleteOrphansSQL, entities); err != nil {
			return fmt.Errorf("delete document episodes %s: remove orphan entities: %w", documentID, err)
		}
	}
	return tx.Commit(ctx)
}

// DeleteEpisodes implements types.EpisodeDeleter: it removes a group's
// episodes, relations, and entities in one transaction. Mentions cascade via
// FK. The default group ("") is rejected: it holds all legacy single-tenant
// data.
func (s *Store) DeleteEpisodes(ctx context.Context, groupID string) error {
	if groupID == "" {
		return fmt.Errorf("delete episodes: group id must not be empty")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("delete episodes %s: %w", groupID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The embedded file holds one statement per line; pgx's extended protocol
	// only accepts a single statement per Exec.
	for _, stmt := range strings.Split(episodeDeleteGroupSQL, ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := tx.Exec(ctx, stmt, groupID); err != nil {
			return fmt.Errorf("delete episodes %s: %w", groupID, err)
		}
	}
	return tx.Commit(ctx)
}

// encodeEpisodeMetadata marshals episode metadata to JSON for the JSONB
// column. Empty metadata is stored as NULL so it round-trips back to nil.
func encodeEpisodeMetadata(meta map[string]string) []byte {
	if len(meta) == 0 {
		return nil
	}
	b, _ := json.Marshal(meta)
	return b
}

// decodeEpisodeMetadata unmarshals JSONB bytes to a metadata map. NULL and
// malformed values decode to nil.
func decodeEpisodeMetadata(b []byte) map[string]string {
	if len(b) == 0 {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	if len(m) == 0 {
		return nil
	}
	return m
}
