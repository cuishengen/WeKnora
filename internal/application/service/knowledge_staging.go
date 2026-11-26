package service

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

type stagedKnowledgeRepository interface {
	FindStagedKnowledge(context.Context, uint64, string, string, string) (*types.Knowledge, error)
	PublishStagedKnowledge(context.Context, *types.Knowledge, *types.Knowledge) error
}

// StageDataSourceKnowledge is intentionally optional on KnowledgeService: only
// the NAS connector opts into waiting for a published, successfully indexed row.
func (s *knowledgeService) StageDataSourceKnowledge(ctx context.Context, ds *types.DataSource,
	item *types.FetchedItem, metadata map[string]string, tagIDs []string, channel string,
	verify func(context.Context) error,
) (updated bool, err error) {
	staging, ok := s.repo.(stagedKnowledgeRepository)
	if !ok {
		return false, fmt.Errorf("knowledge repository does not support staged publication")
	}
	kb, err := s.kbService.GetKnowledgeBaseByID(ctx, ds.KnowledgeBaseID)
	if err != nil {
		return false, err
	}
	if err := validateNASStagingKB(kb, ds.TenantID); err != nil {
		return false, err
	}
	if len(item.Content) == 0 || metadata["sha256"] == "" || item.ExternalID == "" {
		return false, fmt.Errorf("NAS staging requires original bytes, SHA-256 and source identity")
	}
	before, err := s.repo.FindByDataSourceExternalID(ctx, ds.TenantID, kb.ID, ds.ID, item.ExternalID)
	if err != nil {
		return false, err
	}
	if before != nil && before.GetMetadata()["sha256"] == metadata["sha256"] && before.ParseStatus == types.ParseStatusCompleted {
		return false, types.NewDuplicateFileError(before)
	}
	var retirement *knowledgeDeletePlan
	if before != nil {
		retirement, err = s.planKnowledgeDelete(ctx, []string{before.ID})
		if err != nil {
			return false, err
		}
		before = retirement.knowledge[0]
	}
	candidate, err := staging.FindStagedKnowledge(ctx, ds.TenantID, kb.ID, ds.ID, item.ExternalID)
	if err != nil {
		return false, err
	}
	predecessorChanged := candidate != nil && ((before == nil) != (candidate.ReplacesKnowledgeID == nil) ||
		(before != nil && candidate.ReplacesKnowledgeID != nil && *candidate.ReplacesKnowledgeID != before.ID))
	if candidate != nil && (predecessorChanged || candidate.GetMetadata()["sha256"] != metadata["sha256"] ||
		candidate.ParseStatus == types.ParseStatusFailed || candidate.ParseStatus == types.ParseStatusCancelled) {
		// The old published row remains untouched while a failed/obsolete
		// candidate is cancelled and reclaimed through the normal delete path.
		if err := s.DeleteKnowledge(types.WithStagedKnowledgeRead(ctx, candidate.ID), candidate.ID); err != nil {
			return false, fmt.Errorf("remove failed staged candidate: %w", err)
		}
		candidate = nil
	}
	if candidate == nil {
		file, buildErr := bytesToFileHeader(item.Content, item.FileName)
		if buildErr != nil {
			return false, buildErr
		}
		// Imported originals already contain their own context. Avoid one LLM
		// summary call per file during a full scan; text/image indexing is unchanged.
		summary := false
		candidate, err = s.createKnowledgeFromFile(ctx, kb.ID, file, metadata, nil, item.FileName, tagIDs, channel,
			&types.KnowledgeProcessOverrides{SummaryEnabled: &summary},
			&knowledgeReplacement{previous: before, staged: true, metadata: metadata, reference: item.SourceReference})
		if err != nil {
			return false, err
		}
	}
	if (before == nil) != (candidate.ReplacesKnowledgeID == nil) ||
		(before != nil && *candidate.ReplacesKnowledgeID != before.ID) {
		return false, fmt.Errorf("staged predecessor changed; retry the source scan")
	}
	// No DB transaction, model slot or cleanup-queue dependency is held here.
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		candidate, err = s.repo.GetKnowledgeByID(ctx, ds.TenantID, candidate.ID)
		if err != nil {
			return false, err
		}
		if !candidate.IsStaged {
			return before != nil, nil // Lost publication acknowledgement, now recovered.
		}
		switch candidate.ParseStatus {
		case types.ParseStatusFailed, types.ParseStatusCancelled, types.ParseStatusDeleting:
			if candidate.ParseStatus == types.ParseStatusFailed && strings.HasPrefix(candidate.ErrorMessage, stagedVisionDeferredPrefix) {
				return false, &datasource.DocumentParseDeferred{
					Reason: strings.TrimPrefix(candidate.ErrorMessage, stagedVisionDeferredPrefix)}
			}
			if candidate.ParseStatus == types.ParseStatusFailed && strings.HasPrefix(candidate.ErrorMessage, stagedDocumentRejectionPrefix) {
				return false, &datasource.DocumentParseFailure{KnowledgeID: candidate.ID,
					Reason: strings.TrimPrefix(candidate.ErrorMessage, stagedDocumentRejectionPrefix)}
			}
			return false, fmt.Errorf("staged source parsing did not succeed (%s); published version preserved", candidate.ParseStatus)
		case types.ParseStatusCompleted:
			if candidate.EnableStatus != "enabled" || candidate.ProcessedAt == nil {
				return false, fmt.Errorf("staged source has no usable indexed content; published version preserved")
			}
			if candidate.StorageSize <= 0 {
				if err := s.repo.UpdateKnowledgeColumns(ctx, candidate.ID, map[string]interface{}{
					"parse_status": types.ParseStatusFailed, "error_message": stagedDocumentRejectionPrefix + "no usable indexed content",
				}); err != nil {
					return false, err
				}
				return false, &datasource.DocumentParseFailure{KnowledgeID: candidate.ID, Reason: "no usable indexed content"}
			}
			if err := ctx.Err(); err != nil {
				return false, err
			}
			if verify == nil {
				return false, fmt.Errorf("staged source verification is required")
			}
			if err := verify(ctx); err != nil {
				return false, fmt.Errorf("source changed before publication: %w", err)
			}
			freshKB, err := s.kbService.GetKnowledgeBaseByID(ctx, kb.ID)
			if err != nil {
				return false, err
			}
			if err := validateNASStagingKB(freshKB, ds.TenantID); err != nil {
				return false, err
			}
			if freshKB.EmbeddingModelID != candidate.EmbeddingModelID {
				return false, fmt.Errorf("knowledge base embedding model changed during staging")
			}
			if err := staging.PublishStagedKnowledge(ctx, before, candidate); err != nil {
				return false, err
			}
			if retirement != nil {
				retirement.rowsDeleted = true
				cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
				retirement.ctx = cleanupCtx
				cleanupErr := s.executeKnowledgeDelete(retirement, true)
				if cleanupErr == nil {
					cleanupErr = s.repo.HardDeleteKnowledge(cleanupCtx, ds.TenantID, before.ID)
				}
				cancel()
				if cleanupErr != nil {
					logger.Warnf(ctx, "Published source %s; retired resources need cleanup: %v", candidate.ID, cleanupErr)
				}
			}
			return before != nil, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-ticker.C:
		}
	}
}

// Set only at the parser's confirmed content-rejection boundary. Other failed
// states (quota, storage, enqueue, embedding, configuration) remain fatal.
const stagedDocumentRejectionPrefix = "Document content rejected: "

func validateNASStagingKB(kb *types.KnowledgeBase, tenantID uint64) error {
	if kb == nil || kb.TenantID != tenantID || kb.Type != types.KnowledgeBaseTypeDocument ||
		!kb.IndexingStrategy.VectorEnabled || !kb.IndexingStrategy.KeywordEnabled ||
		kb.IndexingStrategy.GraphEnabled || kb.IndexingStrategy.WikiEnabled {
		return fmt.Errorf("NAS staging requires a document KB with vector/keyword indexing and no graph/wiki generation")
	}
	if driver := strings.TrimSpace(os.Getenv("RETRIEVE_DRIVER")); kb.VectorStoreID != nil || (driver != "" && driver != "postgres") {
		return fmt.Errorf("NAS staging currently requires the deployment PostgreSQL retriever")
	}
	return nil
}
