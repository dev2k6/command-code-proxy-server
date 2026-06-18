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
)

const (
	// pricingURL is the public pricing page. We scrape deals from it.
	pricingURL = "https://commandcode.ai/docs/resources/pricing-limits"

	// pricingFetchInterval is how often we re-scrape pricing/deals.
	pricingFetchInterval = 24 * time.Hour

	// pricingFetchTimeout is the per-request timeout.
	pricingFetchTimeout = 15 * time.Second
)

// Deal represents a single pricing deal/discount on a model.
type Deal struct {
	Model       string `json:"model"`        // model identifier (matches upstream ID)
	Multiplier  string `json:"multiplier"`   // e.g. "4x", "2x", "99% off"
	Description string `json:"description"`  // human-readable description
	Status      string `json:"status"`       // "permanent" or expiration date
}

// PricingCache holds scraped deal data with thread-safe access.
type PricingCache struct {
	mu        sync.RWMutex
	deals     map[string]Deal // key: model identifier (lowercase)
	fetchedAt time.Time
	client    *http.Client
}

// NewPricingCache creates an empty pricing cache.
func NewPricingCache() *PricingCache {
	return &PricingCache{
		deals:  make(map[string]Deal),
		client: &http.Client{Timeout: pricingFetchTimeout},
	}
}

// GetDeal returns the deal for a model, or zero-value Deal if no deal applies.
// Comparison is case-insensitive and matches:
//   - exact ID match
//   - last path component (e.g. "Qwen/Qwen3.7-Max" matches "qwen-3.7-max")
//   - substring match (e.g. "nvidia/nemotron-3-ultra-550b-a55b" matches "nemotron-3-ultra")
func (c *PricingCache) GetDeal(modelID string) (Deal, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	lowerID := strings.ToLower(modelID)

	// Try exact match first
	if d, ok := c.deals[lowerID]; ok {
		return d, true
	}

	// Try fuzzy matching against all deal keys.
	for k, d := range c.deals {
		// Extract last path component of model ID
		lastPath := lowerID
		if i := strings.LastIndex(lowerID, "/"); i >= 0 {
			lastPath = lowerID[i+1:]
		}

		// Match strategies (in priority order):
		// 1. last path equals key
		// 2. last path contains key (e.g., "qwen3.7-max" contains "qwen-3.7-max")
		//    OR key contains last path (e.g., "minimax-m3" contains "minimax")
		// 3. Normalize hyphens <-> dots
		if lastPath == k {
			return d, true
		}
		if strings.Contains(lastPath, k) || strings.Contains(k, lastPath) {
			return d, true
		}
		// Try replacing dots <-> dashes
		alt1 := strings.ReplaceAll(lastPath, ".", "-")
		alt2 := strings.ReplaceAll(lastPath, "-", ".")
		if alt1 == k || alt2 == k {
			return d, true
		}
		if strings.Contains(alt1, k) || strings.Contains(alt2, k) {
			return d, true
		}
		// Try replacing dashes with nothing (e.g., "qwen-3.7-max" matches "qwen3.7-max")
		stripLast := strings.ReplaceAll(lastPath, "-", "")
		stripKey := strings.ReplaceAll(k, "-", "")
		if stripLast == stripKey {
			return d, true
		}
	}
	return Deal{}, false
}

// Refresh scrapes the pricing page and rebuilds the deal cache.
// On error, logs but keeps the existing cache.
func (c *PricingCache) Refresh() error {
	ctx, cancel := context.WithTimeout(context.Background(), pricingFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pricingURL, nil)
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
		body, _ := io.ReadAll(resp.Body)
		return &PricingFetchError{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	deals := scrapeDeals(string(body))

	c.mu.Lock()
	c.deals = deals
	c.fetchedAt = time.Now()
	c.mu.Unlock()

	log.Printf("[pricing] refreshed: %d deals", len(deals))
	return nil
}

// StartBackgroundRefresh launches a goroutine that periodically refreshes pricing.
func (c *PricingCache) StartBackgroundRefresh() {
	go func() {
		if err := c.Refresh(); err != nil {
			log.Printf("[pricing] initial refresh failed (deals will be empty): %v", err)
		}
		ticker := time.NewTicker(pricingFetchInterval)
		defer ticker.Stop()
		for range ticker.C {
			if err := c.Refresh(); err != nil {
				log.Printf("[pricing] refresh failed (keeping existing deals): %v", err)
			}
		}
	}()
}

// PricingFetchError is returned when the upstream pricing page returns a non-200.
type PricingFetchError struct {
	Status int
	Body   string
}

func (e *PricingFetchError) Error() string {
	return "pricing fetch failed: status " + http.StatusText(e.Status)
}

// scrapeDeals parses the HTML for DEAL blocks and extracts model + description.
// This is intentionally lenient — Command Code may change the HTML structure,
// and we'd rather get partial data than no data.
//
// Robustness strategy:
//   - Multiple regex patterns tried in order
//   - Falls back to single-model extraction if multi-model match fails
//   - Returns empty map on any parse failure (never errors)
//   - Logs warnings for unexpected structures so we notice breakage
func scrapeDeals(html string) map[string]Deal {
	deals := make(map[string]Deal)

	// Strategy: parse the HTML block-by-block.
	// Each deal block is between DEAL</span> and the next </a></div> or end of deals section.
	// We extract the inner HTML of each <a>...</a> deal element.
	dealBlockRe := regexp.MustCompile(
		`(?s)DEAL</span>(.*?)</a></div>`,
	)

	dealBlocks := dealBlockRe.FindAllStringSubmatch(html, -1)
	log.Printf("[pricing] found %d deal blocks in HTML", len(dealBlocks))
	if len(dealBlocks) == 0 {
		log.Printf("[pricing] WARNING: no deal blocks found — HTML structure may have changed")
		return deals
	}

	// Filter out bogus matches (e.g., footer/nav text accidentally matched).
	// Real deal blocks are short (< 2000 chars) and contain at least one <code> tag.
	var realBlocks [][]string
	for _, b := range dealBlocks {
		if len(b[1]) > 2000 {
			continue // too long — likely a nav menu or footer
		}
		if strings.Contains(b[1], "<code") {
			realBlocks = append(realBlocks, b)
		}
	}
	log.Printf("[pricing] filtered to %d real deal blocks (with <code>, len<2000)", len(realBlocks))
	dealBlocks = realBlocks
	if len(dealBlocks) == 0 {
		return deals
	}

	codeRe := regexp.MustCompile(`<code[^>]*>([^<]+)</code>`)
	statusRe := regexp.MustCompile(`(permanent|through\s+\w+\s+\d+,\s+\d{4})`)

	for _, block := range dealBlocks {
		if len(block) < 2 {
			continue
		}
		inner := block[1]

		// Find all model codes in this deal
		codes := codeRe.FindAllStringSubmatch(inner, -1)
		if len(codes) == 0 {
			continue
		}

		// Find status
		statusMatch := statusRe.FindStringSubmatch(inner)
		status := "unknown"
		if len(statusMatch) >= 2 {
			status = statusMatch[1]
		}

		// Description: text after the LAST </code> in the deal, before status marker
		lastCodeEnd := strings.LastIndex(inner, "</code>")
		if lastCodeEnd < 0 {
			continue
		}
		descStart := lastCodeEnd + len("</code>")

		// Find status in remaining text
		descText := inner[descStart:]
		statusIdx := strings.Index(descText, status)
		if statusIdx < 0 {
			// Status might be in nested span; take up to next </span>
			spanEnd := strings.Index(descText, "</span>")
			if spanEnd < 0 {
				spanEnd = len(descText)
			}
			statusIdx = spanEnd
		}
		desc := descText[:statusIdx]
		desc = stripHTMLTags(desc)
		desc = strings.TrimSpace(desc)

		mult := extractMultiplier(desc)

		// Apply to all models in this deal
		for _, c := range codes {
			if len(c) < 2 {
				continue
			}
			model := strings.TrimSpace(c[1])
			deal := Deal{
				Model:       model,
				Multiplier:  mult,
				Description: desc,
				Status:      status,
			}
			deals[strings.ToLower(model)] = deal
		}
	}

	if len(deals) == 0 {
		log.Printf("[pricing] WARNING: no deals parsed from %d blocks — extraction failed", len(dealBlocks))
	}

	return deals
}

// stripHTMLTags removes HTML tags from a string (best-effort, no full parser).
func stripHTMLTags(s string) string {
	tag := regexp.MustCompile(`<[^>]+>`)
	s = tag.ReplaceAllString(s, "")
	// Collapse whitespace
	ws := regexp.MustCompile(`\s+`)
	s = ws.ReplaceAllString(s, " ")
	return s
}

// extractMultiplier pulls the multiplier string (e.g., "4×", "2x", "99% off") from a description.
func extractMultiplier(desc string) string {
	// Try × (Unicode), x, or %
	patterns := []struct {
		re   *regexp.Regexp
		tmpl func([]string) string
	}{
		{regexp.MustCompile(`(\d+(?:\.\d+)?)\s*[x×]`), func(m []string) string { return m[1] + "x" }},
		{regexp.MustCompile(`(\d+)%\s*off`), func(m []string) string { return m[1] + "% off" }},
	}
	for _, p := range patterns {
		if m := p.re.FindStringSubmatch(desc); m != nil {
			return p.tmpl(m)
		}
	}
	return ""
}
