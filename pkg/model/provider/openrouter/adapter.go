package openrouter

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"

	openrouterapi "github.com/revrost/go-openrouter"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

type StreamAdapter struct {
	stream           *openrouterapi.ChatCompletionStream
	reader           *bufio.Reader
	body             io.Closer
	lastFinishReason chat.FinishReason
	toolCalls        map[int]string
	trackUsage       bool
}

func newHTTPStreamAdapter(reader *bufio.Reader, body io.Closer, trackUsage bool) chat.MessageStream {
	return &StreamAdapter{
		reader:     reader,
		body:       body,
		toolCalls:  make(map[int]string),
		trackUsage: trackUsage,
	}
}

func flattenReasoningDetails(details []openrouterapi.ChatCompletionReasoningDetails) string {
	if len(details) == 0 {
		return ""
	}

	var parts []string
	for _, detail := range details {
		switch detail.Type {
		case openrouterapi.ReasoningDetailsTypeText:
			if detail.Text != "" {
				parts = append(parts, detail.Text)
			}
		case openrouterapi.ReasoningDetailsTypeSummary:
			if detail.Summary != "" {
				parts = append(parts, detail.Summary)
			}
		default:
			if detail.Text != "" {
				parts = append(parts, detail.Text)
				continue
			}
			if detail.Summary != "" {
				parts = append(parts, detail.Summary)
			}
		}
	}

	return strings.Join(parts, "\n")
}

func (a *StreamAdapter) recvHTTPChunk() (openrouterapi.ChatCompletionStreamResponse, error) {
	for {
		line, err := a.reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				return openrouterapi.ChatCompletionStreamResponse{}, io.EOF
			}
			return openrouterapi.ChatCompletionStreamResponse{}, err
		}

		if strings.HasSuffix(string(line), "[DONE]\n") {
			return openrouterapi.ChatCompletionStreamResponse{}, io.EOF
		}
		if strings.HasPrefix(string(line), ": OPENROUTER PROCESSING") || string(line) == "\n" {
			continue
		}

		line = bytes.TrimPrefix(line, []byte("data:"))
		var chunk openrouterapi.ChatCompletionStreamResponse
		if err := json.Unmarshal(line, &chunk); err != nil {
			return openrouterapi.ChatCompletionStreamResponse{}, err
		}
		return chunk, nil
	}
}

func (a *StreamAdapter) Recv() (chat.MessageStreamResponse, error) {
	var (
		chunk openrouterapi.ChatCompletionStreamResponse
		err   error
	)
	if a.stream != nil {
		chunk, err = a.stream.Recv()
	} else {
		chunk, err = a.recvHTTPChunk()
	}
	if err != nil {
		return chat.MessageStreamResponse{}, err
	}

	response := chat.MessageStreamResponse{
		ID:      chunk.ID,
		Object:  chunk.Object,
		Created: chunk.Created,
		Model:   chunk.Model,
		Choices: make([]chat.MessageStreamChoice, len(chunk.Choices)),
	}

	for i := range chunk.Choices {
		choice := chunk.Choices[i]
		finishReason := chat.FinishReason(choice.FinishReason)
		if a.trackUsage && finishReason != chat.FinishReasonNull && finishReason != "" {
			a.lastFinishReason = finishReason
			finishReason = ""
		}

		reasoningContent := choice.Delta.ReasoningContent
		if reasoningContent == "" && choice.Delta.Reasoning != nil {
			reasoningContent = *choice.Delta.Reasoning
		}
		if reasoningContent == "" {
			reasoningContent = flattenReasoningDetails(choice.Delta.ReasoningDetails)
		}

		response.Choices[i] = chat.MessageStreamChoice{
			Index:        choice.Index,
			FinishReason: finishReason,
			Delta: chat.MessageDelta{
				Role:             choice.Delta.Role,
				Content:          choice.Delta.Content,
				ReasoningContent: reasoningContent,
			},
		}

		if choice.Delta.FunctionCall != nil {
			response.Choices[i].Delta.FunctionCall = &tools.FunctionCall{
				Name:      choice.Delta.FunctionCall.Name,
				Arguments: choice.Delta.FunctionCall.Arguments,
			}
		}

		if len(choice.Delta.ToolCalls) > 0 {
			response.Choices[i].Delta.ToolCalls = make([]tools.ToolCall, len(choice.Delta.ToolCalls))
			for j, toolCall := range choice.Delta.ToolCalls {
				id := toolCall.ID
				index := j
				if toolCall.Index != nil {
					index = *toolCall.Index
				}
				if existing, ok := a.toolCalls[index]; ok && id == "" {
					id = existing
				} else if id != "" {
					a.toolCalls[index] = id
				}

				response.Choices[i].Delta.ToolCalls[j] = tools.ToolCall{
					ID:   id,
					Type: tools.ToolType(toolCall.Type),
					Function: tools.FunctionCall{
						Name:      toolCall.Function.Name,
						Arguments: toolCall.Function.Arguments,
					},
				}
			}
		}
	}

	if chunk.Usage != nil {
		if a.trackUsage {
			response.Usage = &chat.Usage{
				InputTokens:       int64(chunk.Usage.PromptTokens - chunk.Usage.PromptTokenDetails.CachedTokens),
				OutputTokens:      int64(chunk.Usage.CompletionTokens),
				CachedInputTokens: int64(chunk.Usage.PromptTokenDetails.CachedTokens),
				CacheWriteTokens:  int64(chunk.Usage.PromptTokenDetails.CacheWriteTokens),
				ReasoningTokens:   int64(chunk.Usage.CompletionTokenDetails.ReasoningTokens),
			}
		}

		finishReason := a.lastFinishReason
		if finishReason == chat.FinishReasonNull || finishReason == "" {
			finishReason = chat.FinishReasonStop
		}

		if len(response.Choices) == 0 {
			response.Choices = append(response.Choices, chat.MessageStreamChoice{FinishReason: finishReason})
		} else {
			response.Choices[0].FinishReason = finishReason
		}
	}

	return response, nil
}

func (a *StreamAdapter) Close() {
	if a.stream != nil {
		a.stream.Close()
	}
	if a.body != nil {
		_ = a.body.Close()
	}
}
