// Copyright 2024 SandrPod
// 阿里云 Provider 注册

package aliyun

import (
	"github.com/sandrpod/sandrpod/pkg/provider"
)

// Register 注册阿里云 Provider 到全局工厂
func Register() error {
	cfg := LoadConfig()
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil // 未配置阿里云，跳过注册
	}

	p, err := NewAliyunProvider(cfg)
	if err != nil {
		return err
	}

	return provider.GetFactory().Register(p)
}
