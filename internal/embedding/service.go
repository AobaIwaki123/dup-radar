package embedding

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

type VertexAIClient struct {
    endpoint string
    apiKey   string
}

type Request struct {
    Input string `json:"input"`
}

type Response struct {
    Output string `json:"output"`
}

func NewVertexAIClient(endpoint, apiKey string) *VertexAIClient {
    return &VertexAIClient{
        endpoint: endpoint,
        apiKey:   apiKey,
    }
}

func (client *VertexAIClient) CallAI(ctx context.Context, input string) (string, error) {
    reqBody := Request{Input: input}
    jsonReqBody, err := json.Marshal(reqBody)
    if err != nil {
        return "", err
    }

    req, err := http.NewRequestWithContext(ctx, http.MethodPost, client.endpoint, jsonReqBody)
    if err != nil {
        return "", err
    }
    req.Header.Set("Content-Type", "application/json")
    req.Header.Set("Authorization", "Bearer "+client.apiKey)

    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        return "", err
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        return "", fmt.Errorf("failed to call Vertex AI: %s", resp.Status)
    }

    var response Response
    if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
        return "", err
    }

    return response.Output, nil
}
