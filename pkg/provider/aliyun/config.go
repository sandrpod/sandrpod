// Copyright 2026 SandrPod Contributors
// Aliyun Provider configuration

package aliyun

import (
	"os"
)

// LoadConfig loads configuration from environment variables.
func LoadConfig() *Config {
	return &Config{
		Region:    getEnv("ALIYUN_REGION", "cn-hangzhou"),
		AccessKey: os.Getenv("ALIYUN_ACCESS_KEY"),
		SecretKey: os.Getenv("ALIYUN_SECRET_KEY"),
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
