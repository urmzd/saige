INSERT INTO rag_variant (uuid, section_id, content_type, mime_type, data, text, embedding, metadata, section_heading, document_title)
SELECT $1::text, s.id, $3::text, $4::text, $5::bytea, $6::text, $7::vector, $8::jsonb, s.heading, d.title
FROM rag_section s
JOIN rag_document d ON d.id = s.document_id
WHERE s.id = $2
