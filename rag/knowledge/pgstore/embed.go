package pgstore

import _ "embed"

// Entity queries.
//
//go:embed sql/entity_upsert.sql
var entityUpsertSQL string

//go:embed sql/entity_get.sql
var entityGetSQL string

//go:embed sql/entity_find_by_name_type.sql
var entityFindByNameTypeSQL string

//go:embed sql/entity_find_by_name_type_group.sql
var entityFindByNameTypeGroupSQL string

//go:embed sql/entity_find_fuzzy.sql
var entityFindFuzzySQL string

//go:embed sql/entity_find_fuzzy_group.sql
var entityFindFuzzyGroupSQL string

//go:embed sql/entity_id.sql
var entityIDSQL string

// Relation queries.
//
//go:embed sql/relation_create.sql
var relationCreateSQL string

//go:embed sql/relation_invalidate.sql
var relationInvalidateSQL string

//go:embed sql/relation_find_between.sql
var relationFindBetweenSQL string

//go:embed sql/relation_neighbors.sql
var relationNeighborsSQL string

// Graph queries.
//
//go:embed sql/graph_get.sql
var graphGetSQL string

//go:embed sql/graph_fact_provenance.sql
var graphFactProvenanceSQL string

// Episode queries.
//
//go:embed sql/episode_create.sql
var episodeCreateSQL string

//go:embed sql/episode_mention.sql
var episodeMentionSQL string

//go:embed sql/episode_delete_group.sql
var episodeDeleteGroupSQL string

// Search queries.
//
//go:embed sql/search_embedding_group.sql
var searchEmbeddingGroupSQL string

//go:embed sql/search_embedding.sql
var searchEmbeddingSQL string

//go:embed sql/search_text_group.sql
var searchTextGroupSQL string

//go:embed sql/search_text.sql
var searchTextSQL string
