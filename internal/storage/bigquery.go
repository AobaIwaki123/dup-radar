// Package storage provides functionality to store and retrieve embeddings from BigQuery
package storage

import (
	"context"
	"fmt"
	"log"
	"time"

	"cloud.google.com/go/bigquery"
	"github.com/AobaIwaki123/dup-radar/internal/config"
	"github.com/google/go-github/v62/github"
	"google.golang.org/api/iterator"
)

// BQClient wraps BigQuery operations
type BQClient struct {
	client *bigquery.Client
	cfg    *config.Config
}

// NewBQClient creates a new BigQuery client
func NewBQClient(ctx context.Context, cfg *config.Config) *BQClient {
	log.Printf("DEBUG: Initializing BigQuery client for project %s", cfg.GCP.ProjectID)
	cli, err := bigquery.NewClient(ctx, cfg.GCP.ProjectID)
	if err != nil {
		log.Fatalf("ERROR: BigQuery client initialization failed: %v", err)
	}
	log.Printf("DEBUG: BigQuery client initialized successfully")
	return &BQClient{client: cli, cfg: cfg}
}

// SearchSimilarIssues searches for similar issues based on vector distance
func (b *BQClient) SearchSimilarIssues(ctx context.Context, vec []float64, topK int) ([]int64, []float64, error) {
	log.Printf("DEBUG: Building BigQuery similarity search query (topK=%d)", topK)

	q := b.client.Query(fmt.Sprintf(`SELECT issue_id,
        ML.DISTANCE(embedding, @query_vec, 'COSINE') AS dist
        FROM %s.%s.%s
        ORDER BY dist
        LIMIT %d`,
		b.cfg.GCP.ProjectID, b.cfg.GCP.BQDataset, b.cfg.GCP.BQTable, topK))

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

// IssueRow represents a BigQuery row for issue data
type IssueRow struct {
	Repo      string    `bigquery:"repo"`
	IssueID   int64     `bigquery:"issue_id"`
	Title     string    `bigquery:"title"`
	Body      string    `bigquery:"body"`
	CreatedAt time.Time `bigquery:"created_at"`
	Embedding []float64 `bigquery:"embedding"`
}

// InsertIssueVector stores issue data and its embedding vector into BigQuery
func (b *BQClient) InsertIssueVector(ctx context.Context, issue *github.Issue, repo string, vec []float64) error {
	log.Printf("DEBUG: Preparing to insert issue data into BigQuery table %s.%s", b.cfg.GCP.BQDataset, b.cfg.GCP.BQTable)
	ins := b.client.Dataset(b.cfg.GCP.BQDataset).Table(b.cfg.GCP.BQTable).Inserter()
	log.Printf("DEBUG: Creating issue row for repo=%s, issue_id=%d, title=%q",
		repo, issue.GetNumber(), issue.GetTitle())
	row := &IssueRow{
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
