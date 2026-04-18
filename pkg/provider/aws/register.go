// Copyright 2024 SandrPod
// AWS Provider 注册

package aws

import (
	"github.com/sandrpod/sandrpod/pkg/provider"
)

// Register 注册 AWS Provider 到全局工厂
func Register() error {
	cfg := LoadConfig()

	p, err := NewAWSProvider(cfg)
	if err != nil {
		return err
	}

	return provider.GetFactory().Register(p)
}
