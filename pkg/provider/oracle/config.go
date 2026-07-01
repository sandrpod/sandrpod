// Copyright 2024 SandrPod
// Oracle Cloud Infrastructure (OCI) Provider configuration

package oracle

import "os"

// Config holds OCI placement settings. Authentication itself comes from an OCI
// ConfigurationProvider (the ~/.oci/config file or OCI_* env), not from here.
type Config struct {
	CompartmentID      string // OCID of the compartment instances are created in
	AvailabilityDomain string // e.g. "Uocm:PHX-AD-1"
	ConfigFile         string // optional ~/.oci/config path; empty = SDK default
}

// LoadConfig loads configuration from environment variables.
func LoadConfig() *Config {
	return &Config{
		CompartmentID:      os.Getenv("OCI_COMPARTMENT_OCID"),
		AvailabilityDomain: os.Getenv("OCI_AVAILABILITY_DOMAIN"),
		ConfigFile:         os.Getenv("OCI_CONFIG_FILE"),
	}
}
