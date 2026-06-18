package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/dev2k6/command-code-proxy-server/internal/api"
	"github.com/dev2k6/command-code-proxy-server/internal/version"
	"github.com/google/uuid"
)

const defaultBaseURL = "https://api.commandcode.ai"
const defaultTimeout = 300 * time.Second
const debugLogLimit = 20000

func truncateLog(s string) string {
	if len(s) <= debugLogLimit {
		return s
	}
	return s[:debugLogLimit] + fmt.Sprintf("... [truncated %d bytes]", len(s)-debugLogLimit)
}

func (p *Proxy) debugf(format string, args ...any) {
	if p.Debug {
		log.Printf(format, args...)
	}
}

func (p *Proxy) writeOpenAIError(w http.ResponseWriter, status int, message, errType string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(api.OpenAIErrorResponse{Error: api.OpenAIError{
		Message: message,
		Type:    errType,
		Param:   nil,
		Code:    nil,
	}})
}

func normalizeFinishReason(reason string) string {
	switch reason {
	case "tool_calls", "tool-calls":
		return "tool_calls"
	case "length", "max_tokens":
		return "length"
	case "content_filter", "content-filter":
		return "content_filter"
	default:
		return "stop"
	}
}

// Proxy struct
type Proxy struct {
	APIKey     string
	BaseURL    string
	Client     *http.Client
	Debug      bool
	Models     *ModelCache
	Pricing    *PricingCache
	ModelInfo  *ModelInfoCache
}

// NewProxy creates a new proxy instance
func NewProxy(apiKey string) *Proxy {
	pricing := NewPricingCache()
	modelInfo := NewModelInfoCache()
	p := &Proxy{
		APIKey:    apiKey,
		BaseURL:   defaultBaseURL,
		Client:    &http.Client{Timeout: defaultTimeout},
		Models:    NewModelCache(pricing, modelInfo),
		Pricing:   pricing,
		ModelInfo: modelInfo,
	}
	// Kick off background refreshes — don't block startup
	pricing.StartBackgroundRefresh()
	modelInfo.StartBackgroundRefresh()
	if apiKey != "" {
		p.Models.StartBackgroundRefresh(apiKey)
	}
	return p
}

// BuildRequest builds the CommandCode request body
func (p *Proxy) BuildRequest(openAIReq api.OpenAIChatRequest) (api.CCRequestBody, error) {
	model := MapModel(openAIReq.Model)
	system, msgs := ExtractSystem(openAIReq.Messages)
	ccMessages := ConvertMessages(msgs)

	temperature := 0.3
	maxTokens := 64000
	if openAIReq.Temperature != nil {
		temperature = *openAIReq.Temperature
	}
	if openAIReq.MaxTokens != nil {
		maxTokens = *openAIReq.MaxTokens
	}
	if openAIReq.MaxCompletionTokens != nil {
		maxTokens = *openAIReq.MaxCompletionTokens
	}

	tools := ConvertTools(openAIReq.Tools)

	// Map reasoning effort
	var reasoning string
	if openAIReq.Reasoning != nil {
		reasoning = string(*openAIReq.Reasoning)
	}

	ccBody := api.CCRequestBody{
		Config: api.CCConfig{
			WorkingDir:    ".",
			Date:          time.Now().Format("2006-01-02"),
			Environment:   "cli",
			Structure:     []string{},
			IsGitRepo:     false,
			CurrentBranch: "",
			MainBranch:    "main",
			GitStatus:     "",
			RecentCommits: []string{},
		},
		Memory: "",
		Taste:  "",
		Skills: "",
		Params: api.CCChatParams{
			Model:       model,
			Messages:    ccMessages,
			Tools:       tools,
			System:      system,
			MaxTokens:   maxTokens,
			Temperature: temperature,
			Stream:      true,
			Reasoning:   reasoning,
		},
		ThreadID: uuid.New().String(),
	}

	return ccBody, nil
}

// CreateUpstreamRequest creates a new HTTP request to the CommandCode API
func (p *Proxy) CreateUpstreamRequest(ctx context.Context, ccBody api.CCRequestBody, apiKey string) (*http.Request, error) {
	reqJSON, err := json.Marshal(ccBody)
	if err != nil {
		return nil, fmt.Errorf("failed to build request: %w", err)
	}

	p.debugf("[DEBUG] CommandCode request body: %s", truncateLog(string(reqJSON)))

	ccReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.BaseURL+"/alpha/generate", bytes.NewReader(reqJSON))
	if err != nil {
		return nil, fmt.Errorf("failed to create upstream request: %w", err)
	}

	ccReq.Header.Set("Content-Type", "application/json")
	ccReq.Header.Set("Authorization", "Bearer "+apiKey)
	ccReq.Header.Set("x-command-code-version", version.GetCommandCodeVersion())
	ccReq.Header.Set("x-cli-environment", "production")
	ccReq.Header.Set("Accept", "text/event-stream")

	return ccReq, nil
}

// CallUpstream makes the request to CommandCode API
func (p *Proxy) CallUpstream(req *http.Request) (*http.Response, error) {
	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream error: %w", err)
	}
	return resp, nil
}

// HandleChatCompletions handles the /v1/chat/completions endpoint
func (p *Proxy) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		p.writeOpenAIError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request_error")
		return
	}

	// Get API key from client Authorization header or server default
	apiKey := r.Header.Get("Authorization")
	if apiKey != "" {
		apiKey = strings.TrimPrefix(apiKey, "Bearer ")
		apiKey = strings.TrimSpace(apiKey)
	} else if p.APIKey != "" {
		apiKey = p.APIKey
	} else {
		p.writeOpenAIError(w, http.StatusUnauthorized, "API key required. Set Authorization header.", "authentication_error")
		return
	}

	// Read request
	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.writeOpenAIError(w, http.StatusBadRequest, "Failed to read body", "invalid_request_error")
		return
	}

	p.debugf("[DEBUG] Client request body: %s", truncateLog(string(body)))

	var openAIReq api.OpenAIChatRequest
	if err := json.Unmarshal(body, &openAIReq); err != nil {
		p.writeOpenAIError(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON: %s", err.Error()), "invalid_request_error")
		return
	}

	if len(openAIReq.Messages) == 0 {
		p.writeOpenAIError(w, http.StatusBadRequest, "messages array is required", "invalid_request_error")
		return
	}

	// Build CommandCode request
	ccBody, err := p.BuildRequest(openAIReq)
	if err != nil {
		p.writeOpenAIError(w, http.StatusInternalServerError, "Failed to build request", "server_error")
		return
	}

	// Create upstream request
	ccReq, err := p.CreateUpstreamRequest(r.Context(), ccBody, apiKey)
	if err != nil {
		p.writeOpenAIError(w, http.StatusInternalServerError, "Failed to create upstream request", "server_error")
		return
	}

	// Call upstream
	ccResp, err := p.CallUpstream(ccReq)
	if err != nil {
		p.writeOpenAIError(w, http.StatusBadGateway, err.Error(), "api_error")
		return
	}
	defer ccResp.Body.Close()

	if ccResp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(ccResp.Body)
		message := fmt.Sprintf("Upstream error: %s", string(errBody))
		log.Printf("[ERROR] Upstream returned %d: %s", ccResp.StatusCode, string(errBody))
		status := http.StatusBadGateway
		if ccResp.StatusCode >= http.StatusBadRequest && ccResp.StatusCode < http.StatusInternalServerError {
			status = ccResp.StatusCode
		}
		p.writeOpenAIError(w, status, message, "api_error")
		return
	}

	requestID := "chatcmpl-" + uuid.New().String()[:29]
	created := time.Now().Unix()

	if openAIReq.Stream {
		p.StreamResponse(w, r, ccResp, requestID, ccBody.Params.Model, created)
	} else {
		p.NonStreamResponse(w, ccResp, requestID, ccBody.Params.Model, created)
	}
}

// StreamResponse handles streaming response from CommandCode to OpenAI SSE
func (p *Proxy) StreamResponse(w http.ResponseWriter, r *http.Request, ccResp *http.Response, requestID, model string, created int64) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		p.writeOpenAIError(w, http.StatusInternalServerError, "Streaming not supported", "server_error")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	scanner := bufio.NewScanner(ccResp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	sentRole := false
	toolCallIndex := 0
	toolCallIndexes := map[string]int{}

	for scanner.Scan() {
		select {
		case <-r.Context().Done():
			return
		default:
		}

		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		p.debugf("[DEBUG] CommandCode stream line: %s", truncateLog(line))

		var event api.CCStreamEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}

		switch event.Type {
		case "text-delta":
			delta := api.OpenAIDelta{Content: event.Text}
			if !sentRole {
				delta.Role = "assistant"
				sentRole = true
			}
			p.WriteSSE(w, flusher, api.OpenAIChatResponse{
				ID:      requestID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []api.OpenAIChoice{{Index: 0, Delta: &delta}},
			})

		case "tool-use":
			toolCalls := []api.OpenAIDeltaToolCall{{
				Index:    toolCallIndex,
				ID:       event.ToolCallID,
				Type:     "function",
				Function: &api.OpenAIDeltaFunction{Name: event.ToolName},
			}}
			delta := api.OpenAIDelta{ToolCalls: toolCalls}
			if !sentRole {
				delta.Role = "assistant"
				sentRole = true
			}
			p.WriteSSE(w, flusher, api.OpenAIChatResponse{
				ID:      requestID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []api.OpenAIChoice{{Index: 0, Delta: &delta}},
			})
			toolCallIndex++

		case "tool-delta":
			toolCalls := []api.OpenAIDeltaToolCall{{
				Index:    toolCallIndex - 1,
				Function: &api.OpenAIDeltaFunction{Arguments: event.Text},
			}}
			p.WriteSSE(w, flusher, api.OpenAIChatResponse{
				ID:      requestID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []api.OpenAIChoice{{Index: 0, Delta: &api.OpenAIDelta{ToolCalls: toolCalls}}},
			})

		case "tool-input-start":
			if _, ok := toolCallIndexes[event.ID]; !ok {
				toolCallIndexes[event.ID] = toolCallIndex
				toolCallIndex++
			}
			delta := api.OpenAIDelta{ToolCalls: []api.OpenAIDeltaToolCall{{
				Index: toolCallIndexes[event.ID],
				ID:    event.ID,
				Type:  "function",
				Function: &api.OpenAIDeltaFunction{
					Name: event.ToolName,
				},
			}}}
			if !sentRole {
				delta.Role = "assistant"
				sentRole = true
			}
			p.WriteSSE(w, flusher, api.OpenAIChatResponse{
				ID:      requestID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []api.OpenAIChoice{{Index: 0, Delta: &delta}},
			})

		case "tool-input-delta":
			idx, ok := toolCallIndexes[event.ID]
			if !ok {
				idx = toolCallIndex
				toolCallIndexes[event.ID] = idx
				toolCallIndex++
			}
			p.WriteSSE(w, flusher, api.OpenAIChatResponse{
				ID:      requestID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []api.OpenAIChoice{{Index: 0, Delta: &api.OpenAIDelta{ToolCalls: []api.OpenAIDeltaToolCall{{
					Index:    idx,
					Function: &api.OpenAIDeltaFunction{Arguments: event.Delta},
				}}}}},
			})

		case "tool-call":
			if _, alreadyStreamed := toolCallIndexes[event.ToolCallID]; alreadyStreamed {
				continue
			}
			idx := toolCallIndex
			toolCallIndexes[event.ToolCallID] = idx
			toolCallIndex++
			args := ""
			if event.Input != nil {
				if data, err := json.Marshal(event.Input); err == nil {
					args = string(data)
				}
			}
			delta := api.OpenAIDelta{ToolCalls: []api.OpenAIDeltaToolCall{{
				Index: idx,
				ID:    event.ToolCallID,
				Type:  "function",
				Function: &api.OpenAIDeltaFunction{
					Name:      event.ToolName,
					Arguments: args,
				},
			}}}
			if !sentRole {
				delta.Role = "assistant"
				sentRole = true
			}
			p.WriteSSE(w, flusher, api.OpenAIChatResponse{
				ID:      requestID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []api.OpenAIChoice{{Index: 0, Delta: &delta}},
			})

		case "finish":
			reason := normalizeFinishReason(event.FinishReason)
			p.WriteSSE(w, flusher, api.OpenAIChatResponse{
				ID:      requestID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []api.OpenAIChoice{{
					Index:        0,
					Delta:        &api.OpenAIDelta{},
					FinishReason: &reason,
				}},
			})
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()

		case "error":
			log.Printf("[ERROR] Stream error: %v", event.Error)
		}
	}

	if err := scanner.Err(); err != nil && err != io.EOF {
		log.Printf("[ERROR] Scanner error: %v", err)
	}
}

// WriteSSE writes a Server-Sent Event
func (p *Proxy) WriteSSE(w io.Writer, flusher http.Flusher, resp api.OpenAIChatResponse) {
	data, _ := json.Marshal(resp)
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()
}

// NonStreamResponse handles non-streaming response
func (p *Proxy) NonStreamResponse(w http.ResponseWriter, ccResp *http.Response, requestID, model string, created int64) {
	scanner := bufio.NewScanner(ccResp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	var content strings.Builder
	var inputTokens, outputTokens int
	var hasToolCalls bool
	var toolCalls []api.ToolCall
	toolCallByID := map[string]int{}
	toolInputBuffers := map[string]*strings.Builder{}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		p.debugf("[DEBUG] CommandCode stream line: %s", truncateLog(line))

		var event api.CCStreamEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}

		switch event.Type {
		case "text-delta":
			content.WriteString(event.Text)
		case "tool-use":
			hasToolCalls = true
			toolCallByID[event.ToolCallID] = len(toolCalls)
			toolCalls = append(toolCalls, api.ToolCall{
				ID:   event.ToolCallID,
				Type: "function",
				Function: api.FunctionCall{
					Name:      event.ToolName,
					Arguments: "",
				},
			})
		case "tool-delta":
			if len(toolCalls) > 0 {
				toolCalls[len(toolCalls)-1].Function.Arguments += event.Text
			}
		case "tool-input-start":
			hasToolCalls = true
			toolCallByID[event.ID] = len(toolCalls)
			toolInputBuffers[event.ID] = &strings.Builder{}
			toolCalls = append(toolCalls, api.ToolCall{
				ID:   event.ID,
				Type: "function",
				Function: api.FunctionCall{
					Name:      event.ToolName,
					Arguments: "",
				},
			})
		case "tool-input-delta":
			if b := toolInputBuffers[event.ID]; b != nil {
				b.WriteString(event.Delta)
			}
			if idx, ok := toolCallByID[event.ID]; ok {
				toolCalls[idx].Function.Arguments += event.Delta
			}
		case "tool-call":
			hasToolCalls = true
			args := ""
			if event.Input != nil {
				if data, err := json.Marshal(event.Input); err == nil {
					args = string(data)
				}
			}
			if idx, ok := toolCallByID[event.ToolCallID]; ok {
				toolCalls[idx].Function.Name = event.ToolName
				if args != "" {
					toolCalls[idx].Function.Arguments = args
				}
			} else {
				toolCallByID[event.ToolCallID] = len(toolCalls)
				toolCalls = append(toolCalls, api.ToolCall{
					ID:   event.ToolCallID,
					Type: "function",
					Function: api.FunctionCall{
						Name:      event.ToolName,
						Arguments: args,
					},
				})
			}
		case "finish":
			if event.TotalUsage != nil {
				inputTokens = event.TotalUsage.InputTokens
				outputTokens = event.TotalUsage.OutputTokens
			}
		case "error":
			log.Printf("[ERROR] Stream error: %v", event.Error)
		}
	}

	msg := &api.OpenAIMessage{
		Role:    "assistant",
		Content: content.String(),
	}
	finishReason := "stop"
	if hasToolCalls {
		msg.Content = nil
		msg.ToolCalls = toolCalls
		finishReason = "tool_calls"
	}

	response := api.OpenAIChatResponse{
		ID:      requestID,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Choices: []api.OpenAIChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: &finishReason,
		}},
		Usage: &api.OpenAIUsage{
			PromptTokens:     inputTokens,
			CompletionTokens: outputTokens,
			TotalTokens:      inputTokens + outputTokens,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (p *Proxy) HandleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		p.writeOpenAIError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request_error")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.writeOpenAIError(w, http.StatusBadRequest, "Failed to read body", "invalid_request_error")
		return
	}

	p.debugf("[DEBUG] Client responses request body: %s", truncateLog(string(body)))

	var responsesReq api.OpenAIResponsesRequest
	if err := json.Unmarshal(body, &responsesReq); err != nil {
		p.writeOpenAIError(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON: %s", err.Error()), "invalid_request_error")
		return
	}

	chatReq := responsesToChatRequest(responsesReq)
	rewritten, err := json.Marshal(chatReq)
	if err != nil {
		p.writeOpenAIError(w, http.StatusInternalServerError, "Failed to build request", "server_error")
		return
	}

	r.Body = io.NopCloser(bytes.NewReader(rewritten))
	r.ContentLength = int64(len(rewritten))
	p.HandleChatCompletions(w, r)
}

func responsesToChatRequest(req api.OpenAIResponsesRequest) api.OpenAIChatRequest {
	messages := responsesInputToMessages(req.Input)
	if req.Instructions != nil {
		messages = append([]api.OpenAIMessage{{Role: "system", Content: req.Instructions}}, messages...)
	}

	maxTokens := req.MaxCompletionTokens
	if maxTokens == nil {
		maxTokens = req.MaxOutputTokens
	}
	if maxTokens == nil {
		maxTokens = req.MaxTokens
	}

	return api.OpenAIChatRequest{
		Model:               req.Model,
		Messages:            messages,
		Temperature:         req.Temperature,
		MaxTokens:           req.MaxTokens,
		MaxCompletionTokens: maxTokens,
		Stream:              req.Stream,
		Tools:               req.Tools,
		ToolChoice:          req.ToolChoice,
		ParallelToolCalls:   req.ParallelToolCalls,
		ResponseFormat:      req.ResponseFormat,
		Stop:                req.Stop,
		TopP:                req.TopP,
		User:                req.User,
	}
}

func responsesInputToMessages(input any) []api.OpenAIMessage {
	switch v := input.(type) {
	case nil:
		return nil
	case string:
		return []api.OpenAIMessage{{Role: "user", Content: v}}
	case []any:
		if messages := responseItemsToMessages(v); len(messages) > 0 {
			return messages
		}
		return []api.OpenAIMessage{{Role: "user", Content: v}}
	default:
		return []api.OpenAIMessage{{Role: "user", Content: v}}
	}
}

func responseItemsToMessages(items []any) []api.OpenAIMessage {
	messages := make([]api.OpenAIMessage, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if role == "" {
			role = "user"
		}
		content := m["content"]
		if content == nil {
			content = m["text"]
		}
		if content == nil {
			content = m["input"]
		}
		messages = append(messages, api.OpenAIMessage{Role: role, Content: content})
	}
	return messages
}

// HandleModels handles the /v1/models endpoint
func (p *Proxy) HandleModels(w http.ResponseWriter, r *http.Request) {
	// Use the dynamic cache; fall back to static list if cache is empty/unavailable.
	cached := p.Models.Get()

	// If cache returned the static fallback (empty cache triggers fallback in Get),
	// still try to refresh in the background so the next request gets fresh data.
	if p.Models.IsStale() && !p.Models.fetchingNow {
		go func() {
			if err := p.Models.Refresh(p.APIKey); err != nil {
				log.Printf("[models] background refresh failed: %v", err)
			}
		}()
	}

	models := api.OpenAIModelList{
		Object: "list",
		Data:   cached,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(models)
}

// getStaticModels returns the hardcoded fallback list of models. Used when
// the dynamic cache is empty and we haven't been able to fetch from upstream.
// Context lengths are looked up from contextmap.go so we have one source of truth.
func getStaticModels() []api.OpenAIModel {
	ids := []string{
		// MoonshotAI
		"moonshotai/Kimi-K2.7-Code", "moonshotai/Kimi-K2.7-Code-Highspeed",
		// ZhipuAI
		"zai-org/GLM-5.2", "zai-org/GLM-5.1", "zai-org/GLM-5",
		// MiniMaxAI
		"MiniMaxAI/MiniMax-M3",
		"MiniMaxAI/MiniMax-M2.7", "MiniMaxAI/MiniMax-M2.5",
		// DeepSeek
		"deepseek/deepseek-v4-pro", "deepseek/deepseek-v4-flash",
		// Qwen
		"Qwen/Qwen3.6-Max-Preview", "Qwen/Qwen3.6-Plus",
		"Qwen/Qwen3.7-Max", "Qwen/Qwen3.7-Plus",
		// StepFun
		"stepfun/Step-3.7-Flash", "stepfun/Step-3.5-Flash",
		// Xiaomi
		"xiaomi/mimo-v2.5-pro", "xiaomi/mimo-v2.5",
		// NVIDIA
		"nvidia/nemotron-3-ultra-550b-a55b",
		// Anthropic
		"claude-sonnet-4-6", "claude-fable-5",
		"claude-opus-4-8", "claude-opus-4-7", "claude-opus-4-6",
		"claude-haiku-4-5", "claude-haiku-4-5-20251001",
		// OpenAI
		"gpt-5.5", "gpt-5.4", "gpt-5.3-codex", "gpt-5.4-mini",
		// Google
		"google/gemini-3.5-flash", "google/gemini-3.1-flash-lite",
	}
	out := make([]api.OpenAIModel, 0, len(ids))
	for _, id := range ids {
		m := api.OpenAIModel{
			ID:            id,
			Object:        "model",
			Created:       0,
			OwnedBy:       inferOwner(id),
			ContextLength: ContextLengthFor(id),
		}
		out = append(out, m)
	}
	return out
}

// inferOwner extracts the provider name from a model ID like "anthropic/claude-...".
func inferOwner(id string) string {
	for i, c := range id {
		if c == '/' {
			return id[:i]
		}
	}
	return "unknown"
}
