// Package embedding provides functionality to create text embeddings using Vertex AI
package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/AobaIwaki123/dup-radar/internal/config"
)

// Response represents the structure of Vertex AI embedding API response
type embedResp struct {
	Predictions []struct {
		Embeddings struct {
			Values     []float64 `json:"values"`
			Statistics struct {
				TokenCount int  `json:"token_count"`
				Truncated  bool `json:"truncated"`
			} `json:"statistics"`
		} `json:"embeddings"`
	} `json:"predictions"`
}

// Request structure for Vertex AI embedding API
type embedReq struct {
	Instances []instanceReq `json:"instances"`
}

// instanceReq represents a single embedding request instance
type instanceReq struct {
	TaskType string `json:"task_type,omitempty"`
	Title    string `json:"title,omitempty"`
	Content  string `json:"content"`
}

// TaskType defines the different types of embedding tasks
type TaskType string

const (
	// Task type constants as defined by Vertex AI
	TaskTypeRetrievalQuery     TaskType = "RETRIEVAL_QUERY"
	TaskTypeRetrievalDocument  TaskType = "RETRIEVAL_DOCUMENT"
	TaskTypeSemanticSimilarity TaskType = "SEMANTIC_SIMILARITY"
	TaskTypeClassification     TaskType = "CLASSIFICATION"
	TaskTypeClustering         TaskType = "CLUSTERING"
	TaskTypeQuestionAnswering  TaskType = "QUESTION_ANSWERING"
	TaskTypeFactVerification   TaskType = "FACT_VERIFICATION"
	TaskTypeCodeRetrievalQuery TaskType = "CODE_RETRIEVAL_QUERY"
)

// buildVertexAIEndpoint constructs the Vertex AI API endpoint URL
func buildVertexAIEndpoint(cfg *config.Config) string {
	region := strings.ToLower(cfg.GCP.Region)
	return fmt.Sprintf(
		"https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/google/models/%s:predict",
		region,
		cfg.GCP.ProjectID,
		region,
		cfg.GCP.EmbeddingModel,
	)
}

// EmbeddingResult contains the embedding vector and metadata
type EmbeddingResult struct {
	Embedding  []float64
	TokenCount int
	Truncated  bool
}

// CreateEmbedding creates a vector embedding for the given text using Vertex AI
// Defaults to RETRIEVAL_DOCUMENT task type
func CreateEmbedding(ctx context.Context, cfg *config.Config, text string) ([]float64, error) {
	result, err := CreateEmbeddingWithOptions(ctx, cfg, text, string(TaskTypeRetrievalDocument), "")
	if err != nil {
		return nil, err
	}
	return result.Embedding, nil
}

// CreateEmbeddingWithOptions creates a vector embedding with specific task type and title
func CreateEmbeddingWithOptions(ctx context.Context, cfg *config.Config, text, taskType, title string) (*EmbeddingResult, error) {
	log.Printf("DEBUG: Creating embedding for text (length: %d characters)", len(text))
	
	endpoint := buildVertexAIEndpoint(cfg)
	log.Printf("DEBUG: Using Vertex AI endpoint: %s", endpoint)

	// Create a properly structured request according to Vertex AI documentation
	request := embedReq{
		Instances: []instanceReq{
			{
				TaskType: taskType,
				Title:    title,
				Content:  text,
			},
		},
	}
	
	body, err := json.Marshal(request)
	if err != nil {
		log.Printf("ERROR: Failed to marshal request: %v", err)
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}
	log.Printf("DEBUG: Request payload size: %d bytes", len(body))

	req, _ := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key := os.Getenv("VERTEX_API_KEY"); key != "" {
		log.Printf("DEBUG: Using Vertex API key for authentication (length: %d)", len(key))
		req.Header.Set("Authorization", "Bearer "+key)
	} else {
		log.Printf("DEBUG: No Vertex API key found, using Application Default Credentials")
	}

	log.Printf("DEBUG: Sending request to Vertex AI embedding endpoint")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("ERROR: Vertex AI request failed: %v", err)
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		log.Printf("ERROR: Vertex API returned non-200 response: %s, body: %s", resp.Status, string(b))
		return nil, fmt.Errorf("vertex api error: %s: %s", resp.Status, string(b))
	}
	log.Printf("DEBUG: Vertex AI response received with status: %s", resp.Status)

	var out embedResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		log.Printf("ERROR: Failed to decode Vertex AI response: %v", err)
		return nil, err
	}

	if len(out.Predictions) == 0 {
		log.Printf("ERROR: Vertex API returned empty predictions array")
		return nil, fmt.Errorf("vertex api: empty predictions")
	}

	result := &EmbeddingResult{
		Embedding:  out.Predictions[0].Embeddings.Values,
		TokenCount: out.Predictions[0].Embeddings.Statistics.TokenCount,
		Truncated:  out.Predictions[0].Embeddings.Statistics.Truncated,
	}

	embeddingSize := len(result.Embedding)
	log.Printf("DEBUG: Successfully created embedding with %d dimensions (tokens: %d, truncated: %v)", 
		embeddingSize, result.TokenCount, result.Truncated)
	
	return result, nil
}
