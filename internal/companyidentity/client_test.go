package companyidentity

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestStatusRequestsAreAuthenticatedAndRevocationIsFresh(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	var disabled atomic.Bool
	var requests atomic.Int32
	nonces := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		raw, _ := io.ReadAll(r.Body)
		digest := sha256.Sum256(raw)
		nonce := r.Header.Get("X-Identity-Nonce")
		canonical := strings.Join([]string{"v1", "POST", r.URL.Path, hex.EncodeToString(digest[:]),
			r.Header.Get("X-Identity-Time"), nonce}, "\n")
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(canonical))
		if r.Method != "POST" || r.Header.Get("X-Identity-Signature") != hex.EncodeToString(mac.Sum(nil)) || nonces[nonce] {
			t.Error("missing, replayed or invalid body/path-bound request MAC")
			w.WriteHeader(401)
			return
		}
		nonces[nonce] = true
		var body map[string]string
		_ = json.Unmarshal(raw, &body)
		if body["issuer"] != defaultIssuer || (r.URL.Path == memberStatusPath && body["sub"] != "stable-subject") ||
			(r.URL.Path == wecomStatusPath && body["wecomID"] != "ExactUserID") {
			t.Error("status lookup did not preserve the stable/opaque identifier")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"active": !disabled.Load(), "allowed": !disabled.Load(),
			"issuer": defaultIssuer, "sub": "stable-subject", "wecomID": "ExactUserID", "createdTime": "2026-11-18"})
	}))
	defer server.Close()
	checker := &Checker{origin: server.URL, issuer: defaultIssuer, key: key, knowledgeBases: []string{"vendor"}, client: server.Client()}
	binding := Binding{Issuer: defaultIssuer, Subject: "stable-subject"}
	if _, err := checker.Check(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	if _, err := checker.LookupWeCom(context.Background(), "ExactUserID"); err != nil {
		t.Fatal(err)
	}
	if _, err := checker.CheckWeCom(context.Background(), binding, "exactuserid"); !errors.Is(err, ErrDenied) {
		t.Fatalf("case-mismatched platform identity accepted: %v", err)
	}
	disabled.Store(true)
	if _, err := checker.Check(context.Background(), binding); !errors.Is(err, ErrDenied) {
		t.Fatalf("revoked directory member accepted: %v", err)
	}
	if requests.Load() != 4 {
		t.Fatalf("employee status must be read freshly per call; requests=%d", requests.Load())
	}
	for _, ids := range [][]string{nil, {"warehouse"}, {"vendor", "warehouse"}} {
		if checker.AuthorizeKnowledgeBases(ids) == nil {
			t.Fatalf("scope %v accepted", ids)
		}
	}
	if err := checker.AuthorizeKnowledgeBases([]string{"vendor"}); err != nil {
		t.Fatal(err)
	}
}

func TestMalformedStatusAndMissingRevalidationFailClosed(t *testing.T) {
	for _, body := range []string{`{}`, `{"active":true,"allowed":true}`, `not-json`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
		checker := &Checker{origin: server.URL, issuer: defaultIssuer, key: make([]byte, 32), client: server.Client()}
		if _, err := checker.Check(context.Background(), Binding{Issuer: defaultIssuer, Subject: "subject"}); err == nil {
			t.Errorf("malformed status accepted: %s", body)
		}
		server.Close()
	}
	if !errors.Is(Revalidate(context.Background()), ErrDenied) {
		t.Fatal("untrusted context must not bypass per-tool revalidation")
	}
	ctx := WithRevalidation(context.Background(), func(context.Context) error { return ErrDenied })
	if !errors.Is(Revalidate(ctx), ErrDenied) {
		t.Fatal("tool ignored revoked identity")
	}
}

func TestFromEnvRequiresPrivateKeyAndExplicitScope(t *testing.T) {
	key := filepath.Join(t.TempDir(), "status-key")
	if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMPANY_IDENTITY_STATUS_ORIGIN", "http://status.internal:8000")
	t.Setenv("COMPANY_IDENTITY_STATUS_KEY_FILE", key)
	t.Setenv("COMPANY_IDENTITY_KNOWLEDGE_BASE_IDS", "vendor")
	t.Setenv("COMPANY_IDENTITY_ALLOW_ORIGINAL_DOWNLOADS", "false")
	checker, err := FromEnv()
	if err != nil || checker.OriginalDownloadsAllowed() {
		t.Fatalf("safe config rejected or downloads enabled: %v", err)
	}
	t.Setenv("COMPANY_IDENTITY_KNOWLEDGE_BASE_IDS", "")
	if _, err := FromEnv(); !errors.Is(err, ErrUnavailable) {
		t.Fatal("empty deployment scope must fail closed")
	}
}
