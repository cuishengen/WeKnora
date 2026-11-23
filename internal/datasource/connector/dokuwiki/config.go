// Package dokuwiki reads an administrator-mounted DokuWiki data directory.
// It never authenticates to the wiki or writes to its source files.
package dokuwiki

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/types"
)

// SourcesEnv contains a JSON object keyed by a safe, public source alias.
// Paths and source URLs are deployment configuration, never browser input.
const SourcesEnv = "WEKNORA_DOKUWIKI_SOURCES"

// Source defines a trusted, read-only mount. Namespace policies constrain all
// selections; they are not a replacement for the target knowledge base ACL.
type Source struct {
	DataRoot          string   `json:"data_root"`
	PublicBaseURL     string   `json:"public_base_url"`
	Sentinel          string   `json:"sentinel"`
	AllowedNamespaces []string `json:"allowed_namespaces,omitempty"`
	DeniedNamespaces  []string `json:"denied_namespaces,omitempty"`
	ACLPath           string   `json:"acl_path,omitempty"`
	ExpectedACLSHA256 string   `json:"expected_acl_sha256,omitempty"`
}

type selection struct {
	alias      string
	source     Source
	namespaces []string
	fileTypes  map[string]bool
}

var contentTypes = map[string]string{
	"txt":  "text/plain",
	"pdf":  "application/pdf",
	"xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	"xls":  "application/vnd.ms-excel",
	"docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
}

// NewConnectorFromEnv loads the server allowlist once at startup. An empty
// allowlist leaves the type visible but no filesystem source accessible.
func NewConnectorFromEnv() (*Connector, error) {
	sources := make(map[string]Source)
	if raw := os.Getenv(SourcesEnv); strings.TrimSpace(raw) != "" {
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&sources); err != nil {
			return nil, fmt.Errorf("%w: invalid %s JSON", datasource.ErrInvalidConfig, SourcesEnv)
		}
		if err := dec.Decode(new(interface{})); err != io.EOF {
			return nil, fmt.Errorf("%w: trailing data in %s", datasource.ErrInvalidConfig, SourcesEnv)
		}
	}
	return NewConnector(sources)
}

// NewConnector validates and copies trusted deployment configuration.
func NewConnector(sources map[string]Source) (*Connector, error) {
	copySources := make(map[string]Source, len(sources))
	for alias, src := range sources {
		if alias == "" || strings.ContainsAny(alias, "/\\:\x00") || alias == "." || alias == ".." {
			return nil, fmt.Errorf("%w: invalid DokuWiki source alias", datasource.ErrInvalidConfig)
		}
		u, err := url.Parse(src.PublicBaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
			u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
			!filepath.IsAbs(src.DataRoot) || !safePath(src.Sentinel) ||
			!strings.HasPrefix(src.Sentinel, "pages/") || path.Ext(src.Sentinel) != ".txt" {
			return nil, fmt.Errorf("%w: invalid DokuWiki source deployment configuration", datasource.ErrInvalidConfig)
		}
		for _, ns := range append(append([]string{}, src.AllowedNamespaces...), src.DeniedNamespaces...) {
			if !validNamespace(ns) {
				return nil, fmt.Errorf("%w: invalid DokuWiki server namespace policy", datasource.ErrInvalidConfig)
			}
		}
		if (src.ACLPath == "") != (src.ExpectedACLSHA256 == "") ||
			(src.ACLPath != "" && (!filepath.IsAbs(src.ACLPath) || !validDigest(src.ExpectedACLSHA256))) {
			return nil, fmt.Errorf("%w: DokuWiki ACL guard requires an absolute path and SHA-256", datasource.ErrInvalidConfig)
		}
		src.DataRoot = filepath.Clean(src.DataRoot)
		src.PublicBaseURL = strings.TrimRight(src.PublicBaseURL, "/")
		src.AllowedNamespaces = append([]string{}, src.AllowedNamespaces...)
		src.DeniedNamespaces = append([]string{}, src.DeniedNamespaces...)
		copySources[alias] = src
	}
	return &Connector{sources: copySources}, nil
}

func (c *Connector) parseConfig(ds *types.DataSourceConfig) (*selection, error) {
	if ds == nil || len(ds.Credentials) != 0 {
		return nil, fmt.Errorf("%w: DokuWiki uses a server source alias, not credentials", datasource.ErrInvalidConfig)
	}
	for key := range ds.Settings {
		if key != "source_alias" && key != "file_types" {
			return nil, fmt.Errorf("%w: unsupported DokuWiki setting", datasource.ErrInvalidConfig)
		}
	}
	alias, ok := ds.Settings["source_alias"].(string)
	src, exists := c.sources[alias]
	if !ok || !exists {
		return nil, fmt.Errorf("%w: DokuWiki source alias is not enabled on this server", datasource.ErrInvalidConfig)
	}
	cfg := &selection{alias: alias, source: src, fileTypes: make(map[string]bool)}
	if value, exists := ds.Settings["file_types"]; exists {
		var names []string
		switch v := value.(type) {
		case []string:
			names = v
		case []interface{}:
			for _, raw := range v {
				name, ok := raw.(string)
				if !ok {
					return nil, datasource.ErrInvalidConfig
				}
				names = append(names, name)
			}
		default:
			return nil, datasource.ErrInvalidConfig
		}
		for _, name := range names {
			if _, ok := contentTypes[name]; !ok {
				return nil, fmt.Errorf("%w: unsupported DokuWiki file type", datasource.ErrInvalidConfig)
			}
			cfg.fileTypes[name] = true
		}
		if len(names) == 0 {
			return nil, fmt.Errorf("%w: select at least one DokuWiki file type", datasource.ErrInvalidConfig)
		}
	} else {
		for name := range contentTypes {
			cfg.fileTypes[name] = true
		}
	}
	for _, id := range ds.ResourceIDs {
		ns, err := parseResourceID(id)
		if err != nil {
			return nil, err
		}
		cfg.namespaces = append(cfg.namespaces, ns)
	}
	sort.Strings(cfg.namespaces)
	return cfg, nil
}

func safePath(p string) bool {
	if p == "" || p == "." || path.Clean(p) != p || strings.ContainsAny(p, "\\:\x00") || strings.HasPrefix(p, "/") {
		return false
	}
	for _, component := range strings.Split(p, "/") {
		if component == "." || component == ".." || component == "" {
			return false
		}
	}
	return true
}

func validNamespace(ns string) bool {
	if ns == "" {
		return true
	}
	return safePath(strings.ReplaceAll(ns, ":", "/"))
}

func parseResourceID(id string) (string, error) {
	if !strings.HasPrefix(id, "namespace:") || !validNamespace(strings.TrimPrefix(id, "namespace:")) {
		return "", fmt.Errorf("%w: invalid DokuWiki namespace resource", datasource.ErrInvalidConfig)
	}
	return strings.TrimPrefix(id, "namespace:"), nil
}

func within(ns, prefix string) bool {
	return prefix == "" || ns == prefix || strings.HasPrefix(ns, prefix+":")
}

func (cfg *selection) allowed(ns string) bool {
	for _, denied := range cfg.source.DeniedNamespaces {
		if within(ns, denied) {
			return false
		}
	}
	if len(cfg.source.AllowedNamespaces) == 0 {
		return true
	}
	for _, allowed := range cfg.source.AllowedNamespaces {
		if within(ns, allowed) {
			return true
		}
	}
	return false
}

func (cfg *selection) selected(ns string) bool {
	for _, selected := range cfg.namespaces {
		if within(ns, selected) {
			return cfg.allowed(ns)
		}
	}
	return false
}
