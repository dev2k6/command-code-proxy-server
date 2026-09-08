package proxy

import "strings"

// MapModel maps a client model name to a CommandCode model id.
// It first resolves against the dynamically fetched model catalog (supports
// exact ids, short names, and punctuation-insensitive matches). If the
// catalog is not loaded yet or the name is not found, it falls back to the
// static alias table, and finally passes unknown names through unchanged.
func MapModel(name string) string {
	if id := catalog.resolve(name); id != "" {
		return id
	}
	return staticMapModel(name)
}

// staticMapModel handles well-known short aliases while the catalog is still
// loading (or as a fallback for names the catalog does not contain).
func staticMapModel(name string) string {
	switch strings.ToLower(name) {
	case "deepseek-v4-pro", "deepseek-v4", "deepseek-pro":
		return "deepseek/deepseek-v4-pro"
	case "deepseek-v4-flash", "deepseek-flash":
		return "deepseek/deepseek-v4-flash"
	case "minimax-m2.7", "minimax2.7":
		return "MiniMaxAI/MiniMax-M2.7"
	case "minimax-m2.5", "minimax2.5", "minimax":
		return "MiniMaxAI/MiniMax-M2.5"
	case "MiniMaxAI/MiniMax-M3", "minimax-m3", "minimax3":
		return "MiniMaxAI/MiniMax-M3"
	case "glm-5.1":
		return "zai-org/GLM-5.1"
	case "glm-5":
		return "zai-org/GLM-5"
	case "kimi-k2.6", "kimi2.6":
		return "moonshotai/Kimi-K2.6"
	case "kimi-k2.5", "kimi2.5":
		return "moonshotai/Kimi-K2.5"
	case "qwen-3.6-max-preview", "qwen3.6-max":
		return "Qwen/Qwen3.6-Max-Preview"
	case "qwen-3.6-plus", "qwen3.6-plus", "qwen3.6":
		return "Qwen/Qwen3.6-Plus"
	case "step-3.5-flash", "step3.5":
		return "stepfun/Step-3.5-Flash"
	case "step-3.7-flash", "step3.7", "stepfun/Step-3.7-Flash":
		return "stepfun/Step-3.7-Flash"
	case "gemini-3.1-flash-lite", "gemini-flash-lite":
		return "google/gemini-3.1-flash-lite"
	case "qwen-3.7-max-free", "qwen3.7-max-free", "Qwen/Qwen3.7-Max-Free":
		return "Qwen/Qwen3.7-Max-Free"
	case "qwen-3.7-max", "qwen3.7-max", "Qwen/Qwen3.7-Max":
		return "Qwen/Qwen3.7-Max"
	case "mimo-v2.5-pro", "mimo-pro", "xiaomi/mimo-v2.5-pro":
		return "xiaomi/mimo-v2.5-pro"
	case "mimo-v2.5", "mimo", "xiaomi/mimo-v2.5":
		return "xiaomi/mimo-v2.5"
	default:
		return name // pass through as-is
	}
}
