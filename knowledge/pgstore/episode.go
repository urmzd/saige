package pgstore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/urmzd/saige/knowledge/types"
)

// CreateEpisode creates an episode and links it to entities via mentions.
func (s *Store) CreateEpisode(ctx context.Context, input *types.EpisodeInput, entityUUIDs []string) (string, error) {
	episodeUUID := uuid.New().String()

	var episodeID int64
	err := s.pool.QueryRow(ctx, episodeCreateSQL,
		episodeUUID, input.Name, input.Body, input.Source, input.GroupID, encodeEpisodeMetadata(input.Metadata),
	).Scan(&episodeID)
	if err != nil {
		return "", fmt.Errorf("create episode %s: %w", input.Name, err)
	}

	for _, entUUID := range entityUUIDs {
		entID, err := s.entityID(ctx, entUUID)
		if err != nil {
			s.logger.Warn("create mention: entity not found", "uuid", entUUID, "error", err)
			continue
		}
		if _, err := s.pool.Exec(ctx, episodeMentionSQL, episodeID, entID); err != nil {
			s.logger.Warn("create mention failed", "episode", input.Name, "error", err)
		}
	}

	return episodeUUID, nil
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
