DO $$ BEGIN
IF EXISTS (SELECT 1 FROM knowledges WHERE is_staged = TRUE AND deleted_at IS NULL) THEN
RAISE EXCEPTION 'Remove staged datasource candidates before removing the publication barrier';
END IF;
END $$;
DROP INDEX IF EXISTS idx_knowledge_staged_replacement;
DROP INDEX IF EXISTS idx_knowledge_dokuwiki_published_source;
ALTER TABLE knowledges DROP COLUMN IF EXISTS replaces_knowledge_id;
ALTER TABLE knowledges DROP COLUMN IF EXISTS is_staged;
