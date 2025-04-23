package main

// DupRadar – minimal but functional MVP
// -------------------------------------
// - Receives GitHub issues.opened webhooks (HMAC‑SHA256 verified)
// - Creates an embedding with Vertex AI text‑embedding‑005 (API Key or ADC)
// - Searches BigQuery Vector Search for similar issues
// - Comments top‑k similar issues if distance below threshold
// - Stores the new issue vector back into BigQuery
//
// Env vars (see .env.example):
//   GITHUB_PAT                – Personal access token (classic or fine‑grained)
//   GITHUB_WEBHOOK_SECRET     – same secret as Webhook config
//   VERTEX_API_KEY            – public API key (or omit to use ADC)
//   GOOGLE_APPLICATION_CREDENTIALS – ADC JSON (if not using gcloud login)
//
// Config file: configs/config.yaml (see README)

import (
	"context"
	"log"
	"os"
	"strconv"

	"github.com/AobaIwaki123/dup-radar/internal/config"
	"github.com/AobaIwaki123/dup-radar/internal/github"
	"github.com/AobaIwaki123/dup-radar/internal/storage"
	"github.com/AobaIwaki123/dup-radar/internal/webhook"
	"github.com/joho/godotenv"
)

func main() {
	log.Printf("DEBUG: Starting DupRadar service")
	_ = godotenv.Load()
	log.Printf("DEBUG: Environment variables loaded from .env file")

	cfg := config.Load("configs/config.yaml")
	log.Printf("DEBUG: Configuration loaded successfully")

	ctx := context.Background()
	
	// Initialize clients
	ghClient := github.NewClient(ctx)
	bqClient := storage.NewBQClient(ctx, cfg)
	log.Printf("DEBUG: GitHub and BigQuery clients initialized")

	secret := os.Getenv("GITHUB_WEBHOOK_SECRET")
	if secret == "" {
		log.Fatal("ERROR: GITHUB_WEBHOOK_SECRET not set")
	}
	log.Printf("DEBUG: GitHub webhook secret found (length: %d)", len(secret))

	// Determine port
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

	// Setup and start server
	server := webhook.SetupServer(cfg, ghClient, bqClient, secret, port)
	log.Fatal(server.ListenAndServe())
}
