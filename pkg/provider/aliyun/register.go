// Copyright 2024 SandrPod
// Aliyun Provider registration

package aliyun

import (
	"github.com/sandrpod/sandrpod/pkg/provider"
)

// Register registers the Aliyun Provider with the global factory.
func Register() error {
	cfg := LoadConfig()
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil // Aliyun not configured, skipping registration
	}

	p, err := NewAliyunProvider(cfg)
	if err != nil {
		return err
	}

	return provider.GetFactory().Register(p)
}
