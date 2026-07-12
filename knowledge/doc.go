// Package knowledge builds and queries knowledge graphs with LLM-powered
// entity extraction, fuzzy deduplication, temporal relation tracking, and
// hybrid search combining vector similarity and full-text ranking via
// Reciprocal Rank Fusion.
//
// Construct a graph with NewGraph and functional options such as
// WithPostgres, WithExtractor, and WithEmbedder. The Graph interface and
// core types (Entity, Relation, Fact, Episode, Ontology) are defined in the
// knowledge/types package.
package knowledge
