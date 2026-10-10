package types

import agenttypes "github.com/urmzd/saige/agent/types"

// The wire codes of this package's sentinel errors, so errors.Is holds for
// them after an error crosses a process boundary (see
// types.RegisterWireSentinel). Codes are part of the wire contract: never
// rename one.
func init() {
	agenttypes.RegisterWireSentinel("rag.document_not_found", ErrDocumentNotFound)
	agenttypes.RegisterWireSentinel("rag.duplicate_document", ErrDuplicateDocument)
	agenttypes.RegisterWireSentinel("rag.embedding_shape", ErrEmbeddingShape)
	agenttypes.RegisterWireSentinel("rag.invalid_keyword_query", ErrInvalidKeywordQuery)
	agenttypes.RegisterWireSentinel("rag.no_extractor", ErrNoExtractor)
	agenttypes.RegisterWireSentinel("rag.no_retriever", ErrNoRetriever)
	agenttypes.RegisterWireSentinel("rag.no_store", ErrNoStore)
	agenttypes.RegisterWireSentinel("rag.partial_ingest", ErrPartialIngest)
	agenttypes.RegisterWireSentinel("rag.partial_search", ErrPartialSearch)
	agenttypes.RegisterWireSentinel("rag.scope_mismatch", ErrScopeMismatch)
	agenttypes.RegisterWireSentinel("rag.sync_unsupported", ErrSyncUnsupported)
	agenttypes.RegisterWireSentinel("rag.unsupported_mime_type", ErrUnsupportedMIMEType)
	agenttypes.RegisterWireSentinel("rag.variant_not_found", ErrVariantNotFound)
	agenttypes.RegisterWireSentinel("rag.variant_part", ErrVariantPart)
}
