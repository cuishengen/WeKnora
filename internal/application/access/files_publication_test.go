package access

import (
	"context"
	"errors"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

type publicationFileCatalog struct {
	fileCatalog
	published bool
	err       error
	checks    int
}

func (c *publicationFileCatalog) IsResourcePublishedForRead(_ context.Context, _ string, tenant uint64) (bool, error) {
	c.checks++
	if tenant != c.resource.TenantID {
		return false, errors.New("publication checked with wrong owner")
	}
	return c.published, c.err
}

func TestMessageArtifactsRequirePublicationAfterOwnership(t *testing.T) {
	const ref = "resource://AbCdEfGhIjKlMnOpQrStUv"
	for _, sourceOwned := range []bool{false, true} {
		for _, queryError := range []bool{false, true} {
			ctx := callerContext()
			owner := uint64(1)
			if sourceOwned {
				owner = 2
			}
			catalog := &publicationFileCatalog{fileCatalog: fileCatalog{&types.StoredResource{
				TenantID: owner, PhysicalPath: "local://1/exports/a.pdf",
			}}}
			if queryError {
				catalog.err = errors.New("database unavailable")
			}
			message := &types.Message{AgentID: "agent", AgentTenantID: owner,
				Artifacts: types.MessageArtifacts{{URL: ref, FileName: "a.pdf"}}}
			// Source-owned artifacts require a live KB share as well as publication.
			if sourceOwned {
				message.AgentTenantID = 1
				message.KnowledgeReferences = types.References{{KnowledgeBaseID: "shared", Content: ref}}
				catalog.resource.Handle = "AbCdEfGhIjKlMnOpQrStUv"
				catalog.resource.PhysicalPath = "local://2/exports/a.pdf"
			}
			auth := MessageKBShareAuthorizer{ShareGuard: &shareLookup{permission: types.OrgRoleViewer},
				KBs:      messageFileKBs{kb: &types.KnowledgeBase{ID: "shared", TenantID: owner}},
				Bindings: &fileBinding{allowed: true}}
			_, err := ResolveMessageArtifact(ctx, message, 0, nil, catalog, auth)
			require.ErrorIs(t, err, ErrNotFound)
			require.Equal(t, 1, catalog.checks)
			catalog.published, catalog.err = true, nil
			file, err := ResolveMessageArtifact(ctx, message, 0, nil, catalog, auth)
			require.NoError(t, err)
			require.Equal(t, owner, file.OwnerTenantID)
		}
	}
}

func TestPublishedFileStillRequiresExactKBGrant(t *testing.T) {
	ctx := callerContext()
	catalog := &publicationFileCatalog{fileCatalog: fileCatalog{&types.StoredResource{
		TenantID: 2, PhysicalPath: "local://2/exports/a.pdf",
	}}, published: true}
	grant := &KBAccess{Caller: types.CallerFromContext(ctx), EffectiveTenantID: 2,
		KnowledgeBase: &types.KnowledgeBase{ID: "kb", TenantID: 2}, Permission: types.OrgRoleViewer}
	_, err := ResolveKBFile(ctx, grant, "kb", "resource://AbCdEfGhIjKlMnOpQrStUv", catalog, &fileBinding{})
	require.ErrorIs(t, err, ErrForbidden)
	require.Zero(t, catalog.checks, "publication must not replace a missing binding")
}
