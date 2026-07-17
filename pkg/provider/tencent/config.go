// Copyright 2026 SandrPod Contributors
// Tencent Cloud Provider configuration

package tencent

import "os"

// Config holds Tencent Cloud credentials and default placement.
type Config struct {
	SecretID  string // TencentCloud API SecretId
	SecretKey string // TencentCloud API SecretKey
	Region    string // Region, e.g. "ap-guangzhou" (the client is bound to it)
	Zone      string // Default availability zone, e.g. "ap-guangzhou-3"
}

// LoadConfig loads configuration from environment variables.
func LoadConfig() *Config {
	return &Config{
		SecretID:  os.Getenv("TENCENTCLOUD_SECRET_ID"),
		SecretKey: os.Getenv("TENCENTCLOUD_SECRET_KEY"),
		Region:    getEnv("TENCENTCLOUD_REGION", "ap-guangzhou"),
		Zone:      getEnv("TENCENTCLOUD_ZONE", "ap-guangzhou-3"),
	}
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
