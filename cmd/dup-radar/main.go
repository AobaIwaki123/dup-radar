package main

// DupRadar – minimal but functional MVP
// -------------------------------------
// - Receives GitHub issues.opened webhooks (HMAC‑SHA256 verified)
// - Creates an embedding with Vertex AI text‑embedding‑005 (API Key or ADC)
// - Searches BigQuery ML.DISTANCE for similar issues
// - Comments top‑k similar issues if distance below threshold
// - Stores the new issue vector back into BigQuery
//
// Env vars (see .env.example):
//   GITHUB_PAT                – Personal access token (classic or fine‑grained)
//   GITHUB_WEBHOOK_SECRET     – same secret as Webhook config
//   VERTEX_API_KEY            – public API key (or omit to use ADC)
//   GOOGLE_APPLICATION_CREDENTIALS – ADC JSON (if not using gcloud login)
//
// Config file: configs/config.yaml (see README)

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/bigquery"
	"github.com/google/go-github/v62/github"
	"github.com/joho/godotenv"
	"golang.org/x/oauth2"
	"google.golang.org/api/iterator"
	"gopkg.in/yaml.v3"
)

// ---------------- Configuration ----------------

type Config struct {
    Server struct {
        Port int    `yaml:"port"`
        Path string `yaml:"path"`
    } `yaml:"server"`
    GitHub struct {
        Similarity float64 `yaml:"similarity_threshold"`
        TopK       int     `yaml:"top_k"`
    } `yaml:"github"`
    GCP struct {
        ProjectID      string `yaml:"project_id"`
        Dataset        string `yaml:"bq_dataset"`
        Table          string `yaml:"bq_table"`
        Region         string `yaml:"region"`
        EmbeddingModel string `yaml:"embedding_model"`
    } `yaml:"gcp"`
}

func loadConfig(path string) *Config {
    log.Printf("DEBUG: Loading configuration from %s", path)
    f, err := os.Open(path)
    if err != nil { 
        log.Fatalf("ERROR: Failed to open config file: %v", err) 
    }
    defer f.Close()
    var c Config
    if err := yaml.NewDecoder(f).Decode(&c); err != nil {
        log.Fatalf("ERROR: Failed to parse yaml config: %v", err)
    }
    log.Printf("DEBUG: Config loaded successfully: server port=%d, path=%s, similarity=%f, topK=%d", 
        c.Server.Port, c.Server.Path, c.GitHub.Similarity, c.GitHub.TopK)
    log.Printf("DEBUG: GCP config: project=%s, dataset=%s, table=%s, region=%s, model=%s",
        c.GCP.ProjectID, c.GCP.Dataset, c.GCP.Table, c.GCP.Region, c.GCP.EmbeddingModel)
    return &c
}

// ---------------- GitHub client ----------------

func newGitHubClient(ctx context.Context) *github.Client {
    log.Printf("DEBUG: Initializing GitHub client")
    pat := os.Getenv("GITHUB_PAT")
    if pat == "" {
        log.Fatalf("ERROR: GITHUB_PAT environment variable is not set")
    }
    log.Printf("DEBUG: GitHub PAT found (length: %d)", len(pat))
    ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: pat})
    tc := oauth2.NewClient(ctx, ts)
    client := github.NewClient(tc)
    log.Printf("DEBUG: GitHub client initialized successfully")
    return client
}

// ---------------- Vertex AI embedding ----------------

type embedResp struct {
    Predictions []struct {
        Embedding []float64 `json:"embedding"`
    } `json:"predictions"`
}

func embedText(ctx context.Context, cfg *Config, text string) ([]float64, error) {
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
        req.Header.Set("Authorization", "Bearer " + key)
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

// ---------------- BigQuery helpers ----------------

type bqClient struct {
    client *bigquery.Client
    cfg    *Config
}

func newBQ(ctx context.Context, cfg *Config) *bqClient {
    log.Printf("DEBUG: Initializing BigQuery client for project %s", cfg.GCP.ProjectID)
    cli, err := bigquery.NewClient(ctx, cfg.GCP.ProjectID)
    if err != nil { 
        log.Fatalf("ERROR: BigQuery client initialization failed: %v", err) 
    }
    log.Printf("DEBUG: BigQuery client initialized successfully")
    return &bqClient{client: cli, cfg: cfg}
}

func (b *bqClient) searchSimilar(ctx context.Context, vec []float64, topK int) ([]int64, []float64, error) {
    log.Printf("DEBUG: Building BigQuery similarity search query (topK=%d)", topK)
    
    q := b.client.Query(fmt.Sprintf(`SELECT issue_id,
        ML.DISTANCE(embedding, @query_vec, 'COSINE') AS dist
        FROM %s.%s.%s
        ORDER BY dist
        LIMIT %d`,
        b.cfg.GCP.ProjectID, b.cfg.GCP.Dataset, b.cfg.GCP.Table, topK))
    
    log.Printf("DEBUG: Using query parameters with vector of %d dimensions", len(vec))
    q.Parameters = []bigquery.QueryParameter{{Name: "query_vec", Value: vec}}

    log.Printf("DEBUG: Executing BigQuery similarity search")
    it, err := q.Read(ctx)
    if err != nil { 
        log.Printf("ERROR: BigQuery query execution failed: %v", err)
        return nil, nil, err 
    }
    
    var ids []int64
    var dists []float64
    rowCount := 0
    log.Printf("DEBUG: Processing BigQuery query results")
    
    for {
        var row struct {
            IssueID int64   `bigquery:"issue_id"`
            Dist    float64 `bigquery:"dist"`
        }
        switch err := it.Next(&row); err {
        case iterator.Done:
            log.Printf("DEBUG: Completed reading %d similar issues from BigQuery", rowCount)
            return ids, dists, nil
        case nil:
            ids = append(ids, row.IssueID)
            dists = append(dists, row.Dist)
            rowCount++
        default:
            log.Printf("ERROR: Error reading BigQuery results: %v", err)
            return nil, nil, err
        }
    }
}

// BigQuery row definition

type issueRow struct {
    Repo      string    `bigquery:"repo"`
    IssueID   int64     `bigquery:"issue_id"`
    Title     string    `bigquery:"title"`
    Body      string    `bigquery:"body"`
    CreatedAt time.Time `bigquery:"created_at"`
    Embedding []float64 `bigquery:"embedding"`
}

func (b *bqClient) insertVector(ctx context.Context, issue *github.Issue, repo string, vec []float64) error {
    log.Printf("DEBUG: Preparing to insert issue data into BigQuery table %s.%s", b.cfg.GCP.Dataset, b.cfg.GCP.Table)
    ins := b.client.Dataset(b.cfg.GCP.Dataset).Table(b.cfg.GCP.Table).Inserter()
    log.Printf("DEBUG: Creating issue row for repo=%s, issue_id=%d, title=%q", 
               repo, issue.GetNumber(), issue.GetTitle())
    row := &issueRow{
        Repo:      repo,
        IssueID:   int64(issue.GetNumber()),
        Title:     issue.GetTitle(),
        Body:      issue.GetBody(),
        CreatedAt: issue.GetCreatedAt().Time,
        Embedding: vec,
    }
    log.Printf("DEBUG: Inserting row into BigQuery with embedding vector of length %d", len(vec))
    err := ins.Put(ctx, row)
    if err != nil {
        log.Printf("ERROR: BigQuery insertion failed: %v", err)
    } else {
        log.Printf("DEBUG: BigQuery insertion successful for issue #%d", issue.GetNumber())
    }
    return err
}

// ---------------- Webhook handler ----------------

func main() {
    log.Printf("DEBUG: Starting DupRadar service")
    _ = godotenv.Load()
    log.Printf("DEBUG: Environment variables loaded from .env file")
    
    cfg := loadConfig("configs/config.yaml")
    log.Printf("DEBUG: Configuration loaded successfully")

    ctx := context.Background()
    gh := newGitHubClient(ctx)
    bq := newBQ(ctx, cfg)
    log.Printf("DEBUG: GitHub and BigQuery clients initialized")

    secret := os.Getenv("GITHUB_WEBHOOK_SECRET")
    if secret == "" { 
        log.Fatal("ERROR: GITHUB_WEBHOOK_SECRET not set") 
    }
    log.Printf("DEBUG: GitHub webhook secret found (length: %d)", len(secret))
    sigKey := []byte(secret)

    http.HandleFunc(cfg.Server.Path, func(w http.ResponseWriter, r *http.Request) {
        log.Printf("DEBUG: Received webhook request from %s %s", r.RemoteAddr, r.Method)
        payload, err := io.ReadAll(r.Body)
        if err != nil { 
            log.Printf("ERROR: Failed to read request body: %v", err)
            http.Error(w, "read", 400)
            return 
        }
        log.Printf("DEBUG: Read %d bytes from request body", len(payload))
        
        sig := strings.TrimPrefix(r.Header.Get("X-Hub-Signature-256"), "sha256=")
        log.Printf("DEBUG: Checking webhook signature: %s", sig)
        mac := hmac.New(sha256.New, sigKey)
        mac.Write(payload)
        expected := hex.EncodeToString(mac.Sum(nil))
        
        if !hmac.Equal([]byte(sig), []byte(expected)) {
            log.Printf("ERROR: Invalid webhook signature received")
            http.Error(w, "invalid signature", http.StatusUnauthorized)
            return
        }
        log.Printf("DEBUG: Webhook signature verified successfully")

        eventType := github.WebHookType(r)
        log.Printf("DEBUG: Webhook event type: %s", eventType)
        
        event, err := github.ParseWebHook(eventType, payload)
        if err != nil { 
            log.Printf("ERROR: Failed to parse webhook payload: %v", err)
            http.Error(w, "parse", 400)
            return 
        }
        
        if evt, ok := event.(*github.IssuesEvent); ok {
            action := evt.GetAction()
            log.Printf("DEBUG: Received issues event with action: %s", action)
            if action == "opened" {
                issueNumber := evt.GetIssue().GetNumber()
                repoName := evt.GetRepo().GetFullName()
                log.Printf("DEBUG: Processing new issue #%d from repo %s", issueNumber, repoName)
                go handleIssue(ctx, gh, bq, cfg, evt)
            } else {
                log.Printf("DEBUG: Ignoring issues event with action: %s", action)
            }
        } else {
            log.Printf("DEBUG: Ignoring non-issues event type: %T", event)
        }
        
        w.WriteHeader(http.StatusAccepted)
        log.Printf("DEBUG: Webhook request processed successfully")
    })

    port := cfg.Server.Port
    if envPort := os.Getenv("PORT"); envPort != "" {
        log.Printf("DEBUG: Found PORT environment variable: %s", envPort)
        if p, err := strconv.Atoi(envPort); err == nil { 
            port = p 
            log.Printf("DEBUG: Using port from environment variable: %d", port)
        } else {
            log.Printf("DEBUG: Invalid PORT environment variable, using config value: %d", port)
        }
    }
    
    log.Printf("INFO: DupRadar listening on :%d%s", port, cfg.Server.Path)
    log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", port), nil))
}

func handleIssue(ctx context.Context, gh *github.Client, bq *bqClient, cfg *Config, evt *github.IssuesEvent) {
    repoFull := evt.GetRepo().GetFullName()
    issue := evt.GetIssue()
    issueNumber := issue.GetNumber()
    log.Printf("DEBUG: Processing issue #%d from repo %s", issueNumber, repoFull)
    
    text := issue.GetTitle() + "\n" + issue.GetBody()
    textLength := len(text)
    log.Printf("DEBUG: Combined text length for embedding: %d characters", textLength)

    // 1) Embed
    log.Printf("DEBUG: [Issue #%d] Creating text embedding", issueNumber)
    vec, err := embedText(ctx, cfg, text)
    if err != nil { 
        log.Printf("ERROR: Failed to create embedding for issue #%d: %v", issueNumber, err)
        return 
    }
    log.Printf("DEBUG: [Issue #%d] Successfully created embedding with %d dimensions", issueNumber, len(vec))

    // 2) Search similar
    log.Printf("DEBUG: [Issue #%d] Searching for similar issues (top %d)", issueNumber, cfg.GitHub.TopK)
    ids, dists, err := bq.searchSimilar(ctx, vec, cfg.GitHub.TopK)
    if err != nil { 
        log.Printf("ERROR: BigQuery search failed for issue #%d: %v", issueNumber, err)
    } else {
        log.Printf("DEBUG: [Issue #%d] Found %d similar issues", issueNumber, len(ids))
        for i := 0; i < len(ids) && i < len(dists); i++ {
            log.Printf("DEBUG: [Issue #%d] Similar issue #%d with distance %.4f", issueNumber, ids[i], dists[i])
        }
    }

    // 3) Comment if similar found
    log.Printf("DEBUG: [Issue #%d] Building comment with similarity threshold %.4f", issueNumber, cfg.GitHub.Similarity)
    if msg := buildComment(cfg, ids, dists); msg != "" {
        owner := evt.GetRepo().GetOwner().GetLogin()
        repo := evt.GetRepo().GetName()
        log.Printf("DEBUG: [Issue #%d] Posting comment to %s/%s#%d", issueNumber, owner, repo, issueNumber)
        _, _, err := gh.Issues.CreateComment(ctx, owner, repo, issue.GetNumber(), &github.IssueComment{Body: &msg})
        if err != nil { 
            log.Printf("ERROR: Failed to create comment on issue #%d: %v", issueNumber, err)
        } else {
            log.Printf("DEBUG: [Issue #%d] Successfully posted comment", issueNumber)
        }
    } else {
        log.Printf("DEBUG: [Issue #%d] No similar issues found above threshold, skipping comment", issueNumber)
    }

    // 4) Insert vector
    log.Printf("DEBUG: [Issue #%d] Storing embedding in BigQuery", issueNumber)
    if err := bq.insertVector(ctx, issue, repoFull, vec); err != nil {
        log.Printf("ERROR: Failed to insert vector for issue #%d: %v", issueNumber, err)
    } else {
        log.Printf("DEBUG: [Issue #%d] Successfully stored embedding in BigQuery", issueNumber)
    }
    
    log.Printf("DEBUG: [Issue #%d] Processing completed", issueNumber)
}

func buildComment(cfg *Config, ids []int64, dists []float64) string {
    log.Printf("DEBUG: Building comment with %d issue IDs and %d distances", len(ids), len(dists))
    
    if len(ids) == 0 {
        log.Printf("DEBUG: No similar issues found, returning empty comment")
        return "" // no similar issues
    }
    
    if len(dists) > 0 && dists[0] > cfg.GitHub.Similarity {
        log.Printf("DEBUG: Most similar issue has distance %.4f which is above threshold %.4f, returning empty comment", 
                  dists[0], cfg.GitHub.Similarity)
        return "" // not similar enough
    }
    
    log.Printf("DEBUG: Found similar issues below threshold, creating comment")
    var sb strings.Builder
    sb.WriteString("### 🤖 類似 Issue 候補\n\n")
    
    issuesIncluded := 0
    for i, id := range ids {
        if i >= len(dists) || dists[i] > cfg.GitHub.Similarity {
            log.Printf("DEBUG: Stopping at issue #%d with distance %.4f (above threshold %.4f)", 
                      id, dists[i], cfg.GitHub.Similarity)
            break
        }
        sb.WriteString(fmt.Sprintf("* #%d (距離 %.3f)\n", id, dists[i]))
        issuesIncluded++
        log.Printf("DEBUG: Added issue #%d with distance %.4f to comment", id, dists[i])
    }
    
    sb.WriteString("\n_Comment generated by DupRadar_\n")
    log.Printf("DEBUG: Created comment with %d similar issues", issuesIncluded)
    
    return sb.String()
}
