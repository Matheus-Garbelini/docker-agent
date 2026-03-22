package openrouter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	openrouterapi "github.com/revrost/go-openrouter"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/oaistream"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/tools"
)

const defaultTokenEnvVar = "OPENROUTER_API_KEY"

type Client struct {
	base.Config

	clientFn   func(context.Context) (*openrouterapi.Client, error)
	authToken  string
	baseURL    string
	httpClient openrouterapi.HTTPDoer
	xTitle     string
	referer    string
}

func NewClient(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (*Client, error) {
	if cfg == nil {
		slog.Error("OpenRouter client creation failed", "error", "model configuration is required")
		return nil, errors.New("model configuration is required")
	}

	var globalOptions options.ModelOptions
	for _, opt := range opts {
		opt(&globalOptions)
	}

	tokenKey := cfg.TokenKey
	if tokenKey == "" {
		tokenKey = defaultTokenEnvVar
	}

	authToken, _ := env.Get(ctx, tokenKey)
	if authToken == "" {
		return nil, fmt.Errorf("%s environment variable is required", tokenKey)
	}

	clientConfig := openrouterapi.DefaultConfig(authToken)
	if cfg.BaseURL != "" {
		clientConfig.BaseURL = cfg.BaseURL
	}
	clientConfig.HTTPClient = httpclient.NewHTTPClient()

	if cfg.ProviderOpts != nil {
		if xTitle, err := stringOpt(cfg.ProviderOpts, "x_title"); err != nil {
			return nil, err
		} else if xTitle != "" {
			clientConfig.XTitle = xTitle
		}

		if referer, err := stringOpt(cfg.ProviderOpts, "http_referer"); err != nil {
			return nil, err
		} else if referer != "" {
			clientConfig.HttpReferer = referer
		}
	}

	client := openrouterapi.NewClientWithConfig(*clientConfig)

	return &Client{
		Config: base.Config{
			ModelConfig:  *cfg,
			ModelOptions: globalOptions,
			Env:          env,
		},
		clientFn: func(context.Context) (*openrouterapi.Client, error) {
			return client, nil
		},
		authToken:  authToken,
		baseURL:    strings.TrimRight(clientConfig.BaseURL, "/"),
		httpClient: clientConfig.HTTPClient,
		xTitle:     clientConfig.XTitle,
		referer:    clientConfig.HttpReferer,
	}, nil
}

func (c *Client) CreateChatCompletionStream(
	ctx context.Context,
	messages []chat.Message,
	requestTools []tools.Tool,
) (chat.MessageStream, error) {
	if len(messages) == 0 {
		return nil, errors.New("at least one message is required")
	}

	params, err := c.buildChatCompletionRequest(messages, requestTools)
	if err != nil {
		return nil, err
	}

	requestJSON, err := marshalChatCompletionRequest(params)
	if err != nil {
		return nil, err
	}

	if len(requestJSON) > 0 {
		slog.Debug("OpenRouter chat completion request", "request", string(requestJSON))
	}

	stream, err := c.createChatCompletionStream(ctx, requestJSON)
	if err != nil {
		return nil, err
	}

	return stream, nil
}

func marshalChatCompletionRequest(params openrouterapi.ChatCompletionRequest) ([]byte, error) {
	requestJSON, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}

	var payload map[string]any
	if err := json.Unmarshal(requestJSON, &payload); err != nil {
		return nil, err
	}

	if reasoningValue, ok := payload["reasoning"]; ok {
		if reasoningMap, ok := reasoningValue.(map[string]any); ok {
			if effort, hasPrompt := reasoningMap["prompt"]; hasPrompt {
				reasoningMap["effort"] = effort
				delete(reasoningMap, "prompt")
			}
		}
	}

	return json.Marshal(payload)
}

func (c *Client) createChatCompletionStream(ctx context.Context, requestJSON []byte) (chat.MessageStream, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", strings.NewReader(string(requestJSON)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.authToken)
	req.Header.Set("HTTP-Referer", c.referer)
	req.Header.Set("X-Title", c.xTitle)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusBadRequest {
		defer resp.Body.Close()
		var responseBody string
		if body, readErr := io.ReadAll(resp.Body); readErr == nil {
			responseBody = strings.TrimSpace(string(body))
		}
		if responseBody == "" {
			responseBody = resp.Status
		}
		return nil, fmt.Errorf("openrouter chat completions request failed: %s", responseBody)
	}

	trackUsage := c.ModelConfig.TrackUsage == nil || *c.ModelConfig.TrackUsage
	return newHTTPStreamAdapter(bufio.NewReader(resp.Body), resp.Body, trackUsage), nil
}

func (c *Client) buildChatCompletionRequest(messages []chat.Message, requestTools []tools.Tool) (openrouterapi.ChatCompletionRequest, error) {
	trackUsage := c.ModelConfig.TrackUsage == nil || *c.ModelConfig.TrackUsage

	params := openrouterapi.ChatCompletionRequest{
		Model:    c.ModelConfig.Model,
		Messages: convertMessages(messages),
		Stream:   true,
	}

	if trackUsage {
		params.StreamOptions = &openrouterapi.StreamOptions{IncludeUsage: true}
	}

	if c.ModelConfig.Temperature != nil {
		params.Temperature = float32(*c.ModelConfig.Temperature)
	}
	if c.ModelConfig.TopP != nil {
		params.TopP = float32(*c.ModelConfig.TopP)
	}
	if c.ModelConfig.FrequencyPenalty != nil {
		params.FrequencyPenalty = float32(*c.ModelConfig.FrequencyPenalty)
	}
	if c.ModelConfig.PresencePenalty != nil {
		params.PresencePenalty = float32(*c.ModelConfig.PresencePenalty)
	}
	if maxToken := c.ModelConfig.MaxTokens; maxToken != nil && *maxToken > 0 {
		params.MaxTokens = int(*maxToken)
	}

	if len(requestTools) > 0 {
		toolsParam := make([]openrouterapi.Tool, len(requestTools))
		for i, tool := range requestTools {
			parameters, err := oaistream.ConvertParametersToSchema(tool.Parameters)
			if err != nil {
				return openrouterapi.ChatCompletionRequest{}, err
			}

			toolsParam[i] = openrouterapi.Tool{
				Type: openrouterapi.ToolTypeFunction,
				Function: &openrouterapi.FunctionDefinition{
					Name:        tool.Name,
					Description: tool.Description,
					Parameters:  parameters,
					Strict:      true,
				},
			}
		}
		params.Tools = toolsParam

		if c.ModelConfig.ParallelToolCalls != nil {
			params.ParallelToolCalls = *c.ModelConfig.ParallelToolCalls
		}
	}

	if c.ModelConfig.ThinkingBudget != nil {
		reasoning, err := getOpenRouterReasoning(c.ModelConfig.ThinkingBudget)
		if err != nil {
			return openrouterapi.ChatCompletionRequest{}, err
		}
		params.Reasoning = reasoning
	}

	if structuredOutput := c.ModelOptions.StructuredOutput(); structuredOutput != nil {
		params.ResponseFormat = &openrouterapi.ChatCompletionResponseFormat{
			Type: openrouterapi.ChatCompletionResponseFormatTypeJSONSchema,
			JSONSchema: &openrouterapi.ChatCompletionResponseFormatJSONSchema{
				Name:        structuredOutput.Name,
				Description: structuredOutput.Description,
				Schema:      oaistream.JSONSchema(structuredOutput.Schema),
				Strict:      structuredOutput.Strict,
			},
		}
	}

	if err := applyProviderOpts(&params, c.ModelConfig.ProviderOpts); err != nil {
		return openrouterapi.ChatCompletionRequest{}, err
	}

	return params, nil
}

func convertMessages(messages []chat.Message) []openrouterapi.ChatCompletionMessage {
	converted := make([]openrouterapi.ChatCompletionMessage, 0, len(messages))
	for i := range messages {
		msg := messages[i]

		if msg.Role == chat.MessageRoleAssistant && len(msg.ToolCalls) == 0 && len(msg.MultiContent) == 0 && strings.TrimSpace(msg.Content) == "" {
			continue
		}

		convertedMsg := openrouterapi.ChatCompletionMessage{Role: string(msg.Role)}

		switch msg.Role {
		case chat.MessageRoleSystem, chat.MessageRoleUser:
			convertedMsg.Content = convertContent(msg)

		case chat.MessageRoleAssistant:
			convertedMsg.Content = convertContent(msg)
			if msg.FunctionCall != nil {
				convertedMsg.FunctionCall = &openrouterapi.FunctionCall{
					Name:      msg.FunctionCall.Name,
					Arguments: msg.FunctionCall.Arguments,
				}
			}
			if msg.ReasoningContent != "" {
				reasoning := msg.ReasoningContent
				convertedMsg.Reasoning = &reasoning
				convertedMsg.ReasoningContent = &reasoning
			}
			if len(msg.ToolCalls) > 0 {
				convertedMsg.ToolCalls = make([]openrouterapi.ToolCall, len(msg.ToolCalls))
				for j, toolCall := range msg.ToolCalls {
					convertedMsg.ToolCalls[j] = openrouterapi.ToolCall{
						ID:   toolCall.ID,
						Type: openrouterapi.ToolType(toolCall.Type),
						Function: openrouterapi.FunctionCall{
							Name:      toolCall.Function.Name,
							Arguments: toolCall.Function.Arguments,
						},
					}
				}
			}

		case chat.MessageRoleTool:
			convertedMsg.ToolCallID = msg.ToolCallID
			convertedMsg.Content = convertToolContent(msg)
		}

		converted = append(converted, convertedMsg)

		if msg.Role == chat.MessageRoleTool && len(msg.MultiContent) > 0 {
			imageParts := convertImageParts(msg.MultiContent)
			if len(imageParts) > 0 {
				converted = append(converted, openrouterapi.ChatCompletionMessage{
					Role: openrouterapi.ChatMessageRoleUser,
					Content: openrouterapi.Content{Multi: append([]openrouterapi.ChatMessagePart{{
						Type: openrouterapi.ChatMessagePartTypeText,
						Text: "Attached image(s) from tool result:",
					}}, imageParts...)},
				})
			}
		}
	}

	return converted
}

func convertContent(msg chat.Message) openrouterapi.Content {
	if len(msg.MultiContent) == 0 {
		return openrouterapi.Content{Text: msg.Content}
	}
	return openrouterapi.Content{Multi: convertMultiContent(msg.MultiContent, false)}
}

func convertToolContent(msg chat.Message) openrouterapi.Content {
	if len(msg.MultiContent) == 0 {
		return openrouterapi.Content{Text: msg.Content}
	}
	return openrouterapi.Content{Multi: convertMultiContent(msg.MultiContent, true)}
}

func convertMultiContent(parts []chat.MessagePart, textOnly bool) []openrouterapi.ChatMessagePart {
	converted := make([]openrouterapi.ChatMessagePart, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case chat.MessagePartTypeText:
			converted = append(converted, openrouterapi.ChatMessagePart{
				Type: openrouterapi.ChatMessagePartTypeText,
				Text: part.Text,
			})
		case chat.MessagePartTypeImageURL:
			if textOnly || part.ImageURL == nil {
				continue
			}
			converted = append(converted, openrouterapi.ChatMessagePart{
				Type: openrouterapi.ChatMessagePartTypeImageURL,
				ImageURL: &openrouterapi.ChatMessageImageURL{
					URL:    part.ImageURL.URL,
					Detail: openrouterapi.ImageURLDetail(part.ImageURL.Detail),
				},
			})
		}
	}
	return converted
}

func convertImageParts(parts []chat.MessagePart) []openrouterapi.ChatMessagePart {
	converted := make([]openrouterapi.ChatMessagePart, 0, len(parts))
	for _, part := range parts {
		if part.Type != chat.MessagePartTypeImageURL || part.ImageURL == nil {
			continue
		}
		converted = append(converted, openrouterapi.ChatMessagePart{
			Type: openrouterapi.ChatMessagePartTypeImageURL,
			ImageURL: &openrouterapi.ChatMessageImageURL{
				URL:    part.ImageURL.URL,
				Detail: openrouterapi.ImageURLDetail(part.ImageURL.Detail),
			},
		})
	}
	return converted
}

func (c *Client) CreateEmbedding(ctx context.Context, text string) (*base.EmbeddingResult, error) {
	batch, err := c.CreateBatchEmbedding(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(batch.Embeddings) == 0 {
		return nil, errors.New("no embedding returned from OpenRouter")
	}

	return &base.EmbeddingResult{
		Embedding:   batch.Embeddings[0],
		InputTokens: batch.InputTokens,
		TotalTokens: batch.TotalTokens,
		Cost:        batch.Cost,
	}, nil
}

func (c *Client) CreateBatchEmbedding(ctx context.Context, texts []string) (*base.BatchEmbeddingResult, error) {
	if len(texts) == 0 {
		return &base.BatchEmbeddingResult{Embeddings: [][]float64{}}, nil
	}

	client, err := c.clientFn(ctx)
	if err != nil {
		return nil, err
	}

	req := openrouterapi.EmbeddingsRequest{
		Model:          c.ModelConfig.Model,
		Input:          texts,
		EncodingFormat: openrouterapi.EmbeddingsEncodingFormatFloat,
	}

	if c.ModelConfig.ProviderOpts != nil {
		if providerValue, ok := c.ModelConfig.ProviderOpts["provider"]; ok {
			providerCfg, err := decodeProvider(providerValue, "provider_opts.provider")
			if err != nil {
				return nil, err
			}
			req.Provider = providerCfg
		}
	}

	resp, err := client.CreateEmbeddings(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to create embeddings: %w", err)
	}

	if len(resp.Data) != len(texts) {
		return nil, fmt.Errorf("expected %d embeddings, got %d", len(texts), len(resp.Data))
	}

	embeddings := make([][]float64, len(resp.Data))
	for i, data := range resp.Data {
		if len(data.Embedding.Vector) == 0 {
			return nil, fmt.Errorf("embedding %d was not returned in float format", i)
		}
		embeddings[i] = data.Embedding.Vector
	}

	var inputTokens int64
	var totalTokens int64
	var cost float64
	if resp.Usage != nil {
		inputTokens = int64(resp.Usage.PromptTokens)
		totalTokens = int64(resp.Usage.TotalTokens)
		cost = resp.Usage.Cost
	}

	return &base.BatchEmbeddingResult{
		Embeddings:  embeddings,
		InputTokens: inputTokens,
		TotalTokens: totalTokens,
		Cost:        cost,
	}, nil
}

func getOpenRouterReasoning(budget *latest.ThinkingBudget) (*openrouterapi.ChatCompletionReasoning, error) {
	if budget == nil {
		return nil, nil
	}

	reasoning := &openrouterapi.ChatCompletionReasoning{}
	if budget.Tokens > 0 {
		tokens := budget.Tokens
		reasoning.MaxTokens = &tokens
		return reasoning, nil
	}

	effort := strings.TrimSpace(strings.ToLower(budget.Effort))
	if effort == "" {
		return nil, errors.New("OpenRouter reasoning requires thinking_budget effort or tokens")
	}
	if effort != "minimal" && effort != "low" && effort != "medium" && effort != "high" {
		return nil, fmt.Errorf("OpenRouter requests only support 'minimal', 'low', 'medium', 'high' as values for thinking_budget effort, got effort: '%s', tokens: '%d'", effort, budget.Tokens)
	}
	reasoning.Effort = &effort

	return reasoning, nil
}

func applyProviderOpts(params *openrouterapi.ChatCompletionRequest, providerOpts map[string]any) error {
	if providerOpts == nil {
		return nil
	}

	if modelsValue, ok := providerOpts["models"]; ok {
		models, err := decodeStringSlice(modelsValue, "provider_opts.models")
		if err != nil {
			return err
		}
		params.Models = models
	}

	if transformsValue, ok := providerOpts["transforms"]; ok {
		transforms, err := decodeStringSlice(transformsValue, "provider_opts.transforms")
		if err != nil {
			return err
		}
		params.Transforms = transforms
	}

	if providerValue, ok := providerOpts["provider"]; ok {
		providerCfg, err := decodeProvider(providerValue, "provider_opts.provider")
		if err != nil {
			return err
		}
		params.Provider = providerCfg
	}

	if pluginsValue, ok := providerOpts["plugins"]; ok {
		plugins, err := decodePlugins(pluginsValue, "provider_opts.plugins")
		if err != nil {
			return err
		}
		params.Plugins = plugins
	}

	if webSearchValue, ok := providerOpts["web_search_options"]; ok {
		webSearch, err := decodeWebSearchOptions(webSearchValue, "provider_opts.web_search_options")
		if err != nil {
			return err
		}
		params.WebSearchOptions = webSearch
	}

	if modalitiesValue, ok := providerOpts["modalities"]; ok {
		modalities, err := decodeModalities(modalitiesValue, "provider_opts.modalities")
		if err != nil {
			return err
		}
		params.Modalities = modalities
	}

	if imageConfigValue, ok := providerOpts["image_config"]; ok {
		imageConfig, err := decodeImageConfig(imageConfigValue, "provider_opts.image_config")
		if err != nil {
			return err
		}
		params.ImageConfig = imageConfig
	}

	return nil
}

func stringOpt(providerOpts map[string]any, key string) (string, error) {
	value, ok := providerOpts[key]
	if !ok {
		return "", nil
	}
	str, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("provider_opts.%s must be a string", key)
	}
	return strings.TrimSpace(str), nil
}

func decodeStringSlice(value any, field string) ([]string, error) {
	var out []string
	if err := decodeJSONValue(value, &out, field); err != nil {
		return nil, err
	}
	return out, nil
}

func decodeProvider(value any, field string) (*openrouterapi.ChatProvider, error) {
	var out openrouterapi.ChatProvider
	if err := decodeJSONValue(value, &out, field); err != nil {
		return nil, err
	}
	return &out, nil
}

func decodePlugins(value any, field string) ([]openrouterapi.ChatCompletionPlugin, error) {
	var out []openrouterapi.ChatCompletionPlugin
	if err := decodeJSONValue(value, &out, field); err != nil {
		return nil, err
	}
	return out, nil
}

func decodeWebSearchOptions(value any, field string) (*openrouterapi.WebSearchOptions, error) {
	var out openrouterapi.WebSearchOptions
	if err := decodeJSONValue(value, &out, field); err != nil {
		return nil, err
	}
	return &out, nil
}

func decodeModalities(value any, field string) ([]openrouterapi.ChatCompletionModality, error) {
	var values []string
	if err := decodeJSONValue(value, &values, field); err != nil {
		return nil, err
	}

	out := make([]openrouterapi.ChatCompletionModality, len(values))
	for i, item := range values {
		out[i] = openrouterapi.ChatCompletionModality(item)
	}
	return out, nil
}

func decodeImageConfig(value any, field string) (*openrouterapi.ChatCompletionImageConfig, error) {
	var out openrouterapi.ChatCompletionImageConfig
	if err := decodeJSONValue(value, &out, field); err != nil {
		return nil, err
	}
	return &out, nil
}

func decodeJSONValue(value, out any, field string) error {
	buf, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("failed to encode %s: %w", field, err)
	}
	if err := json.Unmarshal(buf, out); err != nil {
		return fmt.Errorf("invalid %s: %w", field, err)
	}
	return nil
}
