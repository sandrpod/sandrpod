// Copyright 2024 SandrPod
// Oracle Cloud Infrastructure (OCI) Provider registration

package oracle

import "github.com/sandrpod/sandrpod/pkg/provider"

// Register registers the OCI Provider with the global factory. Registration is
// skipped (not an error) when no compartment is configured.
func Register() error {
	cfg := LoadConfig()
	if cfg.CompartmentID == "" {
		return nil // OCI not configured, skip registration
	}
	p, err := NewOracleProvider(cfg)
	if err != nil {
		return err
	}
	return provider.GetFactory().Register(p)
}
