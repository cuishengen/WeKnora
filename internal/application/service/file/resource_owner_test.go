package file

import (
	"context"
	"errors"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

type ownerBindingCatalog struct {
	*contentCatalogStub
	bindErr       error
	boundOwner    string
	boundRelation string
}

func (s *ownerBindingCatalog) Bind(_ context.Context, _, ownerType, ownerID, relation string) error {
	if ownerType != types.ResourceOwnerKnowledge {
		return errors.New("unexpected resource owner")
	}
	s.boundOwner, s.boundRelation = ownerID, relation
	return s.bindErr
}

func TestParserImageRegistersOwnerBeforeReturningHandle(t *testing.T) {
	catalog := &ownerBindingCatalog{contentCatalogStub: newContentCatalogStub()}
	inner := &countingFileStub{}
	svc := NewResourceCatalogFileService(inner, catalog)
	ctx := types.WithKnowledgeResourceOwner(context.Background(), "candidate")
	_, err := svc.SaveBytes(ctx, pngPayload(), 7, "page.png", false)
	require.NoError(t, err)
	require.Len(t, catalog.registered, 1)
	meta := catalog.registered[0]
	require.Equal(t, types.ResourceOwnerKnowledge, meta.OwnerType)
	require.Equal(t, "candidate", meta.OwnerID)
	require.Equal(t, types.ResourceRelationExtractedImage, meta.Relation)
}

func TestParserImageReuseMustBindOwnerBeforeReturningHandle(t *testing.T) {
	catalog := &ownerBindingCatalog{contentCatalogStub: newContentCatalogStub()}
	inner := &countingFileStub{}
	svc := NewResourceCatalogFileService(inner, catalog)
	_, err := svc.SaveBytes(context.Background(), pngPayload(), 7, "original.png", false)
	require.NoError(t, err)
	catalog.bindErr = errors.New("binding database unavailable")
	ctx := types.WithKnowledgeResourceOwner(context.Background(), "candidate")
	ref, err := svc.SaveBytes(ctx, pngPayload(), 7, "extracted.png", false)
	require.ErrorContains(t, err, "binding database unavailable")
	require.Empty(t, ref)
	require.Equal(t, "candidate", catalog.boundOwner)
	require.Equal(t, 1, inner.saveBytesCalls)
}
