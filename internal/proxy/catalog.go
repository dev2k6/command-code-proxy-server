package proxy

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dev2k6/command-code-proxy-server/internal/api"
)

const (
	modelCatalogURL             = "https://commandcode.ai/docs/reference/cli/models"
	modelCatalogRefreshEvery    = 6 * time.Hour
	modelCatalogRetryAfter      = 5 * time.Minute
	modelCatalogMaxResponseSize = 8 << 20
)

// modelIDRowRegex matches a model-id <code> block inside the models reference
// page. The link target is always https://commandcode.ai/models/<slug> and the
// code block is the canonical id (e.g. "deepseek/deepseek-v4-flash").
var modelIDRowRegex = regexp.MustCompile(`<a href="https://commandcode\.ai/models/[^"]+"[^>]*><code>([^<]+)</code></a>`)

// modelCatalog holds the latest model list fetched from CommandCode.
// It is populated in the background by StartModelRefresher.
type modelCatalog struct {
	mu      sync.RWMutex
	ids     []string
	byID    map[string]string
	byShort map[string]string
	byNorm  map[string]string
	loaded  bool
}

var catalog = &modelCatalog{}

// update replaces the catalog contents and rebuilds lookup indexes.
func (c *modelCatalog) update(ids []string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)

	c.ids = sorted
	c.byID = map[string]string{}
	c.byShort = map[string]string{}
	c.byNorm = map[string]string{}

	for _, id := range sorted {
		lower := strings.ToLower(id)
		c.byID[lower] = id

		short := id
		if i := strings.LastIndex(id, "/"); i >= 0 {
			short = id[i+1:]
		}
		ls := strings.ToLower(short)
		if _, ok := c.byShort[ls]; !ok {
			c.byShort[ls] = id
		}
		key := normalizeModelID(short)
		if _, ok := c.byNorm[key]; !ok {
			c.byNorm[key] = id
		}
	}
	c.loaded = true
}

// resolve maps a client model name to a canonical CommandCode model id using
// the fetched catalog. It supports exact ids, short names (name after "/")
// and punctuation-insensitive matches. It returns "" when the catalog is not
// loaded yet or nothing matches.
func (c *modelCatalog) resolve(name string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if !c.loaded {
		return ""
	}
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		return ""
	}
	if id, ok := c.byID[key]; ok {
		return id
	}
	if id, ok := c.byShort[key]; ok {
		return id
	}
	if id, ok := c.byNorm[normalizeModelID(key)]; ok {
		return id
	}
	return ""
}

// loaded reports whether the catalog has been populated at least once.
func (c *modelCatalog) isLoaded() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.loaded
}

// idList returns a sorted copy of the catalog model ids.
func (c *modelCatalog) idList() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]string(nil), c.ids...)
}

// normalizeModelID strips every non-alphanumeric character and lowercases.
func normalizeModelID(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// parseModelIDs extracts canonical model ids from the CommandCode models
// reference page HTML.
func parseModelIDs(html string) []string {
	seen := map[string]bool{}
	var ids []string
	for _, m := range modelIDRowRegex.FindAllStringSubmatch(html, -1) {
		id := strings.TrimSpace(m[1])
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}

// fetchModelIDs fetches the CommandCode models reference page and parses the
// model ids out of it.
func fetchModelIDs() ([]string, error) {
	return fetchModelIDsFrom(modelCatalogURL)
}

// fetchModelIDsFrom fetches url and parses model ids out of the response.
func fetchModelIDsFrom(url string) ([]string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetch model catalog: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("model catalog returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, modelCatalogMaxResponseSize))
	if err != nil {
		return nil, fmt.Errorf("read model catalog: %w", err)
	}

	ids := parseModelIDs(string(body))
	if len(ids) == 0 {
		return nil, fmt.Errorf("no model ids found in catalog")
	}
	return ids, nil
}

// StartModelRefresher fetches the CommandCode model catalog in the background
// and keeps it fresh. It runs until the process exits.
func (p *Proxy) StartModelRefresher() {
	go func() {
		for {
			ids, err := fetchModelIDs()
			if err != nil {
				log.Printf("model catalog refresh failed: %v", err)
				time.Sleep(modelCatalogRetryAfter)
				continue
			}
			catalog.update(ids)
			if p.Debug {
				log.Printf("model catalog updated: %d models", len(ids))
			}
			time.Sleep(modelCatalogRefreshEvery)
		}
	}()
}

// ownedByFor derives an owned_by value from a model id.
func ownedByFor(id string) string {
	if i := strings.LastIndex(id, "/"); i >= 0 {
		return id[:i]
	}
	return "commandcode"
}

// catalogModels builds the OpenAI-compatible model list from the catalog.
func catalogModels() []api.OpenAIModel {
	ids := catalog.idList()
	out := make([]api.OpenAIModel, 0, len(ids))
	for _, id := range ids {
		out = append(out, api.OpenAIModel{
			ID:      id,
			Object:  "model",
			Created: 0,
			OwnedBy: ownedByFor(id),
		})
	}
	return out
}

// staticModels is the fallback list used until the catalog is fetched.
func staticModels() []api.OpenAIModel {
	return []api.OpenAIModel{
		// MoonshotAI
		{ID: "moonshotai/Kimi-K2.6", Object: "model", Created: 0, OwnedBy: "moonshotai"},
		{ID: "moonshotai/Kimi-K2.5", Object: "model", Created: 0, OwnedBy: "moonshotai"},
		// ZhipuAI
		{ID: "zai-org/GLM-5.1", Object: "model", Created: 0, OwnedBy: "zhipuai"},
		{ID: "zai-org/GLM-5", Object: "model", Created: 0, OwnedBy: "zhipuai"},
		// MiniMaxAI
		{ID: "MiniMaxAI/MiniMax-M2.7", Object: "model", Created: 0, OwnedBy: "minimaxai"},
		{ID: "MiniMaxAI/MiniMax-M2.5", Object: "model", Created: 0, OwnedBy: "minimaxai"},
		{ID: "MiniMaxAI/MiniMax-M3", Object: "model", Created: 0, OwnedBy: "minimaxai"},
		// DeepSeek
		{ID: "deepseek/deepseek-v4-pro", Object: "model", Created: 0, OwnedBy: "deepseek"},
		{ID: "deepseek/deepseek-v4-flash", Object: "model", Created: 0, OwnedBy: "deepseek"},
		// Qwen
		{ID: "Qwen/Qwen3.6-Max-Preview", Object: "model", Created: 0, OwnedBy: "qwen"},
		{ID: "Qwen/Qwen3.6-Plus", Object: "model", Created: 0, OwnedBy: "qwen"},
		// StepFun
		{ID: "stepfun/Step-3.5-Flash", Object: "model", Created: 0, OwnedBy: "stepfun"},
		{ID: "stepfun/Step-3.7-Flash", Object: "model", Created: 0, OwnedBy: "stepfun"},
		// Qwen (3.7 line)
		{ID: "Qwen/Qwen3.7-Max-Free", Object: "model", Created: 0, OwnedBy: "qwen"},
		{ID: "Qwen/Qwen3.7-Max", Object: "model", Created: 0, OwnedBy: "qwen"},
		// Xiaomi MiMo
		{ID: "xiaomi/mimo-v2.5-pro", Object: "model", Created: 0, OwnedBy: "xiaomi"},
		{ID: "xiaomi/mimo-v2.5", Object: "model", Created: 0, OwnedBy: "xiaomi"},
		// Google
		{ID: "google/gemini-3.1-flash-lite", Object: "model", Created: 0, OwnedBy: "google"},
	}
}
