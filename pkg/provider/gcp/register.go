// Copyright 2026 SandrPod Contributors
// GCP Provider registration

package gcp

import (
	"github.com/sandrpod/sandrpod/pkg/provider"
)

// Register registers the GCP Provider with the global factory.
//
// Registration is skipped (not an error) when no project is configured, so a
// server without GCP configured simply doesn't expose the provider.
func Register() error {
	cfg := LoadConfig()
	if cfg.Project == "" {
		return nil // GCP not configured, skip registration
	}

	p, err := NewGCPProvider(cfg)
	if err != nil {
		return err
	}

	return provider.GetFactory().Register(p)
}
