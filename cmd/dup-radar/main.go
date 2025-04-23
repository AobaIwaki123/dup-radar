package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/google/go-github/v62/github"
	"github.com/joho/godotenv"
	"golang.org/x/oauth2"
	"gopkg.in/yaml.v3"
)

// ----------------------------
// Configuration structures
// ----------------------------

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

func loadConfig(path string) Config {
    f, err := os.Open(path)
    if err != nil {
        log.Fatalf("config open: %v", err)
    }
    defer f.Close()
    var cfg Config
    if err := yaml.NewDecoder(f).Decode(&cfg); err != nil {
        log.Fatalf("config decode: %v", err)
    }
    return cfg
}

// ----------------------------
// Webhook handler
// ----------------------------

var cfg Config

func main() {
    // Load .env if present (local dev convenience)
    _ = godotenv.Load()

    cfg = loadConfig("configs/config.yaml")

    http.HandleFunc(cfg.Server.Path, webhookHandler)

    addr := fmt.Sprintf(":%d", cfg.Server.Port)
    log.Printf("DupRadar listening on %s%s", addr, cfg.Server.Path)
    if err := http.ListenAndServe(addr, nil); err != nil {
        log.Fatal(err)
    }
}

func webhookHandler(w http.ResponseWriter, r *http.Request) {
    // Verify HMAC signature
    secret := os.Getenv("GITHUB_WEBHOOK_SECRET")
    if !verifySignature(r, secret) {
        http.Error(w, "signature mismatch", http.StatusUnauthorized)
        return
    }

    body, _ := io.ReadAll(r.Body)
    eventType := github.WebHookType(r)
    event, err := github.ParseWebHook(eventType, body)
    if err != nil {
        http.Error(w, "parse error", 400)
        return
    }

    if ie, ok := event.(*github.IssuesEvent); ok && ie.GetAction() == "opened" {
        go processIssue(ie)
    }

    w.WriteHeader(http.StatusAccepted)
}

func verifySignature(r *http.Request, secret string) bool {
    sig := strings.TrimPrefix(r.Header.Get("X-Hub-Signature-256"), "sha256=")
    if sig == "" || secret == "" {
        return false
    }
    body, _ := io.ReadAll(r.Body)
    r.Body = io.NopCloser(strings.NewReader(string(body))) // restore
    mac := hmac.New(sha256.New, []byte(secret))
    mac.Write(body)
    expected := hex.EncodeToString(mac.Sum(nil))
    return hmac.Equal([]byte(sig), []byte(expected))
}

// ----------------------------
// Core logic (MVP stubs)
// ----------------------------

func processIssue(evt *github.IssuesEvent) {
    ctx := context.Background()

    issue := evt.GetIssue()
    repo := evt.GetRepo()

    text := issue.GetTitle() + "\n" + issue.GetBody()

    // 1. Get embedding vector (Vertex AI) –– MVP uses dummy vector
    vec := dummyEmbed(text)

    // 2. Search BigQuery for similar issues –– returns dummy result
    similarIDs := dummySearch(vec, cfg.GitHub.TopK)

    // 3. If similarity lower (distance) than threshold → comment
    if len(similarIDs) > 0 {
        commentBody := buildComment(similarIDs)
        client := githubClient(ctx)
        _, _, err := client.Issues.CreateComment(ctx, repo.GetOwner().GetLogin(), repo.GetName(), issue.GetNumber(), &github.IssueComment{Body: github.String(commentBody)})
        if err != nil {
            log.Printf("comment error: %v", err)
        }
    }

    // 4. Insert the new vector to BigQuery (stub)
    _ = vec // TODO: implement BQ INSERT
}

func buildComment(ids []int64) string {
    var b strings.Builder
    b.WriteString("### 🔍 似ている Issue が見つかりました\n\n")
    for _, id := range ids {
        b.WriteString(fmt.Sprintf("* #%d\n", id))
    }
    b.WriteString("\n_(自動生成)_")
    return b.String()
}

// ----------------------------
// GitHub client
// ----------------------------

func githubClient(ctx context.Context) *github.Client {
    if pat := os.Getenv("GITHUB_PAT"); pat != "" {
        ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: pat})
        return github.NewClient(oauth2.NewClient(ctx, ts))
    }
    // GitHub App flow省略 (PATが推奨)
    log.Fatal("no GITHUB_PAT provided")
    return nil
}

// ----------------------------
// Dummy stubs – replace with real API calls
// ----------------------------

func dummyEmbed(text string) []float32 {
    // Replace with Vertex AI REST call
    return []float32{0.1, 0.2, 0.3}
}

func dummySearch(vec []float32, k int) []int64 {
    // Replace with BigQuery VECTOR_SEARCH; return Issue IDs for demo
    return []int64{1, 42}
}
