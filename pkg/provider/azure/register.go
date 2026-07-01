// Copyright 2024 SandrPod
// Azure Provider registration

package azure

import (
	"github.com/sandrpod/sandrpod/pkg/provider"
)

// Register registers the Azure Provider with the global factory.
//
// Like the Aliyun provider, registration is skipped (not an error) when the
// required credentials/placement are absent, so a server without Azure
// configured simply doesn't expose the provider rather than exposing a broken
// one that fails on first use.
func Register() error {
	cfg := LoadConfig()
	if cfg.SubscriptionID == "" || cfg.TenantID == "" || cfg.ClientID == "" ||
		cfg.ClientSecret == "" || cfg.ResourceGroup == "" {
		return nil // Azure not configured, skip registration
	}

	p, err := NewAzureProvider(cfg)
	if err != nil {
		return err
	}

	return provider.GetFactory().Register(p)
}
