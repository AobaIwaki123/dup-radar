package main

// DupRadar – minimal but functional MVP
// -------------------------------------
// - Receives GitHub issues.opened webhooks (HMAC‑SHA256 verified)
// - Creates an embedding with Vertex AI text‑embedding‑005 (API Key or ADC)
// - Searches BigQuery VECTOR_SEARCH() for similar issues
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
    f, err := os.Open(path)
    if err != nil { log.Fatalf("open config: %v", err) }
    defer f.Close()
    var c Config
    if err := yaml.NewDecoder(f).Decode(&c); err != nil {
        log.Fatalf("parse yaml: %v", err)
    }
    return &c
}

// ---------------- GitHub client ----------------

func newGitHubClient(ctx context.Context) *github.Client {
    pat := os.Getenv("GITHUB_PAT")
    if pat == "" {
        log.Fatalf("GITHUB_PAT is not set")
    }
    ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: pat})
    tc := oauth2.NewClient(ctx, ts)
    return github.NewClient(tc)
}

// ---------------- Vertex AI embedding ----------------

func embedText(ctx context.Context, cfg *Config, text string) ([]float64, error) {
    endpoint := fmt.Sprintf("https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/google/models/%s:predict",
        strings.ToLower(cfg.GCP.Region), cfg.GCP.ProjectID, strings.ToLower(cfg.GCP.Region), cfg.GCP.EmbeddingModel)

    reqBody := map[string]any{
        "instances": []map[string]string{{"content": text}},
    }
    body, _ := json.Marshal(reqBody)

    req, _ := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
    req.Header.Set("Content-Type", "application/json")
    if key := os.Getenv("VERTEX_API_KEY"); key != "" {
        req.Header.Set("Authorization", "Bearer " + key)
    }

    resp, err := http.DefaultClient.Do(req)
    if err != nil { return nil, err }
    defer resp.Body.Close()
    if resp.StatusCode != 200 {
        b, _ := io.ReadAll(resp.Body)
        return nil, fmt.Errorf("vertex api error: %s: %s", resp.Status, string(b))
    }
    var out struct {
        Predictions [][]float64 `json:"predictions"`
    }
    if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
        return nil, err
    }
    return out.Predictions[0], nil
}

// ---------------- BigQuery helpers ----------------

type bqClient struct {
    client *bigquery.Client
    cfg    *Config
}

func newBQ(ctx context.Context, cfg *Config) *bqClient {
    cli, err := bigquery.NewClient(ctx, cfg.GCP.ProjectID)
    if err != nil { log.Fatalf("bigquery client: %v", err) }
    return &bqClient{client: cli, cfg: cfg}
}

func (b *bqClient) searchSimilar(ctx context.Context, vec []float64, topK int) ([]int64, []float64, error) {
    // Build SQL with VECTOR_SEARCH
    param := &bigquery.QueryParameter{Name: "query_vec", Value: vec}
    q := b.client.Query(fmt.Sprintf(`SELECT issue_id, repo,
        VECTOR_DISTANCE(embedding,@query_vec) AS dist
        FROM %s.%s.%s
        ORDER BY dist
        LIMIT %d`, b.cfg.GCP.ProjectID, b.cfg.GCP.Dataset, b.cfg.GCP.Table, topK))
    q.Parameters = []bigquery.QueryParameter{*param}
    it, err := q.Read(ctx)
    if err != nil { return nil, nil, err }
    var ids []int64
    var dists []float64
    for {
        var row struct {
            IssueID int64   `bigquery:"issue_id"`
            Dist    float64 `bigquery:"dist"`
        }
        err := it.Next(&row)
        if err == iterator.Done { break }
        if err != nil { return nil, nil, err }
        ids = append(ids, row.IssueID)
        dists = append(dists, row.Dist)
    }
    return ids, dists, nil
}

func (b *bqClient) insertVector(ctx context.Context, issue *github.Issue, repo string, vec []float64) error {
    u := b.client.Dataset(b.cfg.GCP.Dataset).Table(b.cfg.GCP.Table).Inserter()
    item := map[string]any{
        "repo":       repo,
        "issue_id":   issue.GetNumber(),
        "title":      issue.GetTitle(),
        "body":       issue.GetBody(),
        "created_at": issue.GetCreatedAt(),
        "embedding":  vec,
    }
    return u.Put(ctx, item)
}

// ---------------- Webhook handler ----------------

func main() {
    _ = godotenv.Load() // ignore error if .env absent
    cfg := loadConfig("configs/config.yaml")

    ctx := context.Background()
    gh := newGitHubClient(ctx)
    bq := newBQ(ctx, cfg)

    secret := os.Getenv("GITHUB_WEBHOOK_SECRET")
    if secret == "" { log.Fatal("GITHUB_WEBHOOK_SECRET not set") }
    sigKey := []byte(secret)

    http.HandleFunc(cfg.Server.Path, func(w http.ResponseWriter, r *http.Request) {
        payload, err := io.ReadAll(r.Body)
        if err != nil { http.Error(w, "read", 400); return }
        // signature header check
        sig := strings.TrimPrefix(r.Header.Get("X-Hub-Signature-256"), "sha256=")
        mac := hmac.New(sha256.New, sigKey)
        mac.Write(payload)
        expected := hex.EncodeToString(mac.Sum(nil))
        if !hmac.Equal([]byte(sig), []byte(expected)) {
            http.Error(w, "invalid signature", 401)
            return
        }

        event, err := github.ParseWebHook(github.WebHookType(r), payload)
        if err != nil { http.Error(w, "parse", 400); return }
        if evt, ok := event.(*github.IssuesEvent); ok && evt.GetAction() == "opened" {
            go handleIssue(ctx, gh, bq, cfg, evt)
        }
        w.WriteHeader(http.StatusAccepted)
    })

    port := cfg.Server.Port
    if envPort := os.Getenv("PORT"); envPort != "" {
        if p, err := strconv.Atoi(envPort); err == nil { port = p }
    }
    log.Printf("DupRadar listening on :%d%s", port, cfg.Server.Path)
    log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", port), nil))
}

func handleIssue(ctx context.Context, gh *github.Client, bq *bqClient, cfg *Config, evt *github.IssuesEvent) {
    repoFull := evt.GetRepo().GetFullName() // owner/repo
    issue    := evt.GetIssue()
    text := issue.GetTitle() + "\n" + issue.GetBody()

    vec, err := embedText(ctx, cfg, text)
    if err != nil { log.Printf("embed error: %v", err); return }

    ids, dists, err := bq.searchSimilar(ctx, vec, cfg.GitHub.TopK)
    if err != nil { log.Printf("bq search: %v", err) }

    message := buildComment(cfg, ids, dists, repoFull)
    if message != "" {
        _, _, err := gh.Issues.CreateComment(ctx, evt.GetRepo().GetOwner().GetLogin(), evt.GetRepo().GetName(), issue.GetNumber(), &github.IssueComment{Body: &message})
        if err != nil { log.Printf("comment error: %v", err) }
    }

    if err := bq.insertVector(ctx, issue, repoFull, vec); err != nil {
        log.Printf("insert bq: %v", err)
    }
}

func buildComment(cfg *Config, ids []int64, dists []float64, repo string) string {
    if len(ids) == 0 || (len(dists) > 0 && dists[0] > cfg.GitHub.Similarity) {
        return "" // 類似なし
    }
    var sb strings.Builder
    sb.WriteString("### 🤖 類似 Issue 候補\n\n")
    for i, id := range ids {
        if i >= len(dists) || dists[i] > cfg.GitHub.Similarity { break }
        sb.WriteString(fmt.Sprintf("* #%d (dist %.3f)\n", id, dists[i]))
    }
    sb.WriteString("\n_This comment was generated by DupRadar_\n")
    return sb.String()
}
