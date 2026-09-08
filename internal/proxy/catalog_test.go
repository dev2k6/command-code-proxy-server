package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

const testCatalogHTML = `<html><body>
<table>
<tr><td><a href="https://commandcode.ai/models/deepseek-v4-pro" class="text-burple-foreground"><code>deepseek/deepseek-v4-pro</code></a></td><td><a href="https://commandcode.ai/models/deepseek-v4-pro">DeepSeek V4 Pro</a></td></tr>
<tr><td><a href="https://commandcode.ai/models/deepseek-v4-flash" class="text-burple-foreground"><code>deepseek/deepseek-v4-flash</code></a></td><td><a href="https://commandcode.ai/models/deepseek-v4-flash">DeepSeek V4 Flash</a></td></tr>
<tr><td><a href="https://commandcode.ai/models/claude-sonnet-5" class="text-burple-foreground"><code>claude-sonnet-5</code></a></td><td><a href="https://commandcode.ai/models/claude-sonnet-5">Claude Sonnet 5</a></td></tr>
<tr><td><a href="https://commandcode.ai/models/qwen3-8-max" class="text-burple-foreground"><code>Qwen/Qwen3.8-Max</code></a></td><td><a href="https://commandcode.ai/models/qwen3-8-max">Qwen 3.8 Max</a></td></tr>
<code>deepseek/deepseek-v4-flash</code>
</table>
</body></html>`

func TestParseModelIDs(t *testing.T) {
	got := parseModelIDs(testCatalogHTML)
	want := []string{"deepseek/deepseek-v4-pro", "deepseek/deepseek-v4-flash", "claude-sonnet-5", "Qwen/Qwen3.8-Max"}
	if len(got) != len(want) {
		t.Fatalf("parseModelIDs returned %d ids %v, want %d %v", len(got), got, len(want), want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("id[%d] = %q, want %q", i, got[i], w)
		}
	}
}

func TestParseModelIDsNonTableCodeIgnored(t *testing.T) {
	got := parseModelIDs(`<p>Run <code>cmd --model deepseek/deepseek-v4-flash</code> and <code>/model</code></p>`)
	if len(got) != 0 {
		t.Errorf("expected no ids from prose code blocks, got %v", got)
	}
}

func TestCatalogResolve(t *testing.T) {
	c := &modelCatalog{}
	c.update([]string{"deepseek/deepseek-v4-pro", "deepseek/deepseek-v4-flash-vision-exp", "claude-sonnet-5", "Qwen/Qwen3.8-Max"})

	cases := []struct {
		in, want string
	}{
		// full ids, case-insensitive
		{"deepseek/deepseek-v4-pro", "deepseek/deepseek-v4-pro"},
		{"DeepSeek/DeepSeek-V4-Pro", "deepseek/deepseek-v4-pro"},
		// short names (name after "/")
		{"deepseek-v4-pro", "deepseek/deepseek-v4-pro"},
		{"claude-sonnet-5", "claude-sonnet-5"},
		// punctuation-insensitive
		{"deepseekv4pro", "deepseek/deepseek-v4-pro"},
		{"qwen38max", "Qwen/Qwen3.8-Max"},
		{"qwen-3.8-max", "Qwen/Qwen3.8-Max"},
		// unknown
		{"some/unknown-model", ""},
		{"", ""},
	}
	for _, cse := range cases {
		got := c.resolve(cse.in)
		if got != cse.want {
			t.Errorf("resolve(%q) = %q, want %q", cse.in, got, cse.want)
		}
	}
}

func TestMapModelUsesCatalog(t *testing.T) {
	// Saved fields to restore the singleton afterwards.
	catalog.mu.Lock()
	savedIDs, savedByID, savedByShort, savedByNorm, savedLoaded := catalog.ids, catalog.byID, catalog.byShort, catalog.byNorm, catalog.loaded
	catalog.mu.Unlock()
	defer func() {
		catalog.mu.Lock()
		catalog.ids, catalog.byID, catalog.byShort, catalog.byNorm, catalog.loaded = savedIDs, savedByID, savedByShort, savedByNorm, savedLoaded
		catalog.mu.Unlock()
	}()

	// Catalog not loaded -> static fallback
	old := catalog
	old.mu.Lock()
	old.loaded = false
	old.mu.Unlock()

	if got := MapModel("deepseek-v4-flash"); got != "deepseek/deepseek-v4-flash" {
		t.Errorf("static fallback: MapModel = %q", got)
	}
	// Catalog loaded -> dynamic resolution wins over static table
	catalog.update([]string{"deepseek/deepseek-v4-flash-fast", "moonshotai/Kimi-K3", "google/gemini-3.8-flash"})
	if got := MapModel("deepseek-v4-flash"); got != "deepseek/deepseek-v4-flash" {
		t.Errorf("catalog should shadow static: MapModel = %q", got)
	}
	if got := MapModel("deepseek-v4-flash-fast"); got != "deepseek/deepseek-v4-flash-fast" {
		t.Errorf("catalog new model: MapModel = %q", got)
	}
	if got := MapModel("kimi-k3"); got != "moonshotai/Kimi-K3" {
		t.Errorf("short name from catalog: MapModel = %q", got)
	}
	if got := MapModel("gemini38flash"); got != "google/gemini-3.8-flash" {
		t.Errorf("punctuation-insensitive: MapModel = %q", got)
	}
	// Model only in static table still resolves
	if got := MapModel("glm-5.1"); got != "zai-org/GLM-5.1" {
		t.Errorf("static-only alias: MapModel = %q", got)
	}
}

func TestFetchModelIDsFrom(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(testCatalogHTML))
	}))
	defer srv.Close()

	ids, err := fetchModelIDsFrom(srv.URL)
	if err != nil {
		t.Fatalf("fetchModelIDsFrom failed: %v", err)
	}
	if len(ids) != 4 {
		t.Fatalf("expected 4 ids, got %d: %v", len(ids), ids)
	}
}

func TestFetchModelIDsFromError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := fetchModelIDsFrom(srv.URL); err == nil {
		t.Fatal("expected error for non-200 response")
	}
}

func TestCatalogModelsBuildsList(t *testing.T) {
	catalog.update([]string{"deepseek/deepseek-v4-pro", "claude-sonnet-5"})
	models := catalogModels()
	if len(models) != 2 {
		t.Fatalf("catalogModels returned %d models, want 2", len(models))
	}
	if models[0].ID != "claude-sonnet-5" && models[0].ID != "deepseek/deepseek-v4-pro" {
		t.Errorf("unexpected first model %q", models[0].ID)
	}
	for _, m := range models {
		if m.Object != "model" {
			t.Errorf("model %q object = %q", m.ID, m.Object)
		}
	}
	if models[0].OwnedBy == "" {
		t.Errorf("owned_by missing for %q", models[0].ID)
	}
}

func TestOwnedByFor(t *testing.T) {
	cases := []struct{ in, want string }{
		{"deepseek/deepseek-v4-pro", "deepseek"},
		{"Qwen/Qwen3.8-Max", "Qwen"},
		{"claude-sonnet-5", "commandcode"},
	}
	for _, c := range cases {
		if got := ownedByFor(c.in); got != c.want {
			t.Errorf("ownedByFor(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
