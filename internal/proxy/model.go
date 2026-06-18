package proxy

import "strings"

// Map model name if client sends short name
func MapModel(name string) string {
	switch strings.ToLower(name) {
	case "deepseek-v4-pro", "deepseek-v4", "deepseek-pro":
		return "deepseek/deepseek-v4-pro"
	case "deepseek-v4-flash", "deepseek-flash":
		return "deepseek/deepseek-v4-flash"
	case "minimax-m2.7", "minimax2.7":
		return "MiniMaxAI/MiniMax-M2.7"
	case "minimax-m2.5", "minimax2.5", "minimax":
		return "MiniMaxAI/MiniMax-M2.5"
	case "minimax-m3", "minimax3":
		return "MiniMaxAI/MiniMax-M3"
	case "minimax-m3-promo", "minimax3-promo":
		return "MiniMaxAI/MiniMax-M3-Promo"
	case "glm-5.2", "glm-52":
		return "zai-org/GLM-5.2"
	case "glm-5.1":
		return "zai-org/GLM-5.1"
	case "glm-5":
		return "zai-org/GLM-5"
	case "kimi-k2.6", "kimi2.6":
		return "moonshotai/Kimi-K2.6"
	case "kimi-k2.5", "kimi2.5":
		return "moonshotai/Kimi-K2.5"
	case "kimi-k2.7-code", "kimi2.7-code":
		return "moonshotai/Kimi-K2.7-Code"
	case "kimi-k2.7-code-highspeed", "kimi2.7-code-highspeed":
		return "moonshotai/Kimi-K2.7-Code-Highspeed"
	case "qwen-3.6-max-preview", "qwen3.6-max":
		return "Qwen/Qwen3.6-Max-Preview"
	case "qwen-3.6-plus", "qwen3.6-plus", "qwen3.6":
		return "Qwen/Qwen3.6-Plus"
	case "qwen-3.7-max", "qwen3.7-max":
		return "Qwen/Qwen3.7-Max"
	case "qwen-3.7-plus", "qwen3.7-plus", "qwen3.7":
		return "Qwen/Qwen3.7-Plus"
	case "step-3.5-flash", "step3.5":
		return "stepfun/Step-3.5-Flash"
	case "step-3.7-flash", "step3.7":
		return "stepfun/Step-3.7-Flash"
	case "mimo-v2.5-pro", "mimo2.5-pro":
		return "xiaomi/mimo-v2.5-pro"
	case "mimo-v2.5", "mimo2.5", "mimo":
		return "xiaomi/mimo-v2.5"
	case "nemotron-3-ultra", "nemotron":
		return "nvidia/nemotron-3-ultra-550b-a55b"
	case "claude-sonnet-4-6", "sonnet-4-6", "sonnet":
		return "claude-sonnet-4-6"
	case "claude-fable-5", "fable-5", "fable":
		return "claude-fable-5"
	case "claude-opus-4-8", "opus-4-8", "opus":
		return "claude-opus-4-8"
	case "claude-opus-4-7", "opus-4-7":
		return "claude-opus-4-7"
	case "claude-opus-4-6", "opus-4-6":
		return "claude-opus-4-6"
	case "claude-haiku-4-5", "haiku-4-5", "haiku":
		return "claude-haiku-4-5"
	case "gpt-5.5":
		return "gpt-5.5"
	case "gpt-5.4":
		return "gpt-5.4"
	case "gpt-5.3-codex", "codex":
		return "gpt-5.3-codex"
	case "gpt-5.4-mini", "gpt-mini":
		return "gpt-5.4-mini"
	case "gemini-3.1-flash-lite", "gemini-flash-lite":
		return "google/gemini-3.1-flash-lite"
	case "gemini-3.5-flash", "gemini-flash":
		return "google/gemini-3.5-flash"
	default:
		return name // pass through as-is
	}
}
