package sandpod

import "testing"

func TestProviderEnvScopedThenGlobal(t *testing.T) {
	t.Setenv("SANDRPOD_VM_SUBNET_ID", "subnet-global")
	t.Setenv("SANDRPOD_VM_SUBNET_ID_ALIYUN", "subnet-aliyun")

	// Provider-scoped value wins.
	if got := providerEnv("SANDRPOD_VM_SUBNET_ID", "aliyun"); got != "subnet-aliyun" {
		t.Errorf("aliyun scoped: got %q, want subnet-aliyun", got)
	}
	// No scoped value -> falls back to the unscoped global.
	if got := providerEnv("SANDRPOD_VM_SUBNET_ID", "aws"); got != "subnet-global" {
		t.Errorf("aws fallback: got %q, want subnet-global", got)
	}
	// Empty provider -> global.
	if got := providerEnv("SANDRPOD_VM_SUBNET_ID", ""); got != "subnet-global" {
		t.Errorf("empty provider: got %q, want subnet-global", got)
	}
}

func TestProviderEnvBoolScopedThenGlobal(t *testing.T) {
	t.Setenv("SANDRPOD_VM_PUBLIC_IP", "true")
	t.Setenv("SANDRPOD_VM_PUBLIC_IP_AWS", "false")

	if providerEnvBool("SANDRPOD_VM_PUBLIC_IP", "aws", true) {
		t.Error("aws-scoped false should override the global true")
	}
	if !providerEnvBool("SANDRPOD_VM_PUBLIC_IP", "aliyun", false) {
		t.Error("aliyun should fall back to the global true")
	}
	if !providerEnvBool("SANDRPOD_VM_UNSET", "aws", true) {
		t.Error("unset key should return the default")
	}
}

func TestVMNetworkConfigPerProvider(t *testing.T) {
	t.Setenv("SANDRPOD_VM_SUBNET_ID", "subnet-global")
	t.Setenv("SANDRPOD_VM_SUBNET_ID_AWS", "subnet-aws")
	t.Setenv("SANDRPOD_VM_SECURITY_GROUP_ALIYUN", "sg-aliyun")

	aws := vmNetworkConfig("aws")
	if aws.SubnetID != "subnet-aws" {
		t.Errorf("aws subnet: got %q, want subnet-aws", aws.SubnetID)
	}

	aliyun := vmNetworkConfig("aliyun")
	if aliyun.SubnetID != "subnet-global" {
		t.Errorf("aliyun subnet fallback: got %q, want subnet-global", aliyun.SubnetID)
	}
	if aliyun.SecurityGroup != "sg-aliyun" {
		t.Errorf("aliyun sg: got %q, want sg-aliyun", aliyun.SecurityGroup)
	}
}
