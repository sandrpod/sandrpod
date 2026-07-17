// Copyright 2026 SandrPod Contributors
// Hetzner Cloud Provider registration

package hetzner

import "github.com/sandrpod/sandrpod/pkg/provider"

// Register registers the Hetzner Cloud Provider with the global factory.
// Registration is skipped (not an error) when no API token is configured.
func Register() error {
	cfg := LoadConfig()
	if cfg.Token == "" {
		return nil // Hetzner not configured, skip registration
	}
	p, err := NewHetznerProvider(cfg)
	if err != nil {
		return err
	}
	return provider.GetFactory().Register(p)
}
