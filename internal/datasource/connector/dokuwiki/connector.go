package dokuwiki

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/utils"
)

var (
	_           datasource.FullStreamingConnector = (*Connector)(nil)
	codeOrTable                                   = regexp.MustCompile(`(?im)^\s*(?:\^|\||<code\b|<file\b|  +[^*\s-])`)
	heading                                       = regexp.MustCompile(`(?m)^\s*=+.*?=+\s*$`)
	markup                                        = regexp.MustCompile(`(?s)\[\[.*?\]\]|\{\{.*?\}\}|~~[^~]*~~|<[^>]+>`)
	decoration                                    = regexp.MustCompile(`[\s*•#_:~\\/|>\-]+`)
	image                                         = regexp.MustCompile(`(?i)\{\{[^}]*\.(?:png|jpe?g|gif|webp|bmp|tiff?)(?:[?|}])`)
)

// Connector has an immutable administrator-defined source allowlist.
type Connector struct{ sources map[string]Source }

func (*Connector) Type() string { return types.ConnectorTypeDokuWikiFilesystem }

func (c *Connector) Validate(ctx context.Context, ds *types.DataSourceConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cfg, err := c.parseConfig(ds)
	if err != nil {
		return err
	}
	s, err := openSource(cfg.source)
	if err != nil {
		return err
	}
	defer s.root.Close()
	return nil
}

// VerifyFetchedItem is an optional publication guard used after asynchronous
// parsing. A changed source or ACL must leave the previously published version
// intact instead of publishing bytes whose authorization/version has changed.
func (c *Connector) VerifyFetchedItem(ctx context.Context, ds *types.DataSourceConfig, item *types.FetchedItem) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cfg, err := c.parseConfig(ds)
	if err != nil {
		return err
	}
	if item == nil || item.Metadata["source_alias"] != cfg.alias || !validDigest(item.Metadata["sha256"]) {
		return fmt.Errorf("%w: invalid DokuWiki publication identity", datasource.ErrInvalidConfig)
	}
	rel := item.Metadata["relative_path"]
	area, tail, found := strings.Cut(rel, "/")
	if !found || !safePath(rel) || (area != "pages" && area != "media") {
		return fmt.Errorf("%w: invalid DokuWiki publication path", datasource.ErrInvalidConfig)
	}
	ns := path.Dir(tail)
	if ns == "." {
		ns = ""
	}
	ns = strings.ReplaceAll(ns, "/", ":")
	if !cfg.selected(ns) {
		return fmt.Errorf("%w: DokuWiki publication is outside the allowed selection", datasource.ErrInvalidConfig)
	}
	s, err := openSource(cfg.source)
	if err != nil {
		return err
	}
	defer s.root.Close()
	info, err := s.noLinks(rel)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > utils.GetMaxFileSize() {
		return datasource.ErrFetchFailed
	}
	e := entry{rel: rel, namespace: ns, info: info}
	content, err := s.read(e, utils.GetMaxFileSize())
	if err != nil {
		return err
	}
	hash := digest(content)
	current := fetchedItem(cfg, e, content, hash)
	if hash != item.Metadata["sha256"] || current.ExternalID != item.ExternalID {
		return fmt.Errorf("%w: DokuWiki source changed before publication", datasource.ErrFetchFailed)
	}
	return s.healthy()
}

func (c *Connector) ListResources(ctx context.Context, ds *types.DataSourceConfig, parentID string) ([]types.Resource, error) {
	cfg, err := c.parseConfig(ds)
	if err != nil {
		return nil, err
	}
	s, err := openSource(cfg.source)
	if err != nil {
		return nil, err
	}
	defer s.root.Close()
	if parentID == "" {
		return []types.Resource{{ExternalID: "namespace:", Name: "DokuWiki", Type: "namespace", HasChildren: true}}, nil
	}
	parent, err := parseResourceID(parentID)
	if err != nil {
		return nil, err
	}
	entries, err := s.inventory(ctx)
	if err != nil {
		return nil, err
	}
	nodes := make(map[string]bool)
	for _, e := range entries {
		if !cfg.allowed(e.namespace) || !within(e.namespace, parent) || e.namespace == parent {
			continue
		}
		remainder := e.namespace
		if parent != "" {
			remainder = strings.TrimPrefix(remainder, parent+":")
		}
		parts := strings.Split(remainder, ":")
		child := parts[0]
		if parent != "" {
			child = parent + ":" + child
		}
		nodes[child] = nodes[child] || len(parts) > 1
	}
	out := make([]types.Resource, 0, len(nodes))
	for ns, hasChildren := range nodes {
		parts := strings.Split(ns, ":")
		out = append(out, types.Resource{ExternalID: "namespace:" + ns, Name: parts[len(parts)-1], Type: "namespace", ParentID: parentID, HasChildren: hasChildren})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExternalID < out[j].ExternalID })
	return out, s.healthy()
}

func (c *Connector) ResolveResourceAncestors(ctx context.Context, ds *types.DataSourceConfig, ids []string) ([]string, error) {
	if err := c.Validate(ctx, ds); err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	for _, id := range ids {
		ns, err := parseResourceID(id)
		if err != nil {
			return nil, err
		}
		if ns == "" {
			continue
		}
		seen["namespace:"] = true
		parts := strings.Split(ns, ":")
		for i := 1; i < len(parts); i++ {
			seen["namespace:"+strings.Join(parts[:i], ":")] = true
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

type collector struct{ items []types.FetchedItem }

func (h *collector) Emit(_ context.Context, item types.FetchedItem) error {
	h.items = append(h.items, item)
	return nil
}
func (*collector) Checkpoint(context.Context, *types.SyncCursor) error { return nil }

func (c *Connector) FetchAll(ctx context.Context, ds *types.DataSourceConfig, resourceIDs []string) ([]types.FetchedItem, error) {
	if ds == nil {
		return nil, datasource.ErrInvalidConfig
	}
	copyDS := *ds
	copyDS.ResourceIDs = resourceIDs
	items, _, err := c.FetchIncremental(ctx, &copyDS, nil)
	return items, err
}

func (c *Connector) FetchIncremental(ctx context.Context, ds *types.DataSourceConfig, cursor *types.SyncCursor) ([]types.FetchedItem, *types.SyncCursor, error) {
	h := &collector{}
	next, err := c.FetchStream(ctx, ds, cursor, h)
	return h.items, next, err
}

func (c *Connector) FetchStream(ctx context.Context, ds *types.DataSourceConfig, cursor *types.SyncCursor, h datasource.StreamHandler) (*types.SyncCursor, error) {
	return c.sync(ctx, ds, cursor, h, false)
}

func (c *Connector) FetchFullStream(ctx context.Context, ds *types.DataSourceConfig, cursor *types.SyncCursor, h datasource.StreamHandler) (*types.SyncCursor, error) {
	return c.sync(ctx, ds, cursor, h, true)
}

type syncState struct {
	Version        int               `json:"version"`
	Binding        string            `json:"binding"`
	Files          map[string]string `json:"files"`
	Failed         map[string]string `json:"failed"`
	Excluded       map[string]string `json:"excluded"`
	ExcludedCounts map[string]int    `json:"excluded_counts"`
	ScanComplete   bool              `json:"scan_complete"`
}

func digest(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
func validDigest(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == sha256.Size
}

func (state *syncState) cursor(completed time.Time) (*types.SyncCursor, error) {
	// Round-trip makes a detached complete snapshot even for handlers that retain
	// it. A later Emit failure must never mutate a previously saved checkpoint.
	b, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	var values map[string]interface{}
	if err := json.Unmarshal(b, &values); err != nil {
		return nil, err
	}
	return &types.SyncCursor{LastSyncTime: completed, ConnectorCursor: values}, nil
}

func (c *Connector) sync(ctx context.Context, ds *types.DataSourceConfig, old *types.SyncCursor, h datasource.StreamHandler, full bool) (*types.SyncCursor, error) {
	cfg, err := c.parseConfig(ds)
	if err != nil {
		return nil, err
	}
	if len(cfg.namespaces) == 0 || h == nil {
		return nil, fmt.Errorf("%w: select at least one DokuWiki namespace", datasource.ErrInvalidConfig)
	}
	bindingData, _ := json.Marshal(struct {
		Alias  string
		Source Source
	}{cfg.alias, cfg.source})
	state := &syncState{Version: 1, Binding: digest(bindingData), Files: make(map[string]string)}
	var lastComplete time.Time
	if old != nil && len(old.ConnectorCursor) > 0 {
		b, marshalErr := json.Marshal(old.ConnectorCursor)
		if marshalErr != nil || json.Unmarshal(b, state) != nil || state.Version != 1 || state.Binding != digest(bindingData) {
			return nil, fmt.Errorf("%w: DokuWiki source binding or server policy changed; review existing publications before resetting the cursor", datasource.ErrInvalidConfig)
		}
		lastComplete = old.LastSyncTime
	}
	if state.Files == nil {
		state.Files = make(map[string]string)
	}
	if state.Failed == nil {
		state.Failed = make(map[string]string)
	}
	state.Excluded = make(map[string]string)
	state.ExcludedCounts = make(map[string]int)
	state.ScanComplete = false
	s, err := openSource(cfg.source)
	if err != nil {
		return nil, err
	}
	defer s.root.Close()
	entries, err := s.inventory(ctx)
	if err != nil {
		return nil, err
	}
	exclude := func(rel, reason string) {
		state.Excluded[rel] = reason
		state.ExcludedCounts[reason]++
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !cfg.allowed(e.namespace) {
			state.ExcludedCounts["outside_server_namespace_policy"]++
			continue
		}
		if !cfg.selected(e.namespace) {
			continue
		}
		ext := strings.TrimPrefix(strings.ToLower(path.Ext(e.rel)), ".")
		if _, supported := contentTypes[ext]; !supported || (strings.HasPrefix(e.rel, "pages/") && ext != "txt") {
			exclude(e.rel, "unsupported_format_not_parsed")
			continue
		}
		if !cfg.fileTypes[ext] {
			exclude(e.rel, "file_type_not_selected")
			continue
		}
		if e.info.Size() > utils.GetMaxFileSize() {
			return nil, fmt.Errorf("%w: DokuWiki file exceeds import size limit: %s", datasource.ErrFetchFailed, e.rel)
		}
		if err := s.healthy(); err != nil {
			return nil, err
		}
		content, err := s.read(e, utils.GetMaxFileSize())
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(e.rel, "pages/") {
			if !utf8.Valid(content) || strings.IndexByte(string(content), 0) >= 0 {
				return nil, fmt.Errorf("%w: DokuWiki page is not lossless UTF-8: %s", datasource.ErrFetchFailed, e.rel)
			}
			if reason := navigationReason(string(content)); reason != "" {
				exclude(e.rel, reason)
				continue
			}
		}
		if len(content) == 0 {
			exclude(e.rel, "empty_source_not_parsed")
			continue
		}
		hash := digest(content)
		if !full && state.Files[e.rel] == hash {
			delete(state.Failed, e.rel)
			continue
		}
		if err := s.healthy(); err != nil {
			return nil, err
		}
		item := fetchedItem(cfg, e, content, hash)
		if err := h.Emit(ctx, item); err != nil {
			var rejected *datasource.DocumentParseFailure
			if !errors.As(err, &rejected) {
				return nil, err
			}
			// Only an explicit content rejection may continue. Keep the last
			// published hash, so this document is retried on the next scan.
			state.Failed[e.rel] = rejected.Reason
			checkpoint, cursorErr := state.cursor(lastComplete)
			if cursorErr != nil {
				return nil, cursorErr
			}
			if err := h.Checkpoint(ctx, checkpoint); err != nil {
				return nil, err
			}
			continue
		}
		// Emit must confirm ingestion success before this document is acknowledged.
		state.Files[e.rel] = hash
		delete(state.Failed, e.rel)
		checkpoint, err := state.cursor(lastComplete)
		if err != nil {
			return nil, err
		}
		if err := h.Checkpoint(ctx, checkpoint); err != nil {
			return nil, err
		}
	}
	after, err := s.inventory(ctx)
	if err != nil {
		return nil, err
	}
	if !sameInventory(entries, after) {
		return nil, fmt.Errorf("%w: DokuWiki source changed during scan; retry from saved checkpoints", datasource.ErrFetchFailed)
	}
	state.ScanComplete = true
	next, err := state.cursor(time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if err := h.Checkpoint(ctx, next); err != nil {
		return nil, err
	}
	logger.Infof(ctx, "[DokuWiki] source=%s scanned=%d failed=%d exclusions=%v deletion_sync=unsupported", cfg.alias, len(entries), len(state.Failed), state.ExcludedCounts)
	return next, nil
}

func navigationReason(text string) string {
	text = strings.TrimPrefix(text, "\ufeff")
	if strings.TrimSpace(text) == "" {
		return "empty_page_not_parsed"
	}
	if codeOrTable.MatchString(text) {
		return ""
	}
	body := decoration.ReplaceAllString(markup.ReplaceAllString(heading.ReplaceAllString(text, ""), ""), "")
	if body != "" {
		return ""
	}
	if image.MatchString(text) {
		return "image_only_page_requires_ocr_review"
	}
	return "pure_links_or_download_list_not_parsed"
}

func fetchedItem(cfg *selection, e entry, content []byte, hash string) types.FetchedItem {
	area, tail, _ := strings.Cut(e.rel, "/")
	wikiID := strings.ReplaceAll(tail, "/", ":")
	if area == "pages" {
		wikiID = strings.TrimSuffix(wikiID, ".txt")
	}
	kind := "media"
	if area == "pages" {
		kind = "wiki"
	}
	sourceID := kind + ":" + wikiID
	u, _ := url.Parse(cfg.source.PublicBaseURL + "/")
	query := url.Values{}
	if area == "pages" {
		query.Set("id", wikiID)
	} else {
		u.Path = strings.TrimRight(u.Path, "/") + "/lib/exe/fetch.php"
		query.Set("media", wikiID)
	}
	u.RawQuery = query.Encode()
	ext := strings.TrimPrefix(strings.ToLower(path.Ext(e.rel)), ".")
	return types.FetchedItem{
		ExternalID: "dokuwiki:" + digest([]byte(cfg.alias+"\x00"+sourceID)),
		Title:      path.Base(e.rel), Content: content, ContentType: contentTypes[ext], FileName: path.Base(e.rel),
		URL: u.String(), UpdatedAt: e.info.ModTime().UTC(), SourceResourceID: "namespace:" + e.namespace,
		Metadata: map[string]string{
			"source_alias": cfg.alias, "source_id": sourceID, "source_url": u.String(),
			"original_name": path.Base(e.rel), "relative_path": e.rel, "namespace": e.namespace,
			"sha256": hash, "content_sha256": hash, "source_type": types.ConnectorTypeDokuWikiFilesystem,
		},
	}
}
