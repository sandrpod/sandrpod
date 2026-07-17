// Copyright 2026 SandrPod Contributors
// AWS Provider registration

package aws

import (
	"github.com/sandrpod/sandrpod/pkg/provider"
)

// Register registers the AWS Provider with the global factory.
func Register() error {
	cfg := LoadConfig()

	p, err := NewAWSProvider(cfg)
	if err != nil {
		return err
	}

	return provider.GetFactory().Register(p)
}
