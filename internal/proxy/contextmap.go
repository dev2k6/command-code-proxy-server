package proxy

// contextLengthMap maps model IDs to their context window size in tokens.
// Command Code's /v1/models endpoint does not expose context windows, so we
// maintain a local override. Unknown models fall back to 128K (safe default).
//
// Sources:
//   - Upstream provider documentation (Anthropic, OpenAI, Google, etc.)
//   - Command Code CLI docs (https://commandcode.ai/docs/reference/cli/models)
//   - Model release notes
var contextLengthMap = map[string]int{
	// MoonshotAI Kimi
	"moonshotai/Kimi-K2.7-Code":           262144,
	"moonshotai/Kimi-K2.7-Code-Highspeed": 262144,
	"moonshotai/Kimi-K2.6":                262144,
	"moonshotai/Kimi-K2.5":                262144,

	// ZhipuAI GLM
	"zai-org/GLM-5.2": 1048576, // 1M
	"zai-org/GLM-5.1": 202752,
	"zai-org/GLM-5":   202752,

	// MiniMax
	"MiniMaxAI/MiniMax-M3":        1000000, // 1M
	"MiniMaxAI/MiniMax-M3-Promo":  1000000, // 1M
	"MiniMaxAI/MiniMax-M2.7":      204800,
	"MiniMaxAI/MiniMax-M2.5":      204800,

	// DeepSeek
	"deepseek/deepseek-v4-pro":   1000000, // 1M
	"deepseek/deepseek-v4-flash": 1000000, // 1M

	// Qwen
	"Qwen/Qwen3.6-Max-Preview": 1048576, // 1M
	"Qwen/Qwen3.6-Plus":        1048576, // 1M
	"Qwen/Qwen3.7-Max":         1048576, // 1M
	"Qwen/Qwen3.7-Plus":        1048576, // 1M

	// StepFun
	"stepfun/Step-3.7-Flash": 262144,
	"stepfun/Step-3.5-Flash": 262144,

	// Xiaomi MiMo
	"xiaomi/mimo-v2.5-pro": 1048576, // 1M
	"xiaomi/mimo-v2.5":     1048576, // 1M

	// NVIDIA Nemotron
	"nvidia/nemotron-3-ultra-550b-a55b": 131072,

	// Anthropic Claude
	"claude-sonnet-4-6":              1000000, // 1M
	"claude-fable-5":                 1000000, // 1M
	"claude-opus-4-8":                1000000, // 1M
	"claude-opus-4-7":                1000000, // 1M
	"claude-opus-4-6":                200000,
	"claude-haiku-4-5":               200000,
	"claude-haiku-4-5-20251001":      200000,

	// OpenAI GPT
	"gpt-5.5":         1050000, // ~1M with reasoning overhead
	"gpt-5.4":         1050000,
	"gpt-5.3-codex":   400000,
	"gpt-5.4-mini":    400000,

	// Google Gemini
	"google/gemini-3.5-flash":      1048576, // 1M
	"google/gemini-3.1-flash-lite": 1048576, // 1M
}

// defaultContextLength is returned for unknown models when we can't determine
// their context window. Conservative — better to over-estimate and trigger
// compression early than to under-estimate and blow the window.
const defaultContextLength = 131072 // 128K

// tasteOneModelID is Command Code's internal model that ships free with all plans.
// Not exposed via upstream /v1/models API; hardcoded here so it's discoverable
// through the proxy.
const tasteOneModelID = "taste-1"

// tasteOneContextLength is the context window for taste-1. Set to a reasonable
// coding-agent window — tune if Command Code publishes a different number.
const tasteOneContextLength = 262144 // 256K

// ContextLengthFor returns the context window for a model ID.
// Falls back to defaultContextLength if the model is not in the map.
func ContextLengthFor(modelID string) int {
	if n, ok := contextLengthMap[modelID]; ok {
		return n
	}
	// Try matching by suffix (model names can have provider prefixes we don't track)
	for knownID, n := range contextLengthMap {
		if modelMatches(modelID, knownID) {
			return n
		}
	}
	return defaultContextLength
}

// modelMatches does fuzzy matching — strips common prefixes and compares.
// e.g. "minimax/MiniMax-M3" matches "MiniMaxAI/MiniMax-M3"
func modelMatches(query, known string) bool {
	// Extract model name after the last "/"
	getSuffix := func(s string) string {
		for i := len(s) - 1; i >= 0; i-- {
			if s[i] == '/' {
				return s[i+1:]
			}
		}
		return s
	}
	return getSuffix(query) == getSuffix(known)
}
