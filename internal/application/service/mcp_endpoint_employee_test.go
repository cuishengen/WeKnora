package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/Tencent/WeKnora/internal/companyidentity"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

func employeeEndpointFixture(t *testing.T) (*mcpEndpointService, context.Context, *atomic.Bool) {
	t.Helper()
	active := &atomic.Bool{}
	active.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"active": active.Load(), "allowed": active.Load(),
			"issuer": "https://auth.ch-chip.com", "sub": "stable-employee", "wecomID": "ExactID", "createdTime": "2026-11-18"})
	}))
	t.Cleanup(server.Close)
	key := filepath.Join(t.TempDir(), "status-key")
	if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMPANY_IDENTITY_STATUS_ORIGIN", server.URL)
	t.Setenv("COMPANY_IDENTITY_STATUS_KEY_FILE", key)
	t.Setenv("COMPANY_IDENTITY_KNOWLEDGE_BASE_IDS", "vendor")
	t.Setenv("COMPANY_IDENTITY_REQUIRE_EMPLOYEE_MCP", "true")
	t.Setenv("COMPANY_IDENTITY_ALLOW_ORIGINAL_DOWNLOADS", "true")
	svc := &mcpEndpointService{repo: newStubMCPEndpointRepo(),
		kbService: &stubKBForMCPEndpoint{kbs: map[string]*types.KnowledgeBase{
			"vendor": {ID: "vendor", TenantID: 1}, "warehouse": {ID: "warehouse", TenantID: 1}}}}
	ctx := types.WithCaller(context.Background(), types.Caller{TenantID: 1, UserID: "administrator", Role: types.TenantRoleAdmin})
	return svc, ctx, active
}

func employeeEndpoint() *types.MCPEndpoint {
	return &types.MCPEndpoint{Name: "Employee reader", Enabled: true,
		EmployeeBinding:  &companyidentity.Binding{Issuer: "https://auth.ch-chip.com", Subject: "stable-employee"},
		KnowledgeBaseIDs: types.StringArray{"vendor"},
		Tools:            types.StringArray{types.MCPEndpointToolReadDocument, types.MCPEndpointToolGetOriginalDocument}}
}

func TestEmployeeMCPEndpointRevocationInvalidatesExistingToken(t *testing.T) {
	svc, ctx, active := employeeEndpointFixture(t)
	ep, token, err := svc.Create(ctx, 1, employeeEndpoint())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, ep.ID, token); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Update(ctx, 1, ep.ID, interfaces.MCPEndpointUpdate{EmployeeBinding: &companyidentity.Binding{
		Issuer: "https://auth.ch-chip.com", Subject: "another-employee"}}); !errors.Is(err, companyidentity.ErrDenied) {
		t.Fatalf("an issued employee credential must not be rebound: %v", err)
	}
	active.Store(false)
	if _, err := svc.Authenticate(ctx, ep.ID, token); !errors.Is(err, companyidentity.ErrDenied) {
		t.Fatalf("existing token survived employee disable: %v", err)
	}
	if _, _, err := svc.RotateToken(ctx, 1, ep.ID); !errors.Is(err, companyidentity.ErrDenied) {
		t.Fatalf("disabled employee obtained a new token: %v", err)
	}
	disabled := false
	if _, err := svc.Update(ctx, 1, ep.ID, interfaces.MCPEndpointUpdate{Enabled: &disabled}); err != nil {
		t.Fatalf("administrator must still be able to disable a revoked employee endpoint: %v", err)
	}
}

func TestEmployeeMCPEndpointCannotWidenOrWrite(t *testing.T) {
	svc, ctx, _ := employeeEndpointFixture(t)
	for _, test := range []struct {
		name   string
		change func(*types.MCPEndpoint)
	}{
		{"unbound", func(ep *types.MCPEndpoint) { ep.EmployeeBinding = nil }},
		{"empty libraries", func(ep *types.MCPEndpoint) { ep.KnowledgeBaseIDs = nil }},
		{"sensitive library", func(ep *types.MCPEndpoint) { ep.KnowledgeBaseIDs = types.StringArray{"warehouse"} }},
		{"write tool", func(ep *types.MCPEndpoint) { ep.Tools = types.StringArray{types.MCPEndpointToolAddDocument} }},
		{"agent execution", func(ep *types.MCPEndpoint) { ep.Tools = types.StringArray{types.MCPEndpointToolAsk} }},
		{"wrong subject", func(ep *types.MCPEndpoint) { ep.EmployeeBinding.Subject = "another-subject" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			ep := employeeEndpoint()
			test.change(ep)
			if _, _, err := svc.Create(ctx, 1, ep); !errors.Is(err, companyidentity.ErrDenied) {
				t.Fatalf("employee credential exceeded its grant: %v", err)
			}
		})
	}
}

type employeeLocalUser struct {
	interfaces.UserService
	user *types.User
}

func (s *employeeLocalUser) GetUserByID(context.Context, string) (*types.User, error) {
	return s.user, nil
}

type employeeLocalMember struct {
	interfaces.TenantMemberService
	member *types.TenantMember
}

func (s *employeeLocalMember) GetMembership(context.Context, string, uint64) (*types.TenantMember, error) {
	return s.member, nil
}

func TestEmployeeMCPEndpointHonorsExplicitLocalAccountRevocation(t *testing.T) {
	svc, ctx, _ := employeeEndpointFixture(t)
	users := &employeeLocalUser{user: &types.User{ID: "local-user", IsActive: true}}
	members := &employeeLocalMember{member: &types.TenantMember{UserID: "local-user", TenantID: 1,
		Role: types.TenantRoleViewer, Status: types.TenantMemberStatusActive}}
	svc.userService, svc.memberService = users, members
	row := employeeEndpoint()
	row.EmployeeBinding.WeKnoraUserID = "local-user"
	ep, token, err := svc.Create(ctx, 1, row)
	if err != nil {
		t.Fatal(err)
	}
	members.member.Status = types.TenantMemberStatusSuspended
	if _, err := svc.Authenticate(ctx, ep.ID, token); !errors.Is(err, companyidentity.ErrDenied) {
		t.Fatalf("suspended local membership survived employee token check: %v", err)
	}
	members.member.Status = types.TenantMemberStatusActive
	users.user.IsActive = false
	if _, err := svc.Authenticate(ctx, ep.ID, token); !errors.Is(err, companyidentity.ErrDenied) {
		t.Fatalf("disabled local account survived employee token check: %v", err)
	}
}
