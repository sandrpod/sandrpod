// Copyright 2024 SandrPod
// DigitalOcean Provider configuration

package digitalocean

import "os"

// Config holds DigitalOcean credentials and default placement.
type Config struct {
	Token     string // DigitalOcean API token
	Region    string // Default region slug, e.g. "nyc3"
	SSHKeyDir string // Directory to persist per-VM SSH keys (empty = memory-only)
}

// LoadConfig loads configuration from environment variables. The token is read
// from DIGITALOCEAN_TOKEN (the SDK's convention), falling back to DO_TOKEN.
func LoadConfig() *Config {
	token := os.Getenv("DIGITALOCEAN_TOKEN")
	if token == "" {
		token = os.Getenv("DO_TOKEN")
	}
	return &Config{
		Token:     token,
		Region:    getEnv("DO_REGION", "nyc3"),
		SSHKeyDir: os.Getenv("SANDRPOD_SSH_KEY_DIR"),
	}
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
