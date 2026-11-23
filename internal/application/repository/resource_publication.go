package repository

import (
	"context"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// CreateWithBinding makes a knowledge-owned resource visible to catalog reads
// only when its publication owner is visible in the same database commit.
func (r *resourceRepository) CreateWithBinding(ctx context.Context, resource *types.StoredResource, binding *types.ResourceBinding) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(resource).Error; err != nil {
			return err
		}
		binding.ResourceID = resource.ID
		return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(binding).Error
	})
}

// IsResourcePublishedForRead is a publication barrier, not an authorization
// decision. A shared blob stays available if any live published document owns
// it; the caller must still enforce the requested resource's existing ACL.
// Dangling knowledge bindings fail closed, including the interval between
// saving a source file and inserting its candidate knowledge row.
func (r *resourceRepository) IsResourcePublishedForRead(
	ctx context.Context, tenantID uint64, resourceID string, references []string,
) (bool, error) {
	if tenantID == 0 {
		return false, nil
	}
	var bound, sources int64
	bindings := r.db.WithContext(ctx).Table("resource_bindings AS b").
		Where("b.tenant_id = ? AND b.resource_id = ? AND b.owner_type = ?",
			tenantID, resourceID, types.ResourceOwnerKnowledge)
	if resourceID != "" {
		if err := bindings.Count(&bound).Error; err != nil {
			return false, err
		}
	}
	source := r.db.WithContext(ctx).Table("knowledges AS k").
		Where("k.tenant_id = ? AND k.file_path IN ?", tenantID, references)
	if err := source.Count(&sources).Error; err != nil {
		return false, err
	}
	if bound == 0 && sources == 0 {
		// Registered ordinary message artifacts have no knowledge owner. Their
		// normal authorization remains authoritative. Unknown raw locators are
		// refused: a physical write may still be awaiting atomic registration.
		return resourceID != "", nil
	}
	var published int64
	if bound > 0 {
		if err := bindings.
			Joins("JOIN knowledges AS k ON k.id = b.owner_id AND k.tenant_id = b.tenant_id").
			Joins("JOIN knowledge_bases AS kb ON kb.id = k.knowledge_base_id AND kb.tenant_id = k.tenant_id").
			Where("k.is_staged = ? AND k.deleted_at IS NULL AND kb.deleted_at IS NULL", false).
			Count(&published).Error; err != nil {
			return false, err
		}
		if published > 0 {
			return true, nil
		}
	}
	if sources > 0 {
		if err := source.
			Joins("JOIN knowledge_bases AS kb ON kb.id = k.knowledge_base_id AND kb.tenant_id = k.tenant_id").
			Where("k.is_staged = ? AND k.deleted_at IS NULL AND kb.deleted_at IS NULL", false).
			Count(&published).Error; err != nil {
			return false, err
		}
	}
	if published > 0 {
		return true, nil
	}
	// Saving a generated artifact to a KB adds a knowledge claim; deleting
	// that document must not revoke the message's own artifact. Preserve that
	// established behavior only when every knowledge claim was published.
	// Staged and dangling claims cannot use a message binding as a bypass.
	if resourceID != "" && bound > 0 {
		var historical int64
		if err := r.db.WithContext(ctx).Table("resource_bindings AS b").
			Joins("JOIN knowledges AS k ON k.id = b.owner_id AND k.tenant_id = b.tenant_id").
			Where("b.tenant_id = ? AND b.resource_id = ? AND b.owner_type = ? AND k.is_staged = ?",
				tenantID, resourceID, types.ResourceOwnerKnowledge, false).Count(&historical).Error; err != nil {
			return false, err
		}
		if historical != bound {
			return false, nil
		}
		if sources > 0 {
			if err := r.db.WithContext(ctx).Table("knowledges AS k").
				Where("k.tenant_id = ? AND k.file_path IN ? AND k.is_staged = ?", tenantID, references, false).
				Count(&historical).Error; err != nil {
				return false, err
			}
			if historical != sources {
				return false, nil
			}
		}
		var messages int64
		if err := r.db.WithContext(ctx).Table("resource_bindings").
			Where("tenant_id = ? AND resource_id = ? AND owner_type = ? AND relation = ?",
				tenantID, resourceID, types.ResourceOwnerMessage, types.ResourceRelationArtifact).
			Count(&messages).Error; err != nil {
			return false, err
		}
		return messages > 0, nil
	}
	return false, nil
}
