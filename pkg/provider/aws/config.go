// Copyright 2024 SandrPod
// AWS Provider 配置

package aws

import (
	"os"
)

// LoadConfig 从环境变量加载配置
func LoadConfig() *Config {
	return &Config{
		Region:    getEnv("AWS_REGION", "us-east-1"),
		AccessKey: os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
