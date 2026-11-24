// Package companyidentity checks the existing company employee directory over
// its private, body-authenticated status projection. It never creates employees
// or infers an identity from a name, email address or client-supplied header.
package companyidentity

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	memberStatusPath = "/internal/member-status"
	wecomStatusPath  = "/internal/wecom-member-status"
	defaultIssuer    = "https://auth.ch-chip.com"
)

var (
	ErrUnavailable  = errors.New("company employee verification is unavailable")
	ErrDenied       = errors.New("company employee access is denied")
	statusTransport = func() *http.Transport {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		return transport
	}()
)

// Binding pins a credential to the directory's stable issuer and subject. An
// optional local account id is an explicit administrator binding, never an
// email lookup or an instruction to create/grant a WeKnora account.
type Binding struct {
	Issuer        string `json:"issuer"`
	Subject       string `json:"sub"`
	WeKnoraUserID string `json:"weknora_user_id,omitempty"`
}

func (b Binding) Value() (driver.Value, error) { return json.Marshal(b) }

func (b *Binding) Scan(value any) error {
	var raw []byte
	switch v := value.(type) {
	case nil:
		*b = Binding{}
		return nil
	case []byte:
		raw = v
	case string:
		raw = []byte(v)
	default:
		return ErrDenied
	}
	return json.Unmarshal(raw, b)
}

type Member struct {
	Issuer      string `json:"issuer"`
	Subject     string `json:"sub"`
	WeComID     string `json:"wecomID"`
	CreatedTime string `json:"createdTime"`
	Display     string `json:"display"`
}

type Checker struct {
	origin         string
	issuer         string
	key            []byte
	knowledgeBases []string
	downloads      bool
	client         *http.Client
}

type revalidationKey struct{}

// WithRevalidation installs an in-process check; an HTTP header cannot supply
// it. A batched/long-lived MCP request must re-check access for every tool.
func WithRevalidation(ctx context.Context, check func(context.Context) error) context.Context {
	return context.WithValue(ctx, revalidationKey{}, check)
}

func Revalidate(ctx context.Context) error {
	check, ok := ctx.Value(revalidationKey{}).(func(context.Context) error)
	if !ok || check == nil {
		return ErrDenied
	}
	return check(ctx)
}

// FromEnv reads only the existing private status key and a deployment-owned
// knowledge-base allowlist. No user-supplied origin or key is accepted.
func FromEnv() (*Checker, error) {
	origin := strings.TrimRight(strings.TrimSpace(os.Getenv("COMPANY_IDENTITY_STATUS_ORIGIN")), "/")
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, ErrUnavailable
	}
	keyPath := strings.TrimSpace(os.Getenv("COMPANY_IDENTITY_STATUS_KEY_FILE"))
	info, err := os.Lstat(keyPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, ErrUnavailable
	}
	key, err := os.ReadFile(keyPath)
	if err != nil || len(key) != 32 {
		return nil, ErrUnavailable
	}
	issuer := strings.TrimSpace(os.Getenv("COMPANY_IDENTITY_ISSUER"))
	if issuer == "" {
		issuer = defaultIssuer
	}
	ids := strings.FieldsFunc(os.Getenv("COMPANY_IDENTITY_KNOWLEDGE_BASE_IDS"),
		func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
	if len(ids) == 0 {
		return nil, ErrUnavailable
	}
	return &Checker{
		origin: origin, issuer: issuer, key: key, knowledgeBases: ids,
		downloads: strings.EqualFold(os.Getenv("COMPANY_IDENTITY_ALLOW_ORIGINAL_DOWNLOADS"), "true"),
		client: &http.Client{Transport: statusTransport, Timeout: 2 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}, nil
}

func RequireEmployeeMCP() bool {
	return strings.EqualFold(os.Getenv("COMPANY_IDENTITY_REQUIRE_EMPLOYEE_MCP"), "true")
}

func (c *Checker) AllowedKnowledgeBaseIDs() []string {
	return append([]string(nil), c.knowledgeBases...)
}

func (c *Checker) OriginalDownloadsAllowed() bool { return c.downloads }

// AuthorizeKnowledgeBases rejects an empty or wider scope; newly added
// workspace libraries never become employee integration grants implicitly.
func (c *Checker) AuthorizeKnowledgeBases(ids []string) error {
	if len(ids) == 0 {
		return ErrDenied
	}
	for _, id := range ids {
		found := false
		for _, allowed := range c.knowledgeBases {
			if id == allowed {
				found = true
				break
			}
		}
		if !found {
			return ErrDenied
		}
	}
	return nil
}

func validIdentifier(value string) bool {
	return value != "" && len(value) <= 256 && !strings.ContainsFunc(value,
		func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}

func (c *Checker) Check(ctx context.Context, binding Binding) (*Member, error) {
	if binding.Issuer != c.issuer || !validIdentifier(binding.Subject) {
		return nil, ErrDenied
	}
	member, err := c.call(ctx, memberStatusPath, map[string]string{"issuer": binding.Issuer, "sub": binding.Subject})
	if err != nil {
		return nil, err
	}
	if member.Subject != binding.Subject {
		return nil, ErrDenied
	}
	return member, nil
}

func (c *Checker) LookupWeCom(ctx context.Context, verifiedUserID string) (*Member, error) {
	if !validIdentifier(verifiedUserID) {
		return nil, ErrDenied
	}
	member, err := c.call(ctx, wecomStatusPath, map[string]string{"issuer": c.issuer, "wecomID": verifiedUserID})
	if err != nil {
		return nil, err
	}
	if member.WeComID != verifiedUserID { // opaque platform ids are case-sensitive
		return nil, ErrDenied
	}
	return member, nil
}

func (c *Checker) CheckWeCom(ctx context.Context, binding Binding, verifiedUserID string) (*Member, error) {
	member, err := c.Check(ctx, binding)
	if err != nil {
		return nil, err
	}
	if member.WeComID != verifiedUserID {
		return nil, ErrDenied
	}
	return member, nil
}

func (c *Checker) call(ctx context.Context, path string, body map[string]string) (*Member, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, ErrUnavailable
	}
	nonceBytes := make([]byte, 24)
	if _, err := rand.Read(nonceBytes); err != nil {
		return nil, ErrUnavailable
	}
	timestamp, nonce := strconv.FormatInt(time.Now().Unix(), 10), hex.EncodeToString(nonceBytes)
	digest := sha256.Sum256(raw)
	canonical := strings.Join([]string{"v1", "POST", path, hex.EncodeToString(digest[:]), timestamp, nonce}, "\n")
	mac := hmac.New(sha256.New, c.key)
	mac.Write([]byte(canonical))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.origin+path, bytes.NewReader(raw))
	if err != nil {
		return nil, ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Identity-Time", timestamp)
	req.Header.Set("X-Identity-Nonce", nonce)
	req.Header.Set("X-Identity-Signature", hex.EncodeToString(mac.Sum(nil)))
	response, err := c.client.Do(req)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, ErrUnavailable
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, 8193))
	if err != nil || len(payload) > 8192 {
		return nil, ErrUnavailable
	}
	var result struct {
		Active  bool `json:"active"`
		Allowed bool `json:"allowed"`
		Member
	}
	if json.Unmarshal(payload, &result) != nil {
		return nil, ErrUnavailable
	}
	if !result.Active || !result.Allowed || result.Issuer != c.issuer ||
		!validIdentifier(result.Subject) || !validIdentifier(result.WeComID) || result.CreatedTime == "" {
		return nil, ErrDenied
	}
	return &result.Member, nil
}
