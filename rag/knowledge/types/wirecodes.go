package types

import agenttypes "github.com/urmzd/saige/agent/types"

// The wire codes of this package's sentinel errors, so errors.Is holds for
// them after an error crosses a process boundary (see
// types.RegisterWireSentinel). Codes are part of the wire contract: never
// rename one.
func init() {
	agenttypes.RegisterWireSentinel("rag.knowledge.no_embedder", ErrNoEmbedder)
	agenttypes.RegisterWireSentinel("rag.knowledge.no_extractor", ErrNoExtractor)
	agenttypes.RegisterWireSentinel("rag.knowledge.node_not_found", ErrNodeNotFound)
	agenttypes.RegisterWireSentinel("rag.knowledge.partial_episode", ErrPartialEpisode)
	agenttypes.RegisterWireSentinel("rag.knowledge.store_not_ready", ErrStoreNotReady)
}
