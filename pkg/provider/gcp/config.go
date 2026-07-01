// Copyright 2024 SandrPod
// GCP Provider configuration

package gcp

import (
	"os"
)

// Config holds Google Cloud placement and auth settings.
//
// GCP has no managed run-command service (unlike AWS SSM / Aliyun CloudAssist /
// Azure Run Command), so remote execution goes over SSH. That requires the VM
// to have a public IP and a firewall rule allowing the server to reach port 22
// — target it with the network tag applied to every SandrPod VM ("sandrpod").
type Config struct {
	Project       string // GCP project ID
	Zone          string // Default zone, e.g. "us-central1-a" (GCP is zonal)
	Network       string // Fallback network when no subnet is given
	AdminUsername string // Linux user created via SSH-key metadata
	// CredentialsFile is an optional service-account JSON path. When empty the
	// client uses Application Default Credentials (GOOGLE_APPLICATION_CREDENTIALS
	// or the metadata server).
	CredentialsFile string
}

// LoadConfig loads configuration from environment variables.
func LoadConfig() *Config {
	return &Config{
		Project:         os.Getenv("GCP_PROJECT"),
		Zone:            getEnv("GCP_ZONE", "us-central1-a"),
		Network:         getEnv("GCP_NETWORK", "global/networks/default"),
		AdminUsername:   getEnv("GCP_ADMIN_USERNAME", "sandrpod"),
		CredentialsFile: os.Getenv("GCP_CREDENTIALS_FILE"),
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
