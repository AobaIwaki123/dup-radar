// internal/config/config.go
package config

import (
	"log"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server struct {
		Port int    `yaml:"port"`
		Path string `yaml:"path"`
	}
	GitHub struct {
		Similarity float64 `yaml:"similarity_threshold"`
		TopK       int     `yaml:"top_k"`
	}
	GCP struct {
		ProjectID      string `yaml:"project_id"`
		BQDataset      string `yaml:"bq_dataset"`
		BQTable        string `yaml:"bq_table"`
		Region         string `yaml:"region"`
		EmbeddingModel string `yaml:"embedding_model"`
		VectorSearch   struct {
			Distance   string `yaml:"distance_type"` // COSINE, DOT_PRODUCT, or EUCLIDEAN
			Dimensions int    `yaml:"dimensions"`    // Vector dimensions (e.g., 768)
		} `yaml:"vector_search"`
	}
}

func Load(path string) *Config {
	f, err := os.Open(path)
	if err != nil {
		log.Fatalf("open config: %v", err)
	}
	defer f.Close()

	var c Config
	if err := yaml.NewDecoder(f).Decode(&c); err != nil {
		log.Fatalf("parse yaml: %v", err)
	}
	return &c
}
