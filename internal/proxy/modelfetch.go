package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dev2k6/command-code-proxy-server/internal/api"
	"github.com/dev2k6/command-code-proxy-server/internal/version"
)

const (
	// upstreamModelsURL is Command Code's Provider API models endpoint
	upstreamModelsURL = "https://api.commandcode.ai/provider/v1/models"

	// upstreamProbeURL is the chat endpoint we use to probe whether a model is
	// actually reachable. We send a minimal request — if it returns non-403
	// (e.g., 200 with content, or 400 for bad request), the model exists.
	upstreamProbeURL = "https://api.commandcode.ai/alpha/generate"

	// modelCacheTTL is how often we refresh the model list from upstream
	modelCacheTTL = 6 * time.Hour

	// modelFetchTimeout is the per-request timeout for upstream model fetching
	modelFetchTimeout = 10 * time.Second

	// modelProbeConcurrency caps how many models we probe in parallel.
	// Upstream gets cranky if we hammer with 30 concurrent requests.
	modelProbeConcurrency = 4
)

// UpstreamModel represents a single model in the upstream Command Code API
// response (OpenAI-compatible /v1/models format).
type UpstreamModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// UpstreamModelList is the response wrapper for /v1/models
type UpstreamModelList struct {
	Object string          `json:"object"`
	Data   []UpstreamModel `json:"data"`
}

// ModelCache holds the dynamically-fetched model list with thread-safe access.
type ModelCache struct {
	mu          sync.RWMutex
	models      []api.OpenAIModel
	fetchedAt   time.Time
	httpClient  *http.Client
	fetchingNow bool // prevents concurrent refreshes
	pricing     *PricingCache
	modelInfo   *ModelInfoCache
}

// NewModelCache creates an empty cache. Call Refresh() to populate it.
func NewModelCache(pricing *PricingCache, info *ModelInfoCache) *ModelCache {
	return &ModelCache{
		models:    nil,
		pricing:   pricing,
		modelInfo: info,
		httpClient: &http.Client{
			Timeout: modelFetchTimeout,
		},
	}
}

// Get returns a copy of the current cached models. If cache is empty,
// returns the static fallback list from getStaticModels().
func (c *ModelCache) Get() []api.OpenAIModel {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.models) == 0 {
		// Return static fallback enriched with model info (if available)
		static := getStaticModels()
		if c.modelInfo != nil {
			for i := range static {
				c.modelInfo.AttachInfo(&static[i])
			}
		}
		return static
	}
	// Return a copy so callers can't mutate cache state
	out := make([]api.OpenAIModel, len(c.models))
	copy(out, c.models)
	return out
}

// IsStale returns true if the cache needs refreshing.
func (c *ModelCache) IsStale() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return time.Since(c.fetchedAt) > modelCacheTTL || len(c.models) == 0
}

// Refresh fetches the upstream model list and rebuilds the cache.
// Models that 403 upstream (model not recognized) are filtered out.
// On error, logs but keeps the existing cache (or static fallback if empty).
func (c *ModelCache) Refresh(apiKey string) error {
	c.mu.Lock()
	if c.fetchingNow {
		c.mu.Unlock()
		return nil // another goroutine is already refreshing
	}
	c.fetchingNow = true
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.fetchingNow = false
		c.mu.Unlock()
	}()

	models, err := c.fetchAndValidate(apiKey)
	if err != nil {
		return err
	}

	c.mu.Lock()
	c.models = models
	c.fetchedAt = time.Now()
	c.mu.Unlock()

	log.Printf("[models] refreshed cache: %d models (validated against upstream)", len(models))
	return nil
}

// fetchAndValidate fetches the upstream model list, then probes each model
// to verify it's reachable. Models that 403 upstream are filtered out.
func (c *ModelCache) fetchAndValidate(apiKey string) ([]api.OpenAIModel, error) {
	ctx, cancel := context.WithTimeout(context.Background(), modelFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamModelsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "command-code-proxy/1.0")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch upstream: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("upstream status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var upstream UpstreamModelList
	if err := json.NewDecoder(resp.Body).Decode(&upstream); err != nil {
		return nil, fmt.Errorf("decode upstream: %w", err)
	}

	// Probe each model in parallel to filter out non-existent ones.
	probeResults := c.probeModels(apiKey, upstream.Data)

	// Build the validated model list.
	enriched := make([]api.OpenAIModel, 0, len(probeResults))
	for _, pr := range probeResults {
		if !pr.reachable {
			log.Printf("[models] filtered out unreachable model: %s (probe status: %d)", pr.model.ID, pr.status)
			continue
		}
		m := api.OpenAIModel{
			ID:            pr.model.ID,
			Object:        pr.model.Object,
			Created:       pr.model.Created,
			OwnedBy:       pr.model.OwnedBy,
			ContextLength: ContextLengthFor(pr.model.ID),
		}
		// Attach pricing/deal info if available
		if c.pricing != nil {
			if deal, ok := c.pricing.GetDeal(pr.model.ID); ok {
				m.Pricing = &api.ModelPricing{
					Multiplier:  deal.Multiplier,
					Description: deal.Description,
					Status:      deal.Status,
				}
			}
		}
		// Attach display name + description if available
		if c.modelInfo != nil {
			c.modelInfo.AttachInfo(&m)
		}
		enriched = append(enriched, m)
	}
	return enriched, nil
}

// probeResult holds the result of probing a single model.
type probeResult struct {
	model     UpstreamModel
	reachable bool
	status    int
}

// probeModels probes each model in parallel to verify upstream reachability.
// Returns results in input order.
func (c *ModelCache) probeModels(apiKey string, models []UpstreamModel) []probeResult {
	results := make([]probeResult, len(models))
	sem := make(chan struct{}, modelProbeConcurrency)
	var wg sync.WaitGroup

	for i, m := range models {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, model UpstreamModel) {
			defer wg.Done()
			defer func() { <-sem }()
			reachable, status := c.probeModel(apiKey, model.ID)
			results[idx] = probeResult{model: model, reachable: reachable, status: status}
		}(i, m)
	}
	wg.Wait()
	return results
}

// probeModel sends a minimal chat request to verify a model is reachable.
// Returns (true, status) if the model exists (any non-403 response).
// Returns (false, status) if upstream returns 403 (model not recognized).
func (c *ModelCache) probeModel(apiKey, modelID string) (bool, int) {
	ctx, cancel := context.WithTimeout(context.Background(), modelFetchTimeout)
	defer cancel()

	// Build a minimal probe request — empty messages, max 1 token.
	// Upstream will return either:
	//   - 403 MODEL_NOT_IN_PLAN or "model not recognized" — model doesn't exist
	//   - 200 with empty content — model exists but request was minimal
	//   - 400 bad request — model exists, request was malformed
	probeBody := map[string]any{
		"config": map[string]any{
			"workingDir":    ".",
			"date":          time.Now().Format("2006-01-02"),
			"environment":   "cli",
			"structure":     []string{},
			"isGitRepo":     false,
			"currentBranch": "",
			"mainBranch":    "main",
			"gitStatus":     "",
			"recentCommits": []string{},
		},
		"memory": "",
		"taste":  "",
		"skills": "",
		"params": map[string]any{
			"model":       modelID,
			"messages":    []map[string]any{},
			"tools":       []any{},
			"system":      "",
			"max_tokens":  1,
			"temperature": 0.0,
			"stream":      false,
		},
		"threadId": "probe",
	}
	bodyJSON, _ := json.Marshal(probeBody)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamProbeURL, bytes.NewReader(bodyJSON))
	if err != nil {
		return false, 0
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("x-command-code-version", version.GetCommandCodeVersion())
	req.Header.Set("x-cli-environment", "production")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "command-code-proxy/1.0")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, 0
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	// 403 means model not recognized — filter out.
	// Anything else (200, 400, 429, 500) means model exists at this ID.
	return resp.StatusCode != http.StatusForbidden, resp.StatusCode
}

// StartBackgroundRefresh launches a goroutine that periodically refreshes
// the cache. Safe to call once at startup.
func (c *ModelCache) StartBackgroundRefresh(apiKey string) {
	go func() {
		// Initial fetch
		if err := c.Refresh(apiKey); err != nil {
			log.Printf("[models] initial refresh failed (using static fallback): %v", err)
		}

		ticker := time.NewTicker(modelCacheTTL)
		defer ticker.Stop()
		for range ticker.C {
			if err := c.Refresh(apiKey); err != nil {
				log.Printf("[models] refresh failed (keeping existing cache): %v", err)
			}
		}
	}()
}
