package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// FindStagedKnowledge is an internal datasource lookup, never a public listing.
func (r *knowledgeRepository) FindStagedKnowledge(ctx context.Context, tenantID uint64,
	kbID, dataSourceID, externalID string,
) (*types.Knowledge, error) {
	var row types.Knowledge
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND knowledge_base_id = ? AND is_staged = ?", tenantID, kbID, true).
		Where("metadata->>'datasource_id' = ? AND metadata->>'external_id' = ?", dataSourceID, externalID).
		Order("created_at DESC").First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &row, err
}

// PublishStagedKnowledge switches an already indexed candidate and its expected
// predecessor in one DB transaction. Remote resource cleanup must happen later.
func (r *knowledgeRepository) PublishStagedKnowledge(ctx context.Context, before, candidate *types.Knowledge) error {
	if candidate == nil || candidate.ID == "" {
		return fmt.Errorf("missing staged knowledge")
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current types.Knowledge
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND tenant_id = ? AND knowledge_base_id = ?", candidate.ID,
				candidate.TenantID, candidate.KnowledgeBaseID).First(&current).Error; err != nil {
			return err
		}
		meta, expected := current.GetMetadata(), candidate.GetMetadata()
		if meta["datasource_id"] == "" || meta["external_id"] == "" ||
			meta["datasource_id"] != expected["datasource_id"] || meta["external_id"] != expected["external_id"] ||
			current.FileHash != candidate.FileHash || meta["sha256"] != expected["sha256"] {
			return fmt.Errorf("staged source identity changed")
		}
		if !current.IsStaged {
			return nil // Retry after a committed publication acknowledgement was lost.
		}
		if current.ParseStatus != types.ParseStatusCompleted || current.EnableStatus != "enabled" || current.ProcessedAt == nil || current.StorageSize <= 0 {
			return fmt.Errorf("staged knowledge is not ready")
		}
		if before != nil {
			if current.ReplacesKnowledgeID == nil || *current.ReplacesKnowledgeID != before.ID ||
				before.TenantID != current.TenantID || before.KnowledgeBaseID != current.KnowledgeBaseID ||
				before.GetMetadata()["datasource_id"] != meta["datasource_id"] ||
				before.GetMetadata()["external_id"] != meta["external_id"] {
				return fmt.Errorf("invalid staged predecessor")
			}
			retired := tx.Where("id = ? AND tenant_id = ? AND knowledge_base_id = ? AND updated_at = ? AND is_staged = ?",
				before.ID, before.TenantID, before.KnowledgeBaseID, before.UpdatedAt, false).Delete(&types.Knowledge{})
			if retired.Error != nil {
				return retired.Error
			}
			if retired.RowsAffected != 1 {
				return fmt.Errorf("knowledge changed during staged publication")
			}
		} else if current.ReplacesKnowledgeID != nil {
			return fmt.Errorf("staged predecessor missing")
		}
		published := tx.Model(&types.Knowledge{}).
			Where("id = ? AND is_staged = ? AND parse_status = ?", current.ID, true, types.ParseStatusCompleted).
			UpdateColumn("is_staged", false)
		if published.Error != nil {
			return published.Error
		}
		if published.RowsAffected != 1 {
			return fmt.Errorf("staged publication conflict")
		}
		return nil
	})
}
