ALTER TABLE knowledges ADD COLUMN is_staged BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE knowledges ADD COLUMN replaces_knowledge_id VARCHAR(36);
CREATE UNIQUE INDEX idx_knowledge_staged_replacement ON knowledges(replaces_knowledge_id)
WHERE is_staged = TRUE AND deleted_at IS NULL AND replaces_knowledge_id IS NOT NULL;
CREATE UNIQUE INDEX idx_knowledge_dokuwiki_published_source
ON knowledges(tenant_id, knowledge_base_id, (metadata->>'datasource_id'), (metadata->>'external_id'))
WHERE channel = 'dokuwiki_filesystem' AND is_staged = FALSE AND deleted_at IS NULL;
