package im

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Tencent/WeKnora/internal/companyidentity"
	"github.com/google/uuid"
)

type companyChannelMember struct {
	Issuer  string `json:"issuer"`
	Subject string `json:"sub"`
}

type companyChannelGrant struct {
	ChannelID        string                 `json:"channel_id"`
	TenantID         uint64                 `json:"tenant_id"`
	BotIdentity      string                 `json:"bot_identity"`
	AgentID          string                 `json:"agent_id"`
	Members          []companyChannelMember `json:"members"`
	KnowledgeBaseIDs []string               `json:"knowledge_base_ids"`
}

// Only a marked channel may consume this separate deployment grant. Legacy
// channels never read the file and retain their existing global library scope.
// A marked channel fails closed on removal, invalid configuration or revocation.
func companyChannelScope(channel *IMChannel, member *companyidentity.Member) ([]string, string, error) {
	var credentials struct {
		Required bool `json:"company_channel_grant_required"`
	}
	if len(channel.Credentials) > 0 && json.Unmarshal(channel.Credentials, &credentials) != nil {
		return nil, "", companyidentity.ErrDenied
	}
	if !credentials.Required {
		return nil, "", nil
	}
	if channel.Platform != string(PlatformWeCom) || member == nil {
		return nil, "", companyidentity.ErrDenied
	}
	path := os.Getenv("COMPANY_IM_CHANNEL_GRANTS_FILE")
	if !filepath.IsAbs(path) {
		return nil, "", companyidentity.ErrDenied
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm()&0077 != 0 || before.Size() > 64*1024 {
		return nil, "", companyidentity.ErrDenied
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, "", companyidentity.ErrDenied
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm()&0077 != 0 {
		return nil, "", companyidentity.ErrDenied
	}
	var policy struct {
		Version  int                   `json:"version"`
		Channels []companyChannelGrant `json:"channels"`
	}
	decoder := json.NewDecoder(io.LimitReader(file, 64*1024+1))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&policy) != nil || policy.Version != 1 || len(policy.Channels) == 0 || len(policy.Channels) > 64 {
		return nil, "", companyidentity.ErrDenied
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return nil, "", companyidentity.ErrDenied
	}
	seen := map[string]bool{}
	var selected *companyChannelGrant
	for i := range policy.Channels {
		grant := &policy.Channels[i]
		if !companyGrantUUID(grant.ChannelID) || seen[grant.ChannelID] || grant.TenantID == 0 ||
			!companyGrantUUID(grant.AgentID) || !strings.HasPrefix(grant.BotIdentity, "wecom:ws:") ||
			len(grant.BotIdentity) <= len("wecom:ws:") || strings.ContainsAny(grant.BotIdentity, " \t\r\n") ||
			len(grant.Members) == 0 || len(grant.KnowledgeBaseIDs) == 0 {
			return nil, "", companyidentity.ErrDenied
		}
		seen[grant.ChannelID] = true
		ids := map[string]bool{}
		for _, id := range grant.KnowledgeBaseIDs {
			if !companyGrantUUID(id) || ids[id] {
				return nil, "", companyidentity.ErrDenied
			}
			ids[id] = true
		}
		for _, allowed := range grant.Members {
			if allowed.Issuer == "" || allowed.Subject == "" || strings.ContainsAny(allowed.Issuer+allowed.Subject, "\r\n\t") {
				return nil, "", companyidentity.ErrDenied
			}
		}
		if grant.ChannelID == channel.ID {
			selected = grant
		}
	}
	if selected == nil || selected.TenantID != channel.TenantID || selected.BotIdentity != channel.BotIdentity || selected.AgentID != channel.AgentID {
		return nil, "", companyidentity.ErrDenied
	}
	for _, allowed := range selected.Members {
		if allowed.Issuer == member.Issuer && allowed.Subject == member.Subject {
			// A changed grant gets a separate conversation identity and cannot
			// disclose answers already queued under its previous library scope.
			raw, _ := json.Marshal(selected)
			digest := sha256.Sum256(raw)
			return append([]string(nil), selected.KnowledgeBaseIDs...), hex.EncodeToString(digest[:]), nil
		}
	}
	return nil, "", companyidentity.ErrDenied
}

func companyGrantUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}
