package pgstore

import _ "embed"

// Document queries.
//
//go:embed sql/document_create.sql
var documentCreateSQL string

//go:embed sql/document_get.sql
var documentGetSQL string

//go:embed sql/document_find_fingerprint.sql
var documentFindFingerprintSQL string

//go:embed sql/document_delete.sql
var documentDeleteSQL string

//go:embed sql/document_id.sql
var documentIDSQL string

// Original queries.
//
//go:embed sql/original_upsert.sql
var originalUpsertSQL string

//go:embed sql/original_get.sql
var originalGetSQL string

// Section queries.
//
//go:embed sql/section_create.sql
var sectionCreateSQL string

//go:embed sql/section_get.sql
var sectionGetSQL string

//go:embed sql/section_id.sql
var sectionIDSQL string

// Variant queries.
//
//go:embed sql/variant_create.sql
var variantCreateSQL string

//go:embed sql/variant_update_embedding.sql
var variantUpdateEmbeddingSQL string

//go:embed sql/variant_get.sql
var variantGetSQL string

//go:embed sql/variant_get_record.sql
var variantGetRecordSQL string

//go:embed sql/variant_get_records.sql
var variantGetRecordsSQL string

//go:embed sql/document_list.sql
var documentListSQL string

//go:embed sql/document_find_source.sql
var documentFindSourceSQL string

//go:embed sql/document_list_source.sql
var documentListSourceSQL string
