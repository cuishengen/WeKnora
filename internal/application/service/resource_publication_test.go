package service

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

func TestResourcePublicationBarrier(t *testing.T) {
	catalog, db := newResourceCatalogForTest(t)
	require.NoError(t, db.Exec(`CREATE TABLE knowledges (id TEXT PRIMARY KEY, tenant_id INTEGER,
		knowledge_base_id TEXT, file_path TEXT, is_staged BOOLEAN NOT NULL DEFAULT false, deleted_at DATETIME)`).Error)
	require.NoError(t, db.Exec(`CREATE TABLE knowledge_bases (id TEXT PRIMARY KEY, tenant_id INTEGER, deleted_at DATETIME)`).Error)
	require.NoError(t, db.Exec(`INSERT INTO knowledge_bases(id, tenant_id) VALUES ('kb',7),('pending-kb',7)`).Error)
	check := catalog.(interface {
		IsResourcePublishedForRead(context.Context, string, uint64) (bool, error)
	})
	ctx := context.Background()
	const physical = "local://7/docs/candidate.pdf"
	ref, err := catalog.Register(ctx, 7, physical, interfaces.ResourceRegistration{})
	require.NoError(t, err)
	assertReadable := func(reference string, tenant uint64, want bool) {
		t.Helper()
		got, err := check.IsResourcePublishedForRead(ctx, reference, tenant)
		require.NoError(t, err)
		require.Equal(t, want, got, reference)
	}
	// Ordinary chat artifacts remain governed by their existing ACL.
	require.NoError(t, catalog.Bind(ctx, ref, types.ResourceOwnerMessage, "msg", types.ResourceRelationArtifact))
	assertReadable(ref, 7, true)
	assertReadable(physical, 7, true)
	assertReadable(ref, 8, false)
	assertReadable("local://7/docs/unregistered-chat.pdf", 7, false)
	// A published legacy source remains readable even without a catalog row.
	require.NoError(t, db.Exec(`INSERT INTO knowledges(id,tenant_id,knowledge_base_id,file_path,is_staged)
		VALUES ('legacy-published',7,'kb','local://7/docs/published-legacy.pdf',false)`).Error)
	assertReadable("local://7/docs/published-legacy.pdf", 7, true)
	// SaveFile binds before the knowledge row is created: no transient read.
	require.NoError(t, catalog.Bind(ctx, ref, types.ResourceOwnerKnowledge, "candidate", types.ResourceRelationSourceFile))
	assertReadable(ref, 7, false)
	require.NoError(t, db.Exec(`INSERT INTO knowledges(id,tenant_id,knowledge_base_id,file_path,is_staged)
		VALUES ('candidate',7,'pending-kb',?,true)`, ref).Error)
	assertReadable(ref, 7, false)
	assertReadable(physical, 7, false)
	// Deduplicated bytes may also belong to a live published document.
	require.NoError(t, db.Exec(`INSERT INTO knowledges(id,tenant_id,knowledge_base_id,file_path,is_staged)
		VALUES ('published',7,'kb','',false)`).Error)
	require.NoError(t, catalog.Bind(ctx, ref, types.ResourceOwnerKnowledge, "published", types.ResourceRelationExtractedImage))
	assertReadable(ref, 7, true)
	assertReadable(physical, 7, true)
	// A published binding elsewhere cannot turn the staged document's KB into
	// an authorization path for those shared bytes.
	lookup := catalog.(interfaces.KBResourceLookup)
	bound, err := lookup.IsReferencedByKnowledgeBase(ctx, 7, "pending-kb", ref)
	require.NoError(t, err)
	require.False(t, bound)
	messageLookup := catalog.(interfaces.MessageFileBindingLookup)
	messageBindings, err := messageLookup.GetMessageFileBindings(ctx, 7, ref, "")
	require.NoError(t, err)
	require.Equal(t, []string{"kb"}, messageBindings.KnowledgeBaseIDs)
	require.NoError(t, db.Exec(`UPDATE knowledges SET deleted_at=CURRENT_TIMESTAMP WHERE id='published'`).Error)
	assertReadable(ref, 7, false)
	// Publishing the candidate switches access without changing the handle.
	require.NoError(t, db.Exec(`UPDATE knowledges SET is_staged=false WHERE id='candidate'`).Error)
	assertReadable(ref, 7, true)
	require.NoError(t, db.Exec(`UPDATE knowledge_bases SET deleted_at=CURRENT_TIMESTAMP WHERE id='pending-kb'`).Error)
	assertReadable(ref, 7, true)
	// A generated message artifact survives deletion of its published KB;
	// releasing that independent claim removes the compatibility exception.
	_, err = catalog.Release(ctx, ref, types.ResourceOwnerMessage, "msg")
	require.NoError(t, err)
	assertReadable(ref, 7, false)
	// Legacy unregistered source locators must not bypass publication.
	require.NoError(t, db.Exec(`INSERT INTO knowledges(id,tenant_id,knowledge_base_id,file_path,is_staged)
		VALUES ('legacy',7,'kb','local://7/docs/legacy.pdf',true)`).Error)
	assertReadable("local://7/docs/legacy.pdf", 7, false)
	// Storage/query errors fail closed rather than treating the file as chat.
	require.NoError(t, db.Exec(`DROP TABLE knowledges`).Error)
	got, err := check.IsResourcePublishedForRead(ctx, physical, 7)
	require.Error(t, err)
	require.False(t, got)
}

func TestResourceRegistrationCommitsKnowledgeBindingAtomically(t *testing.T) {
	catalog, db := newResourceCatalogForTest(t)
	ctx := context.Background()
	meta := interfaces.ResourceRegistration{OwnerType: types.ResourceOwnerKnowledge,
		OwnerID: "candidate", Relation: types.ResourceRelationExtractedImage}
	// A failed binding cannot leave a readable unbound resource behind.
	require.NoError(t, db.Exec(`CREATE TRIGGER reject_owner BEFORE INSERT ON resource_bindings
		BEGIN SELECT RAISE(ABORT, 'owner binding failed'); END`).Error)
	_, err := catalog.Register(ctx, 7, "local://7/images/new.png", meta)
	require.Error(t, err)
	var count int64
	require.NoError(t, db.Model(&types.StoredResource{}).Count(&count).Error)
	require.Zero(t, count)
	require.NoError(t, db.Exec(`DROP TRIGGER reject_owner`).Error)
	ref, err := catalog.Register(ctx, 7, "local://7/images/new.png", meta)
	require.NoError(t, err)
	require.NoError(t, db.Model(&types.ResourceBinding{}).Where("owner_id = ?", "candidate").Count(&count).Error)
	require.EqualValues(t, 1, count)
	// Both location and already-logical reuse must claim the owner before
	// returning a reference, including the shared-file registration path.
	for i, path := range []string{"local://7/images/new.png", ref} {
		meta.OwnerID = []string{"candidate-2", "candidate-3"}[i]
		again, err := catalog.Register(ctx, 7, path, meta)
		require.NoError(t, err)
		require.Equal(t, ref, again)
	}
	require.NoError(t, db.Model(&types.ResourceBinding{}).Count(&count).Error)
	require.EqualValues(t, 3, count)
	_, err = catalog.Register(ctx, 8, ref, meta)
	require.Error(t, err)
}
