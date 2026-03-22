package gemini

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"google.golang.org/genai"

	"github.com/docker/docker-agent/pkg/model/provider/base"
)

// CreateEmbedding generates an embedding vector for the given text.
func (c *Client) CreateEmbedding(ctx context.Context, text string) (*base.EmbeddingResult, error) {
	slog.Debug("Creating Gemini embedding", "model", c.ModelConfig.Model, "text_length", len(text))

	batchResult, err := c.CreateBatchEmbedding(ctx, []string{text})
	if err != nil {
		return nil, err
	}

	if len(batchResult.Embeddings) == 0 {
		return nil, errors.New("no embedding returned from Gemini")
	}

	return &base.EmbeddingResult{
		Embedding:   batchResult.Embeddings[0],
		InputTokens: batchResult.InputTokens,
		TotalTokens: batchResult.TotalTokens,
		Cost:        batchResult.Cost,
	}, nil
}

// CreateBatchEmbedding generates embedding vectors for multiple texts.
func (c *Client) CreateBatchEmbedding(ctx context.Context, texts []string) (*base.BatchEmbeddingResult, error) {
	if len(texts) == 0 {
		return &base.BatchEmbeddingResult{
			Embeddings: [][]float64{},
		}, nil
	}

	slog.Debug("Creating Gemini batch embeddings", "model", c.ModelConfig.Model, "batch_size", len(texts))

	client, err := c.clientFn(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create Gemini client for embedding: %w", err)
	}

	contents := make([]*genai.Content, len(texts))
	for i, text := range texts {
		contents[i] = &genai.Content{
			Parts: []*genai.Part{{Text: text}},
		}
	}

	response, err := client.Models.EmbedContent(ctx, c.ModelConfig.Model, contents, &genai.EmbedContentConfig{
		TaskType: "RETRIEVAL_DOCUMENT",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create Gemini embeddings: %w", wrapGeminiError(err))
	}

	if len(response.Embeddings) != len(texts) {
		return nil, fmt.Errorf("expected %d embeddings, got %d", len(texts), len(response.Embeddings))
	}

	embeddings := make([][]float64, len(response.Embeddings))
	for i, emb := range response.Embeddings {
		embedding := make([]float64, len(emb.Values))
		for j, v := range emb.Values {
			embedding[j] = float64(v)
		}
		embeddings[i] = embedding
	}

	slog.Debug("Gemini batch embeddings created successfully",
		"batch_size", len(embeddings),
		"dimension", len(embeddings[0]))

	return &base.BatchEmbeddingResult{
		Embeddings: embeddings,
		Cost:       0, // Cost calculated at strategy level
	}, nil
}
