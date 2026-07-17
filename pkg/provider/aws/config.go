// Copyright 2026 SandrPod Contributors
// AWS Provider configuration

package aws

import (
	"os"
)

// LoadConfig loads configuration from environment variables.
func LoadConfig() *Config {
	return &Config{
		Region:    getEnv("AWS_REGION", "us-east-1"),
		AccessKey: os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		// IAM instance profile attached to launched VMs so SSM can run the
		// bootstrap commands on them. Required for the cloud-provisioning path.
		IAMInstanceProfile: os.Getenv("AWS_IAM_INSTANCE_PROFILE"),
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
