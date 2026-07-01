// Copyright 2024 SandrPod
// DigitalOcean Provider registration

package digitalocean

import "github.com/sandrpod/sandrpod/pkg/provider"

// Register registers the DigitalOcean Provider with the global factory.
// Registration is skipped (not an error) when no API token is configured.
func Register() error {
	cfg := LoadConfig()
	if cfg.Token == "" {
		return nil // DigitalOcean not configured, skip registration
	}
	p, err := NewDOProvider(cfg)
	if err != nil {
		return err
	}
	return provider.GetFactory().Register(p)
}
