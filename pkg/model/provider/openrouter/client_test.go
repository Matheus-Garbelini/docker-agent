package openrouter

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestNewClient_DefaultTokenKeyRequired(t *testing.T) {
	t.Parallel()

	_, err := NewClient(t.Context(), &latest.ModelConfig{
		Provider: "openrouter",
		Model:    "openai/gpt-5-mini",
	}, environment.NewNoEnvProvider())

	require.Error(t, err)
	assert.Contains(t, err.Error(), defaultTokenEnvVar)
}

func TestCreateChatCompletionStream_RequestShape(t *testing.T) {
	t.Parallel()

	var (
		mu              sync.Mutex
		receivedPath    string
		receivedAuth    string
		receivedTitle   string
		receivedReferer string
		receivedBody    map[string]any
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		receivedPath = r.URL.Path
		receivedAuth = r.Header.Get("Authorization")
		receivedTitle = r.Header.Get("X-Title")
		receivedReferer = r.Header.Get("HTTP-Referer")
		_ = json.NewDecoder(r.Body).Decode(&receivedBody)
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)

		writeSSEChunk(t, w, map[string]any{
			"id":      "chunk_1",
			"object":  "chat.completion.chunk",
			"created": 1,
			"model":   "openai/o3-mini",
			"choices": []map[string]any{{
				"index":         0,
				"delta":         map[string]any{"content": "Hello"},
				"finish_reason": nil,
			}},
		})
		writeSSEChunk(t, w, map[string]any{
			"id":      "chunk_2",
			"object":  "chat.completion.chunk",
			"created": 2,
			"model":   "openai/o3-mini",
			"choices": []map[string]any{{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": "stop",
			}},
		})
		writeSSEChunk(t, w, map[string]any{
			"id":      "chunk_3",
			"object":  "chat.completion.chunk",
			"created": 3,
			"model":   "openai/o3-mini",
			"choices": []map[string]any{},
			"usage": map[string]any{
				"prompt_tokens":            12,
				"completion_tokens":        4,
				"total_tokens":             16,
				"prompt_token_details":     map[string]any{"cached_tokens": 2, "cache_write_tokens": 1},
				"completion_token_details": map[string]any{"reasoning_tokens": 3},
			},
		})
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer server.Close()

	parallelToolCalls := true
	trackUsage := true
	client, err := NewClient(t.Context(), &latest.ModelConfig{
		Provider:          "openrouter",
		Model:             "openai/o3-mini",
		BaseURL:           server.URL,
		ThinkingBudget:    &latest.ThinkingBudget{Effort: "medium"},
		ParallelToolCalls: &parallelToolCalls,
		TrackUsage:        &trackUsage,
		ProviderOpts: map[string]any{
			"models": []string{"openai/o3-mini", "openai/gpt-5-mini"},
			"provider": map[string]any{
				"only":            []string{"openai"},
				"allow_fallbacks": false,
			},
			"transforms": []string{"middle-out"},
			"web_search_options": map[string]any{
				"search_context_size": "high",
			},
			"x_title":      "docker-agent-tests",
			"http_referer": "https://example.test/app",
		},
	}, environment.NewMapEnvProvider(map[string]string{
		defaultTokenEnvVar: "openrouter-secret",
	}), options.WithStructuredOutput(&latest.StructuredOutput{
		Name:        "weather",
		Description: "Structured weather output",
		Strict:      true,
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"summary": map[string]any{"type": "string"},
			},
			"required": []string{"summary"},
		},
	}))
	require.NoError(t, err)

	stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{
		Role:    chat.MessageRoleUser,
		Content: "What's the weather?",
	}}, []tools.Tool{{
		Name:        "get_weather",
		Description: "Get the current weather",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"location": map[string]any{"type": "string"},
			},
		},
	}})
	require.NoError(t, err)
	defer stream.Close()

	last, err := drainStream(stream)
	require.NoError(t, err)
	require.NotNil(t, last.Usage)
	assert.Equal(t, int64(10), last.Usage.InputTokens)
	assert.Equal(t, int64(4), last.Usage.OutputTokens)
	assert.Equal(t, int64(2), last.Usage.CachedInputTokens)
	assert.Equal(t, int64(1), last.Usage.CacheWriteTokens)
	assert.Equal(t, int64(3), last.Usage.ReasoningTokens)

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, "/chat/completions", receivedPath)
	assert.Equal(t, "Bearer openrouter-secret", receivedAuth)
	assert.Equal(t, "docker-agent-tests", receivedTitle)
	assert.Equal(t, "https://example.test/app", receivedReferer)
	assert.Equal(t, "openai/o3-mini", receivedBody["model"])
	assert.Equal(t, true, receivedBody["stream"])
	assert.Equal(t, true, receivedBody["parallel_tool_calls"])

	models := receivedBody["models"].([]any)
	assert.Len(t, models, 2)
	assert.Equal(t, "openai/o3-mini", models[0])

	reasoning := receivedBody["reasoning"].(map[string]any)
	assert.Equal(t, "medium", reasoning["effort"])

	provider := receivedBody["provider"].(map[string]any)
	assert.Equal(t, false, provider["allow_fallbacks"])
	assert.Equal(t, []any{"openai"}, provider["only"])

	responseFormat := receivedBody["response_format"].(map[string]any)
	assert.Equal(t, "json_schema", responseFormat["type"])
	jsonSchema := responseFormat["json_schema"].(map[string]any)
	assert.Equal(t, "weather", jsonSchema["name"])
	assert.Equal(t, true, jsonSchema["strict"])

	toolsPayload := receivedBody["tools"].([]any)
	require.Len(t, toolsPayload, 1)
	tool := toolsPayload[0].(map[string]any)
	assert.Equal(t, "function", tool["type"])
	function := tool["function"].(map[string]any)
	assert.Equal(t, "get_weather", function["name"])
	assert.Equal(t, true, function["strict"])

	streamOptions := receivedBody["stream_options"].(map[string]any)
	assert.Equal(t, true, streamOptions["include_usage"])
	assert.Equal(t, []any{"middle-out"}, receivedBody["transforms"])
	webSearch := receivedBody["web_search_options"].(map[string]any)
	assert.Equal(t, "high", webSearch["search_context_size"])
}

func TestCreateChatCompletionStream_ReasoningDetails(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)

		writeSSEChunk(t, w, map[string]any{
			"id":      "chunk_1",
			"object":  "chat.completion.chunk",
			"created": 1,
			"model":   "openai/o3-mini",
			"choices": []map[string]any{{
				"index": 0,
				"delta": map[string]any{
					"reasoning_details": []map[string]any{
						{"type": "reasoning.text", "text": "first step"},
						{"type": "reasoning.summary", "summary": "condensed"},
						{"type": "reasoning.encrypted", "data": "opaque"},
					},
				},
				"finish_reason": nil,
			}},
		})
		writeSSEChunk(t, w, map[string]any{
			"id":      "chunk_2",
			"object":  "chat.completion.chunk",
			"created": 2,
			"model":   "openai/o3-mini",
			"choices": []map[string]any{{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": "stop",
			}},
		})
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer server.Close()

	client, err := NewClient(t.Context(), &latest.ModelConfig{
		Provider: "openrouter",
		Model:    "openai/o3-mini",
		BaseURL:  server.URL,
	}, environment.NewMapEnvProvider(map[string]string{
		defaultTokenEnvVar: "openrouter-secret",
	}))
	require.NoError(t, err)

	stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{
		Role:    chat.MessageRoleUser,
		Content: "think",
	}}, nil)
	require.NoError(t, err)
	defer stream.Close()

	responses, err := collectStream(stream)
	require.NoError(t, err)
	require.Len(t, responses, 2)
	assert.Len(t, responses[0].Choices, 1)
	assert.Equal(t, "first step\ncondensed", responses[0].Choices[0].Delta.ReasoningContent)
}

func TestCreateBatchEmbedding(t *testing.T) {
	t.Parallel()

	var (
		mu           sync.Mutex
		receivedPath string
		receivedBody map[string]any
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		receivedPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&receivedBody)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "emb_1",
			"object": "list",
			"model":  "openai/text-embedding-3-small",
			"data": []map[string]any{
				{"object": "embedding", "index": 0, "embedding": []float64{0.1, 0.2}},
				{"object": "embedding", "index": 1, "embedding": []float64{0.3, 0.4}},
			},
			"usage": map[string]any{
				"prompt_tokens": 7,
				"total_tokens":  7,
				"cost":          0.123,
			},
		})
	}))
	defer server.Close()

	client, err := NewClient(t.Context(), &latest.ModelConfig{
		Provider: "openrouter",
		Model:    "openai/text-embedding-3-small",
		BaseURL:  server.URL,
		ProviderOpts: map[string]any{
			"provider": map[string]any{"only": []string{"openai"}},
		},
	}, environment.NewMapEnvProvider(map[string]string{defaultTokenEnvVar: "openrouter-secret"}))
	require.NoError(t, err)

	result, err := client.CreateBatchEmbedding(t.Context(), []string{"alpha", "beta"})
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, "/embeddings", receivedPath)
	assert.Equal(t, "openai/text-embedding-3-small", receivedBody["model"])
	assert.InEpsilon(t, float64(7), float64(result.InputTokens), 0.000001)
	assert.InEpsilon(t, float64(7), float64(result.TotalTokens), 0.000001)
	assert.InDelta(t, 0.123, result.Cost, 0.000001)
	require.Len(t, result.Embeddings, 2)
	assert.Equal(t, []float64{0.1, 0.2}, result.Embeddings[0])
	assert.Equal(t, []float64{0.3, 0.4}, result.Embeddings[1])
}

func TestCreateChatCompletionStream_InvalidProviderOpts(t *testing.T) {
	t.Parallel()

	client, err := NewClient(t.Context(), &latest.ModelConfig{
		Provider: "openrouter",
		Model:    "openai/gpt-5-mini",
		ProviderOpts: map[string]any{
			"models": "not-an-array",
		},
	}, environment.NewMapEnvProvider(map[string]string{defaultTokenEnvVar: "openrouter-secret"}))
	require.NoError(t, err)

	_, err = client.CreateChatCompletionStream(t.Context(), []chat.Message{{
		Role:    chat.MessageRoleUser,
		Content: "hi",
	}}, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "provider_opts.models")
}

func writeSSEChunk(t *testing.T, w http.ResponseWriter, data map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(data)
	require.NoError(t, err)
	_, _ = w.Write([]byte("data: " + string(encoded) + "\n\n"))
}

func drainStream(stream chat.MessageStream) (chat.MessageStreamResponse, error) {
	var last chat.MessageStreamResponse
	for {
		resp, err := stream.Recv()
		if err != nil {
			if err.Error() == "EOF" {
				return last, nil
			}
			return last, err
		}
		last = resp
	}
}

func collectStream(stream chat.MessageStream) ([]chat.MessageStreamResponse, error) {
	var responses []chat.MessageStreamResponse
	for {
		resp, err := stream.Recv()
		if err != nil {
			if err.Error() == "EOF" {
				return responses, nil
			}
			return responses, err
		}
		responses = append(responses, resp)
	}
}
