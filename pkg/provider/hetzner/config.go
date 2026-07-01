// Copyright 2024 SandrPod
// Hetzner Cloud Provider configuration

package hetzner

import "os"

// Config holds Hetzner Cloud credentials and default placement.
type Config struct {
	Token    string // Hetzner Cloud API token
	Location string // Default location, e.g. "fsn1"
}

// LoadConfig loads configuration from environment variables.
func LoadConfig() *Config {
	return &Config{
		Token:    os.Getenv("HCLOUD_TOKEN"),
		Location: getEnv("HCLOUD_LOCATION", "fsn1"),
	}
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
