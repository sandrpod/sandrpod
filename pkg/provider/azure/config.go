// Copyright 2024 SandrPod
// Azure Provider configuration

package azure

import (
	"os"
)

// Config holds Azure credentials and default placement settings.
//
// Azure needs more than a region + key pair: a subscription, a service
// principal (tenant/client/secret), and a pre-existing resource group that new
// VMs (and their per-VM NIC/public-IP) are created into.
type Config struct {
	SubscriptionID string // Azure subscription ID
	TenantID       string // Entra tenant ID (service principal)
	ClientID       string // Service principal application (client) ID
	ClientSecret   string // Service principal secret
	Location       string // Default region, e.g. "eastus"
	ResourceGroup  string // Pre-existing resource group new VMs land in
	AdminUsername  string // Linux admin user created on each VM
	// SSHPublicKey, when set, is installed for AdminUsername and password auth
	// is disabled. When empty, a throwaway strong password is generated to
	// satisfy Azure's mandatory auth requirement — it is never used, since all
	// remote execution goes through the VM agent (Run Command), not SSH.
	SSHPublicKey string
}

// LoadConfig loads configuration from environment variables.
func LoadConfig() *Config {
	return &Config{
		SubscriptionID: os.Getenv("AZURE_SUBSCRIPTION_ID"),
		TenantID:       os.Getenv("AZURE_TENANT_ID"),
		ClientID:       os.Getenv("AZURE_CLIENT_ID"),
		ClientSecret:   os.Getenv("AZURE_CLIENT_SECRET"),
		Location:       getEnv("AZURE_LOCATION", "eastus"),
		ResourceGroup:  os.Getenv("AZURE_RESOURCE_GROUP"),
		AdminUsername:  getEnv("AZURE_ADMIN_USERNAME", "sandrpod"),
		SSHPublicKey:   os.Getenv("AZURE_SSH_PUBLIC_KEY"),
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
