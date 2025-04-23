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
		Embedding []float64 `json:"embedding"`
	} `json:"predictions"`
}

// CreateEmbedding creates a vector embedding for the given text using Vertex AI
func CreateEmbedding(ctx context.Context, cfg *config.Config, text string) ([]float64, error) {
	log.Printf("DEBUG: Creating embedding for text (length: %d characters)", len(text))
	endpoint := fmt.Sprintf("https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/google/models/%s:predict",
		strings.ToLower(cfg.GCP.Region), cfg.GCP.ProjectID, strings.ToLower(cfg.GCP.Region), cfg.GCP.EmbeddingModel)
	log.Printf("DEBUG: Using Vertex AI endpoint: %s", endpoint)

	body, _ := json.Marshal(map[string]any{"instances": []map[string]string{{"content": text}}})
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

	embeddingSize := len(out.Predictions[0].Embedding)
	log.Printf("DEBUG: Successfully created embedding with %d dimensions", embeddingSize)
	return out.Predictions[0].Embedding, nil
}
