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
	log.Printf("DEBUG: Building BigQuery vector search query (topK=%d)", topK)

	// Using BigQuery vector search capabilities with correct syntax per docs
	q := b.client.Query(fmt.Sprintf(`
        SELECT issue_id, 
        VECTOR_SEARCH_DISTANCE(embedding, @query_vec) AS dist
        FROM `+"`%s.%s.%s`"+`
        WHERE VECTOR_SEARCH_COSINE_DISTANCE(embedding, @query_vec) <= 2.0
        ORDER BY dist
        LIMIT %d`,
		b.cfg.GCP.ProjectID, b.cfg.GCP.BQDataset, b.cfg.GCP.BQTable, topK))

	log.Printf("DEBUG: Using query parameters with vector of %d dimensions", len(vec))
	q.Parameters = []bigquery.QueryParameter{{Name: "query_vec", Value: vec}}

	log.Printf("DEBUG: Executing BigQuery vector search")
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

// EnsureTableWithVectorSearch creates or updates the BigQuery table with vector search capabilities
func (b *BQClient) EnsureTableWithVectorSearch(ctx context.Context) error {
	log.Printf("DEBUG: Ensuring BigQuery table exists with vector search capabilities")
	
	// Get reference to dataset and table
	dataset := b.client.Dataset(b.cfg.GCP.BQDataset)
	
	// Check if dataset exists, if not create it
	datasetMeta, err := dataset.Metadata(ctx)
	if err != nil {
		log.Printf("DEBUG: Creating new dataset %s", b.cfg.GCP.BQDataset)
		if err := dataset.Create(ctx, &bigquery.DatasetMetadata{
			Name:        b.cfg.GCP.BQDataset,
			Description: "Dataset for dup-radar issue embeddings",
			Location:    b.cfg.GCP.Region,
		}); err != nil {
			log.Printf("ERROR: Failed to create dataset: %v", err)
			return fmt.Errorf("failed to create dataset: %w", err)
		}
	} else {
		log.Printf("DEBUG: Dataset %s already exists in location %s", datasetMeta.Name, datasetMeta.Location)
	}
	
	// Check if table exists by attempting to get its metadata
	table := dataset.Table(b.cfg.GCP.BQTable)
	_, err = table.Metadata(ctx)
	
	if err != nil {
		log.Printf("DEBUG: Table %s does not exist, creating with vector search capabilities", b.cfg.GCP.BQTable)
		
		// Use SQL DDL to create table with vector search capabilities instead of struct-based API
		// This is compatible with all versions of the BigQuery Go client
		createTableSQL := fmt.Sprintf(`
			CREATE TABLE IF NOT EXISTS `+"`%s.%s.%s`"+` (
				repo STRING NOT NULL,
				issue_id INT64 NOT NULL,
				title STRING,
				body STRING,
				created_at TIMESTAMP,
				embedding ARRAY<FLOAT64>
			)
			OPTIONS (
				description = "Table for issue embeddings with vector search"
			);
		`, b.cfg.GCP.ProjectID, b.cfg.GCP.BQDataset, b.cfg.GCP.BQTable)
		
		log.Printf("DEBUG: Executing CREATE TABLE DDL statement")
		q := b.client.Query(createTableSQL)
		q.Location = b.cfg.GCP.Region
		
		job, err := q.Run(ctx)
		if err != nil {
			log.Printf("ERROR: Failed to create table: %v", err)
			return fmt.Errorf("failed to create table: %w", err)
		}
		
		status, err := job.Wait(ctx)
		if err != nil {
			log.Printf("ERROR: Failed to wait for table creation job: %v", err)
			return fmt.Errorf("failed to wait for table creation job: %w", err)
		}
		
		if status.Err() != nil {
			log.Printf("ERROR: Table creation job failed: %v", status.Err())
			return fmt.Errorf("table creation job failed: %w", status.Err())
		}
		
		// Create vector index on the embedding column
		createVectorIndexSQL := fmt.Sprintf(`
			CREATE VECTOR INDEX vector_index_embedding
			ON `+"`%s.%s.%s`"+`(embedding)
			OPTIONS (
				index_type = 'IVF',
				distance_type = '%s',
				dimensions = %d,
				leaf_nodes_to_search_percent = 10
			);
		`, b.cfg.GCP.ProjectID, b.cfg.GCP.BQDataset, b.cfg.GCP.BQTable, 
		   b.cfg.GCP.VectorSearch.Distance, b.cfg.GCP.VectorSearch.Dimensions)
		
		log.Printf("DEBUG: Executing CREATE VECTOR INDEX DDL statement")
		q = b.client.Query(createVectorIndexSQL)
		q.Location = b.cfg.GCP.Region
		
		job, err = q.Run(ctx)
		if err != nil {
			log.Printf("ERROR: Failed to create vector index: %v", err)
			return fmt.Errorf("failed to create vector index: %w", err)
		}
		
		status, err = job.Wait(ctx)
		if err != nil {
			log.Printf("ERROR: Failed to wait for vector index creation job: %v", err)
			return fmt.Errorf("failed to wait for vector index creation job: %w", err)
		}
		
		if status.Err() != nil {
			log.Printf("ERROR: Vector index creation job failed: %v", status.Err())
			return fmt.Errorf("vector index creation job failed: %w", status.Err())
		}
		
		log.Printf("DEBUG: Successfully created table %s with vector search index", b.cfg.GCP.BQTable)
	} else {
		log.Printf("DEBUG: Table %s already exists, ensuring it has vector search index", b.cfg.GCP.BQTable)
		
		// Check if vector index exists and create it if not
		checkVectorIndexSQL := fmt.Sprintf(`
			SELECT index_name
			FROM `+"`%s.%s.INFORMATION_SCHEMA.VECTOR_INDEXES`"+`
			WHERE table_name = '%s' AND index_name = 'vector_index_embedding'
		`, b.cfg.GCP.ProjectID, b.cfg.GCP.BQDataset, b.cfg.GCP.BQTable)
		
		q := b.client.Query(checkVectorIndexSQL)
		job, err := q.Run(ctx)
		if err != nil {
			log.Printf("ERROR: Failed to check for vector index: %v", err)
			return fmt.Errorf("failed to check for vector index: %w", err)
		}
		
		it, err := job.Read(ctx)
		if err != nil {
			log.Printf("ERROR: Failed to read vector index check results: %v", err)
			return fmt.Errorf("failed to read vector index check results: %w", err)
		}
		
		var row struct {
			IndexName string `bigquery:"index_name"`
		}
		
		if err := it.Next(&row); err == iterator.Done {
			// Vector index doesn't exist, create it
			log.Printf("DEBUG: Vector index not found, creating it now")
			createVectorIndexSQL := fmt.Sprintf(`
				CREATE VECTOR INDEX vector_index_embedding
				ON `+"`%s.%s.%s`"+`(embedding)
				OPTIONS (
					index_type = 'IVF',
					distance_type = '%s',
					dimensions = %d,
					leaf_nodes_to_search_percent = 10
				);
			`, b.cfg.GCP.ProjectID, b.cfg.GCP.BQDataset, b.cfg.GCP.BQTable,
			   b.cfg.GCP.VectorSearch.Distance, b.cfg.GCP.VectorSearch.Dimensions)
			
			q = b.client.Query(createVectorIndexSQL)
			q.Location = b.cfg.GCP.Region
			
			job, err = q.Run(ctx)
			if err != nil {
				log.Printf("ERROR: Failed to create vector index: %v", err)
				return fmt.Errorf("failed to create vector index: %w", err)
			}
			
			status, err := job.Wait(ctx)
			if err != nil {
				log.Printf("ERROR: Failed to wait for vector index creation job: %v", err)
				return fmt.Errorf("failed to wait for vector index creation job: %w", err)
			}
			
			if status.Err() != nil {
				log.Printf("ERROR: Vector index creation job failed: %v", status.Err())
				return fmt.Errorf("vector index creation job failed: %w", status.Err())
			}
			
			log.Printf("DEBUG: Vector index created successfully")
		} else if err != nil {
			log.Printf("ERROR: Failed to check if vector index exists: %v", err)
			return fmt.Errorf("failed to check if vector index exists: %w", err)
		} else {
			log.Printf("DEBUG: Vector index %s already exists on table", row.IndexName)
		}
	}
	
	return nil
}
