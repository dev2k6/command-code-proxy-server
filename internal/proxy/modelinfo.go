package proxy

import (
	"context"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dev2k6/command-code-proxy-server/internal/api"
)

const (
	// modelsDocURL is the public Command Code docs page that lists every model
	// with its display name and a one-line "best for" description.
	modelsDocURL = "https://commandcode.ai/docs/reference/cli/models"

	// modelInfoFetchInterval — refresh model info every 24 hours. Descriptions
	// change infrequently; daily refresh is plenty.
	modelInfoFetchInterval = 24 * time.Hour

	// modelInfoFetchTimeout — per-request timeout.
	modelInfoFetchTimeout = 15 * time.Second
)

// ModelInfo describes a single model: its display name, best-for description,
// and capabilities (e.g., "text, vision").
type ModelInfo struct {
	ID           string `json:"id"`            // matches upstream model ID (lowercase for lookup)
	DisplayName  string `json:"display_name"`  // human-friendly name like "Claude Sonnet 4.6"
	Description  string `json:"description"`   // one-line "best for" description
	Capabilities string `json:"capabilities"`  // comma-separated: "text", "text, vision"
}

// ModelInfoCache holds scraped model metadata with thread-safe access.
type ModelInfoCache struct {
	mu        sync.RWMutex
	infos     map[string]ModelInfo // key: model ID (lowercase)
	fetchedAt time.Time
	client    *http.Client
}

// NewModelInfoCache creates an empty cache.
func NewModelInfoCache() *ModelInfoCache {
	return &ModelInfoCache{
		infos:  make(map[string]ModelInfo),
		client: &http.Client{Timeout: modelInfoFetchTimeout},
	}
}

// Get returns the model info for a given ID, or zero-value if unknown.
// Tries exact match first, then fuzzy match by name.
func (c *ModelInfoCache) Get(modelID string) (ModelInfo, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	lower := strings.ToLower(modelID)
	if info, ok := c.infos[lower]; ok {
		return info, true
	}
	// Fuzzy: last path component normalized
	lastPath := lower
	if i := strings.LastIndex(lower, "/"); i >= 0 {
		lastPath = lower[i+1:]
	}
	for k, info := range c.infos {
		// Try matching last path of stored key
		storedLast := k
		if i := strings.LastIndex(k, "/"); i >= 0 {
			storedLast = k[i+1:]
		}
		if normalizeModelKey(lastPath) == normalizeModelKey(storedLast) {
			return info, true
		}
	}
	return ModelInfo{}, false
}

// normalizeModelKey strips hyphens, dots, and lowercases for fuzzy comparison.
func normalizeModelKey(s string) string {
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, ".", "")
	s = strings.ReplaceAll(s, "_", "")
	return strings.ToLower(s)
}

// Refresh scrapes the docs page and rebuilds the cache.
func (c *ModelInfoCache) Refresh() error {
	ctx, cancel := context.WithTimeout(context.Background(), modelInfoFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsDocURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "command-code-proxy/1.0")

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return &ModelInfoFetchError{Status: resp.StatusCode}
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	infos := scrapeModelInfo(string(body))

	c.mu.Lock()
	c.infos = infos
	c.fetchedAt = time.Now()
	c.mu.Unlock()

	log.Printf("[modelinfo] refreshed: %d models", len(infos))
	return nil
}

// ModelInfoFetchError returned on non-200 from upstream docs page.
type ModelInfoFetchError struct {
	Status int
}

func (e *ModelInfoFetchError) Error() string {
	return "model info fetch failed: status " + http.StatusText(e.Status)
}

// StartBackgroundRefresh launches a goroutine that periodically refreshes info.
func (c *ModelInfoCache) StartBackgroundRefresh() {
	go func() {
		if err := c.Refresh(); err != nil {
			log.Printf("[modelinfo] initial refresh failed (descriptions will be empty): %v", err)
		}
		ticker := time.NewTicker(modelInfoFetchInterval)
		defer ticker.Stop()
		for range ticker.C {
			if err := c.Refresh(); err != nil {
				log.Printf("[modelinfo] refresh failed (keeping existing): %v", err)
			}
		}
	}()
}

// scrapeModelInfo parses the docs HTML and extracts (id, name, description, capabilities)
// for each model. Returns a map keyed by lowercase model ID.
func scrapeModelInfo(html string) map[string]ModelInfo {
	infos := make(map[string]ModelInfo)

	// Pattern matches rows in the docs models table:
	// <code>model-id</code> ... <td>Name</td> <td>best-for description</td> <td>capabilities</td>
	rowPattern := regexp.MustCompile(
		`<code[^>]*>([^<]+)</code>.*?<td[^>]*>\s*([A-Z][^<]+?)\s*</td>\s*<td[^>]*>([^<]+?)</td>\s*<td[^>]*>([^<]+?)</td>`,
	)
	matches := rowPattern.FindAllStringSubmatch(html, -1)
	for _, m := range matches {
		if len(m) < 5 {
			continue
		}
		id := strings.TrimSpace(m[1])
		name := strings.TrimSpace(m[2])
		desc := strings.TrimSpace(m[3])
		caps := strings.TrimSpace(m[4])

		// Skip placeholder/empty
		if strings.HasPrefix(id, "-") || id == "" {
			continue
		}
		if strings.Contains(desc, "&amp;") {
			desc = strings.ReplaceAll(desc, "&amp;", "&")
		}

		infos[strings.ToLower(id)] = ModelInfo{
			ID:           id,
			DisplayName:  name,
			Description:  desc,
			Capabilities: caps,
		}
	}

	if len(infos) == 0 {
		log.Printf("[modelinfo] WARNING: no models parsed from docs page — HTML may have changed")
	}
	return infos
}

// AttachInfo enriches an OpenAIModel with display name and description
// if available in the cache. Mutates the model in-place.
func (c *ModelInfoCache) AttachInfo(model *api.OpenAIModel) {
	if c == nil || model == nil {
		return
	}
	info, ok := c.Get(model.ID)
	if !ok {
		// Try hardcoded fallbacks for models not in the docs table
		if fallback, exists := hardcodedInfo[strings.ToLower(model.ID)]; exists {
			model.DisplayName = fallback.DisplayName
			model.Description = fallback.Description
			model.Capabilities = fallback.Capabilities
		}
		return
	}
	model.DisplayName = info.DisplayName
	model.Description = info.Description
	if info.Capabilities != "" {
		model.Capabilities = info.Capabilities
	}
}

// hardcodedInfo provides descriptions for models that exist upstream but aren't
// listed in the public docs page. These are kept minimal and conservative.
var hardcodedInfo = map[string]ModelInfo{
	"claude-haiku-4-5-20251001": {
		ID:           "claude-haiku-4-5-20251001",
		DisplayName:  "Claude Haiku 4.5",
		Description:  "fast, compact model for high-throughput tasks",
		Capabilities: "text, vision",
	},
	"zai-org/glm-5.2": {
		ID:           "zai-org/GLM-5.2",
		DisplayName:  "GLM 5.2",
		Description:  "extended-context autonomous coding agent",
		Capabilities: "text",
	},
}
