package service

import (
	"context"
	"fmt"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

func registeredOwner(resource *types.StoredResource, meta interfaces.ResourceRegistration) *types.ResourceBinding {
	relation := meta.Relation
	if relation == "" {
		relation = types.ResourceRelationAttachment
	}
	return &types.ResourceBinding{ResourceID: resource.ID, TenantID: resource.TenantID,
		OwnerType: meta.OwnerType, OwnerID: meta.OwnerID, Relation: relation}
}

func (s *resourceCatalog) bindRegisteredOwner(ctx context.Context, resource *types.StoredResource, meta interfaces.ResourceRegistration) error {
	if meta.OwnerID == "" {
		return nil
	}
	return s.repo.CreateBinding(ctx, registeredOwner(resource, meta))
}

func (s *resourceCatalog) createRegisteredResource(ctx context.Context, resource *types.StoredResource, meta interfaces.ResourceRegistration) error {
	if meta.OwnerID == "" {
		return s.repo.Create(ctx, resource)
	}
	writer, ok := s.repo.(interface {
		CreateWithBinding(context.Context, *types.StoredResource, *types.ResourceBinding) error
	})
	if !ok {
		return fmt.Errorf("atomic resource owner registration unavailable")
	}
	return writer.CreateWithBinding(ctx, resource, registeredOwner(resource, meta))
}

// IsResourcePublishedForRead is an optional ResourceCatalog capability used by
// every file proxy after its normal authentication and permission checks.
func (s *resourceCatalog) IsResourcePublishedForRead(
	ctx context.Context, reference string, tenantID uint64,
) (bool, error) {
	physical, resource, err := s.ResolvePath(ctx, reference)
	if err != nil {
		return false, err
	}
	if resource == nil {
		resource, err = s.repo.GetByTenantLocation(ctx, tenantID, resourceLocationHash(physical))
		if err != nil {
			return false, err
		}
	}
	resourceID := ""
	if resource != nil {
		if resource.TenantID != tenantID {
			return false, nil
		}
		resourceID = resource.ID
	}
	lookup, ok := s.repo.(interface {
		IsResourcePublishedForRead(context.Context, uint64, string, []string) (bool, error)
	})
	if !ok {
		return false, fmt.Errorf("resource publication lookup unavailable")
	}
	references := []string{reference}
	if physical != reference {
		references = append(references, physical)
	}
	if resource != nil {
		references = append(references, types.BuildResourcePath(resource.Handle))
	}
	return lookup.IsResourcePublishedForRead(ctx, tenantID, resourceID, references)
}
