// Copyright 2026 SandrPod Contributors
// Tencent Cloud Provider registration

package tencent

import "github.com/sandrpod/sandrpod/pkg/provider"

// Register registers the Tencent Cloud Provider with the global factory.
// Registration is skipped (not an error) when credentials are absent.
func Register() error {
	cfg := LoadConfig()
	if cfg.SecretID == "" || cfg.SecretKey == "" {
		return nil // Tencent Cloud not configured, skip registration
	}
	p, err := NewTencentProvider(cfg)
	if err != nil {
		return err
	}
	return provider.GetFactory().Register(p)
}
