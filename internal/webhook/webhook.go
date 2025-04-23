// Package webhook provides functionality to handle GitHub webhooks
package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/AobaIwaki123/dup-radar/internal/config"
	"github.com/AobaIwaki123/dup-radar/internal/embedding"
	ghclient "github.com/AobaIwaki123/dup-radar/internal/github"
	"github.com/AobaIwaki123/dup-radar/internal/storage"
	githubapi "github.com/google/go-github/v62/github"
)

// Handler handles GitHub webhooks
type Handler struct {
	config     *config.Config
	ghClient   *ghclient.Client
	bqClient   *storage.BQClient
	signingKey []byte
}

// NewHandler creates a new webhook handler
func NewHandler(cfg *config.Config, gh *ghclient.Client, bq *storage.BQClient, secret string) *Handler {
	log.Printf("DEBUG: Creating webhook handler")
	return &Handler{
		config:     cfg,
		ghClient:   gh,
		bqClient:   bq,
		signingKey: []byte(secret),
	}
}

// HandleWebhook processes GitHub webhook requests
func (h *Handler) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	log.Printf("DEBUG: Received webhook request from %s %s", r.RemoteAddr, r.Method)
	
	// Only accept POST requests
	if r.Method != http.MethodPost {
		log.Printf("ERROR: Received non-POST request: %s", r.Method)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	// Read payload with a size limit to prevent memory exhaustion
	const maxSize = 5 * 1024 * 1024 // 5MB limit
	payloadReader := io.LimitReader(r.Body, maxSize)
	payload, err := io.ReadAll(payloadReader)
	if err != nil {
		log.Printf("ERROR: Failed to read request body: %v", err)
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	log.Printf("DEBUG: Read %d bytes from request body", len(payload))

	// Verify webhook signature
	sig := strings.TrimPrefix(r.Header.Get("X-Hub-Signature-256"), "sha256=")
	if sig == "" {
		log.Printf("ERROR: Missing X-Hub-Signature-256 header")
		http.Error(w, "Missing signature", http.StatusUnauthorized)
		return
	}
	
	log.Printf("DEBUG: Checking webhook signature: %s", sig)
	mac := hmac.New(sha256.New, h.signingKey)
	mac.Write(payload)
	expected := hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(sig), []byte(expected)) {
		log.Printf("ERROR: Invalid webhook signature received")
		http.Error(w, "Invalid signature", http.StatusUnauthorized)
		return
	}
	log.Printf("DEBUG: Webhook signature verified successfully")

	eventType := githubapi.WebHookType(r)
	log.Printf("DEBUG: Webhook event type: %s", eventType)

	event, err := githubapi.ParseWebHook(eventType, payload)
	if err != nil {
		log.Printf("ERROR: Failed to parse webhook payload: %v", err)
		http.Error(w, "Failed to parse webhook payload", http.StatusBadRequest)
		return
	}

	if evt, ok := event.(*githubapi.IssuesEvent); ok {
		action := evt.GetAction()
		log.Printf("DEBUG: Received issues event with action: %s", action)
		if action == "opened" {
			issueNumber := evt.GetIssue().GetNumber()
			repoName := evt.GetRepo().GetFullName()
			log.Printf("DEBUG: Processing new issue #%d from repo %s", issueNumber, repoName)
			
			// Use a background context for the goroutine instead of request context
			bgCtx := context.Background()
			go h.handleIssue(bgCtx, evt)
		} else {
			log.Printf("DEBUG: Ignoring issues event with action: %s", action)
		}
	} else {
		log.Printf("DEBUG: Ignoring non-issues event type: %T", event)
	}

	w.WriteHeader(http.StatusAccepted)
	log.Printf("DEBUG: Webhook request processed successfully")
}

// handleIssue processes new GitHub issues
func (h *Handler) handleIssue(ctx context.Context, evt *githubapi.IssuesEvent) {
	repoFull := evt.GetRepo().GetFullName()
	issue := evt.GetIssue()
	issueNumber := issue.GetNumber()
	log.Printf("DEBUG: Processing issue #%d from repo %s", issueNumber, repoFull)

	text := issue.GetTitle() + "\n" + issue.GetBody()
	textLength := len(text)
	log.Printf("DEBUG: Combined text length for embedding: %d characters", textLength)

	// 1) Embed
	log.Printf("DEBUG: [Issue #%d] Creating text embedding", issueNumber)
	vec, err := embedding.CreateEmbedding(ctx, h.config, text)
	if err != nil {
		log.Printf("ERROR: Failed to create embedding for issue #%d: %v", issueNumber, err)
		return
	}
	log.Printf("DEBUG: [Issue #%d] Successfully created embedding with %d dimensions", issueNumber, len(vec))

	// 2) Search similar
	log.Printf("DEBUG: [Issue #%d] Searching for similar issues (top %d)", issueNumber, h.config.GitHub.TopK)
	ids, dists, err := h.bqClient.SearchSimilarIssues(ctx, vec, h.config.GitHub.TopK)
	if err != nil {
		log.Printf("ERROR: BigQuery search failed for issue #%d: %v", issueNumber, err)
	} else {
		log.Printf("DEBUG: [Issue #%d] Found %d similar issues", issueNumber, len(ids))
		for i := 0; i < len(ids) && i < len(dists); i++ {
			log.Printf("DEBUG: [Issue #%d] Similar issue #%d with distance %.4f", issueNumber, ids[i], dists[i])
		}
	}

	// 3) Comment if similar found
	log.Printf("DEBUG: [Issue #%d] Building comment with similarity threshold %.4f", issueNumber, h.config.GitHub.Similarity)
	if msg := ghclient.BuildSimilarIssuesComment(h.config.GitHub.Similarity, ids, dists); msg != "" {
		owner := evt.GetRepo().GetOwner().GetLogin()
		repo := evt.GetRepo().GetName()
		log.Printf("DEBUG: [Issue #%d] Posting comment to %s/%s#%d", issueNumber, owner, repo, issueNumber)
		if err := h.ghClient.CreateIssueComment(ctx, owner, repo, issue.GetNumber(), msg); err != nil {
			log.Printf("ERROR: Failed to create comment on issue #%d: %v", issueNumber, err)
		} else {
			log.Printf("DEBUG: [Issue #%d] Successfully posted comment", issueNumber)
		}
	} else {
		log.Printf("DEBUG: [Issue #%d] No similar issues found above threshold, skipping comment", issueNumber)
	}

	// 4) Insert vector
	log.Printf("DEBUG: [Issue #%d] Storing embedding in BigQuery", issueNumber)
	if err := h.bqClient.InsertIssueVector(ctx, issue, repoFull, vec); err != nil {
		log.Printf("ERROR: Failed to store embedding for issue #%d: %v", issueNumber, err)
		return
	}
	log.Printf("DEBUG: [Issue #%d] Successfully stored embedding", issueNumber)
}
