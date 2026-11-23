-- Abort rollback while hidden candidates exist: an older binary has no barrier.
CREATE TEMP TABLE staging_rollback_guard (n INTEGER CHECK (n = 0));
INSERT INTO staging_rollback_guard SELECT COUNT(*) FROM knowledges WHERE is_staged = TRUE AND deleted_at IS NULL;
DROP TABLE staging_rollback_guard;
DROP INDEX IF EXISTS idx_knowledge_staged_replacement;
DROP INDEX IF EXISTS idx_knowledge_dokuwiki_published_source;
ALTER TABLE knowledges DROP COLUMN replaces_knowledge_id;
ALTER TABLE knowledges DROP COLUMN is_staged;
