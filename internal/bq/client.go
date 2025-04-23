package bq

import (
    "context"
    "cloud.google.com/go/bigquery"
    "fmt"
    "log"
)

type Client struct {
    bqClient *bigquery.Client
}

func NewClient(projectID string) (*Client, error) {
    ctx := context.Background()
    client, err := bigquery.NewClient(ctx, projectID)
    if err != nil {
        return nil, fmt.Errorf("failed to create BigQuery client: %v", err)
    }
    return &Client{bqClient: client}, nil
}

func (c *Client) InsertData(datasetID, tableID string, rows interface{}) error {
    ctx := context.Background()
    inserter := c.bqClient.Dataset(datasetID).Table(tableID).Inserter()
    if err := inserter.Put(ctx, rows); err != nil {
        return fmt.Errorf("failed to insert data: %v", err)
    }
    return nil
}

func (c *Client) QueryData(query string) (*bigquery.RowIterator, error) {
    ctx := context.Background()
    q := c.bqClient.Query(query)
    return q.Read(ctx)
}