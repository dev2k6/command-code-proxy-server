package proxy

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dev2k6/command-code-proxy-server/internal/api"
)

// Convert OpenAI messages to CommandCode format
func ConvertMessages(openAIMsgs []api.OpenAIMessage) []api.CCMessage {
	var ccMsgs []api.CCMessage
	toolNames := map[string]string{}

	for _, m := range openAIMsgs {
		for _, tc := range m.ToolCalls {
			if tc.ID != "" && tc.Function.Name != "" {
				toolNames[tc.ID] = tc.Function.Name
			}
		}

		if m.Role == "tool" {
			toolName := m.Name
			if toolName == "" {
				toolName = toolNames[m.ToolCallID]
			}
			if toolName == "" {
				toolName = "unknown"
			}
			contentStr := contentToString(m.Content)
			outputType := "text"
			if strings.HasPrefix(contentStr, "Error:") {
				outputType = "error-text"
			}
			ccMsgs = append(ccMsgs, api.CCMessage{
				Role: "tool",
				Content: []api.CCContentPart{{
					Type:       "tool-result",
					ToolCallID: strPtr(m.ToolCallID),
					ToolName:   strPtr(toolName),
					Output: &api.CCToolOutput{
						Type:  outputType,
						Value: contentStr,
					},
				}},
			})
			continue
		}

		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			contentParts := parseContent(m.Content, toolNames)
			addedTools := map[string]bool{}
			for _, part := range contentParts {
				if part.Type == "tool-call" && part.ToolCallID != nil {
					addedTools[*part.ToolCallID] = true
				}
			}
			for _, tc := range m.ToolCalls {
				if addedTools[tc.ID] {
					continue
				}
				contentParts = append(contentParts, api.CCContentPart{
					Type:       "tool-call",
					ToolCallID: strPtr(tc.ID),
					ToolName:   strPtr(tc.Function.Name),
					Input:      parseToolInput(tc.Function.Arguments),
				})
				addedTools[tc.ID] = true
			}
			ccMsgs = append(ccMsgs, api.CCMessage{Role: m.Role, Content: contentParts})
			continue
		}

		// The upstream API only accepts "user"|"assistant"|"tool". Fall back
		// unknown roles (e.g. an unextracted "developer", or custom roles from
		// third-party clients) to "user" so the request is never rejected.
		role := m.Role
		if role != "user" && role != "assistant" && role != "tool" {
			role = "user"
		}
		ccMsgs = append(ccMsgs, api.CCMessage{Role: role, Content: parseContent(m.Content, toolNames)})
	}
	return ccMsgs
}

func ConvertTools(openAITools []any) []any {
	if len(openAITools) == 0 {
		return []any{}
	}

	tools := make([]any, 0, len(openAITools))
	for _, tool := range openAITools {
		toolMap, ok := tool.(map[string]any)
		if !ok {
			continue
		}

		toolType, _ := toolMap["type"].(string)
		if toolType != "function" {
			tools = append(tools, toolMap)
			continue
		}

		fn, ok := toolMap["function"].(map[string]any)
		if !ok {
			continue
		}

		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}

		inputSchema, ok := fn["parameters"].(map[string]any)
		if !ok || inputSchema == nil {
			inputSchema = map[string]any{"type": "object", "properties": map[string]any{}}
		}

		ccTool := map[string]any{"name": name, "input_schema": inputSchema}
		if description, ok := fn["description"].(string); ok && description != "" {
			ccTool["description"] = description
		}
		tools = append(tools, ccTool)
	}

	return tools
}

func parseToolInput(arguments string) any {
	if arguments == "" {
		return map[string]any{}
	}
	var input any
	if err := json.Unmarshal([]byte(arguments), &input); err != nil {
		return map[string]any{"arguments": arguments}
	}
	return input
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func contentToString(content interface{}) string {
	switch v := content.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, part := range v {
			if partMap, ok := part.(map[string]any); ok {
				text := contentPartToString(partMap)
				if text != "" {
					b.WriteString(text)
				}
			}
		}
		return b.String()
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(data)
	}
}

func contentPartToString(content any) string {
	switch v := content.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				b.WriteString(contentPartToString(m))
			}
		}
		return b.String()
	case map[string]any:
		for _, key := range []string{"text", "content", "output_text", "input_text", "refusal", "thinking", "redacted_thinking"} {
			if text, ok := v[key].(string); ok {
				return text
			}
		}
		if imgURL, ok := v["image_url"].(map[string]any); ok {
			if url, ok := imgURL["url"].(string); ok {
				return "[Image URL: " + url + "]"
			}
		}
		if url, ok := v["image_url"].(string); ok {
			return "[Image URL: " + url + "]"
		}
		data, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(data)
	default:
		return fmt.Sprint(v)
	}
}

func parseContent(content interface{}, toolNames map[string]string) []api.CCContentPart {
	switch v := content.(type) {
	case nil:
		return nil
	case string:
		if v == "" {
			return nil
		}
		return []api.CCContentPart{{Type: "text", Text: strPtr(v)}}
	case []any:
		var parts []api.CCContentPart
		for _, part := range v {
			partMap, ok := part.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := partMap["type"].(string)
			switch typ {
			case "text", "input_text", "output_text", "refusal", "thinking", "redacted_thinking", "reasoning", "document", "search_result":
				if text := contentPartToString(partMap); text != "" {
					parts = append(parts, api.CCContentPart{Type: "text", Text: strPtr(text)})
				}
			case "image_url", "input_image", "image":
				if text := contentPartToString(partMap); text != "" {
					parts = append(parts, api.CCContentPart{Type: "text", Text: strPtr(text)})
				}
			case "tool_use", "tool-call":
				id, _ := partMap["id"].(string)
				if id == "" {
					id, _ = partMap["toolCallId"].(string)
				}
				if id == "" {
					id, _ = partMap["tool_use_id"].(string)
				}
				name, _ := partMap["name"].(string)
				if name == "" {
					name, _ = partMap["toolName"].(string)
				}
				if id != "" && name != "" {
					toolNames[id] = name
				}
				input := partMap["input"]
				if input == nil {
					input = partMap["arguments"]
				}
				parts = append(parts, api.CCContentPart{
					Type:       "tool-call",
					ToolCallID: strPtr(id),
					ToolName:   strPtr(name),
					Input:      input,
				})
			case "tool_result", "tool-result":
				toolID, _ := partMap["tool_use_id"].(string)
				if toolID == "" {
					toolID, _ = partMap["toolCallId"].(string)
				}
				toolName, _ := partMap["toolName"].(string)
				if toolName == "" {
					toolName = toolNames[toolID]
				}
				if toolName == "" {
					toolName = "unknown"
				}
				contentVal := contentPartToString(partMap["content"])
				if contentVal == "" {
					contentVal = contentPartToString(partMap["output"])
				}
				outputType := "text"
				if strings.HasPrefix(contentVal, "Error:") {
					outputType = "error-text"
				}
				parts = append(parts, api.CCContentPart{
					Type:       "tool-result",
					ToolCallID: strPtr(toolID),
					ToolName:   strPtr(toolName),
					Output: &api.CCToolOutput{
						Type:  outputType,
						Value: contentVal,
					},
				})
			}
		}
		return parts
	default:
		return []api.CCContentPart{{Type: "text", Text: strPtr(contentToString(v))}}
	}
}

// Extract system message and remaining messages
func ExtractSystem(msgs []api.OpenAIMessage) (string, []api.OpenAIMessage) {
	var system strings.Builder
	var rest []api.OpenAIMessage
	for _, m := range msgs {
		// "developer" is the newer name for "system" (OpenAI/DeepSeek clients
		// such as DeepSeek Harness send system prompts with this role). It must
		// be extracted like "system", otherwise it is forwarded verbatim and
		// CommandCode rejects the request (messages[].role only accepts
		// "user"|"assistant"|"tool").
		if m.Role == "system" || m.Role == "developer" {
			if system.Len() > 0 {
				system.WriteString("\n")
			}
			system.WriteString(contentToString(m.Content))
		} else {
			rest = append(rest, m)
		}
	}
	return system.String(), rest
}
