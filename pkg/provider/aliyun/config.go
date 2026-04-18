// Copyright 2024 SandrPod
// 阿里云 Provider 配置

package aliyun

import (
	"os"
)

// LoadConfig 从环境变量加载配置
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
