package router

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/middleware"
	"github.com/Tencent/WeKnora/internal/types"
	secutils "github.com/Tencent/WeKnora/internal/utils"
	"github.com/gin-gonic/gin"
)

func TestFileProxiesRejectUnpublishedResourcesBeforeStorage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("STORAGE_TYPE", "local")
	t.Setenv("SYSTEM_SIGNING_KEY", "")
	t.Setenv("SYSTEM_AES_KEY", "weknora-test-aes-key-32bytes!!!")
	t.Setenv("LOCAL_STORAGE_BASE_DIR", t.TempDir())
	const ref = "resource://AbCdEfGhIjKlMnOpQrStUv"
	const physical = "local://7/exports/candidate.pdf"
	for _, lookupError := range []bool{false, true} {
		for _, route := range []string{"tenant-ref", "tenant-raw", "grant", "kb", "message", "presigned", "preview"} {
			t.Run(route+map[bool]string{false: "-staged", true: "-lookup-error"}[lookupError], func(t *testing.T) {
				checks := 0
				bindingChecks := 0
				catalog := &stubResourceCatalog{
					resource: &types.StoredResource{Handle: "AbCdEfGhIjKlMnOpQrStUv", TenantID: 7, PhysicalPath: physical},
					bound: func(_ context.Context, tenant uint64, kb, reference string) (bool, error) {
						bindingChecks++
						return tenant == 7 && kb == "kb" && reference == ref, nil
					},
					published: func(_ context.Context, reference string, tenant uint64) (bool, error) {
						checks++
						if tenant != 7 || (reference != ref && reference != physical) {
							t.Fatalf("unexpected publication scope tenant=%d ref=%q", tenant, reference)
						}
						if lookupError {
							return false, errors.New("database unavailable")
						}
						return false, nil
					},
				}
				// No storage or tenant resolution may follow a rejected publication.
				storageReads := 0
				files := &stubFileService{getFile: func(context.Context, string) (io.ReadCloser, error) {
					storageReads++
					return nil, errors.New("unpublished resource reached storage")
				}}
				tenants := &stubTenantService{get: func(context.Context, uint64) (*types.Tenant, error) {
					t.Fatal("unpublished resource reached storage tenant resolution")
					return nil, nil
				}}
				engine := gin.New()
				path := "/files?file_path=" + url.QueryEscape(ref)
				switch route {
				case "tenant-ref", "tenant-raw":
					engine.GET("/files", newFileServeHandler(files, nil, catalog))
					if route == "tenant-raw" {
						path = "/files?file_path=" + url.QueryEscape(physical)
					}
				case "grant":
					serveResourceGrants(engine, catalog, tenants, files, nil)
					path = "/r/token"
				case "kb":
					engine.GET("/knowledge-bases/:id/files", func(c *gin.Context) {
						ctx := types.WithCaller(c.Request.Context(), types.Caller{TenantID: 7, UserID: "viewer", Role: types.TenantRoleViewer})
						grant := &middleware.KBAccess{
							KnowledgeBase: &types.KnowledgeBase{ID: "kb", TenantID: 7}, EffectiveTenantID: 7,
							Caller:     types.CallerFromContext(ctx),
							Permission: types.OrgRoleViewer,
						}
						c.Set(middleware.KBAccessContextKey, grant)
						c.Request = c.Request.WithContext(grant.Context(ctx))
						c.Next()
					}, newKBScopedFileServeHandlerWithResources(tenants, files, nil, catalog))
					path = "/knowledge-bases/kb/files?file_path=" + url.QueryEscape(ref)
				case "message":
					engine = newMessageScopedFilesTestEngine(7, &stubMessageFileLookup{get: func(context.Context, string, string) (*types.Message, error) {
						return &types.Message{AgentTenantID: 7, Content: ref}, nil
					}}, nil, tenants, files, catalog, messageKBShareAuthorizer{})
					path = "/sessions/session/messages/message/files?file_path=" + url.QueryEscape(ref)
				case "presigned":
					engine.GET("/api/v1/files/presigned", presignedFileHandlerWithResources(tenants, t.TempDir(), nil, catalog))
					signed, err := secutils.SignFileURL("https://example.com", physical, 7, time.Minute)
					if err != nil {
						t.Fatal(err)
					}
					u, err := url.Parse(signed)
					if err != nil {
						t.Fatal(err)
					}
					path = "/api/v1/files/presigned?" + u.RawQuery
				case "preview":
					// The shared resolver is the preview signer's publication gate.
					engine.GET("/files", func(c *gin.Context) { resolveCatalogResource(c, catalog, ref, 7) })
				}
				request := httptest.NewRequest(http.MethodGet, path, nil)
				request = request.WithContext(context.WithValue(request.Context(), types.TenantInfoContextKey, &types.Tenant{ID: 7}))
				recorder := httptest.NewRecorder()
				engine.ServeHTTP(recorder, request)
				if recorder.Code != http.StatusNotFound || checks != 1 {
					t.Fatalf("status=%d publication checks=%d body=%q", recorder.Code, checks, recorder.Body.String())
				}
				if route == "kb" && bindingChecks != 1 {
					t.Fatalf("existing KB file ACL checks=%d, want 1 before publication check", bindingChecks)
				}
				if storageReads != 0 {
					t.Fatalf("storage reads=%d, want 0 for an unpublished resource", storageReads)
				}
			})
		}
	}
}
