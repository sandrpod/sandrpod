package azure

import (
	"context"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"

	"github.com/sandrpod/sandrpod/pkg/provider"
)

func TestMapPowerState(t *testing.T) {
	cases := map[string]provider.VMState{
		"PowerState/running":      provider.VMStateRunning,
		"PowerState/starting":     provider.VMStatePending,
		"PowerState/stopping":     provider.VMStateStopping,
		"PowerState/stopped":      provider.VMStateStopped,
		"PowerState/deallocated":  provider.VMStateStopped,
		"PowerState/unknown-code": provider.VMStatePending, // default
	}
	for in, want := range cases {
		if got := mapPowerState(in); got != want {
			t.Errorf("mapPowerState(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestImageReference_URN(t *testing.T) {
	ref := imageReference("Canonical:0001-com-ubuntu-server-jammy:22_04-lts-gen2:latest")
	if ref.Publisher == nil || *ref.Publisher != "Canonical" {
		t.Errorf("Publisher = %v, want Canonical", ref.Publisher)
	}
	if ref.SKU == nil || *ref.SKU != "22_04-lts-gen2" {
		t.Errorf("SKU = %v", ref.SKU)
	}
	if ref.Version == nil || *ref.Version != "latest" {
		t.Errorf("Version = %v", ref.Version)
	}
	if ref.ID != nil {
		t.Errorf("ID should be nil for a URN, got %v", ref.ID)
	}
}

func TestImageReference_ResourceID(t *testing.T) {
	id := "/subscriptions/x/resourceGroups/y/providers/Microsoft.Compute/images/custom"
	ref := imageReference(id)
	if ref.ID == nil || *ref.ID != id {
		t.Errorf("ID = %v, want %q", ref.ID, id)
	}
	if ref.Publisher != nil {
		t.Errorf("Publisher should be nil for a resource ID, got %v", ref.Publisher)
	}
}

func TestImageReference_EmptyFallsBackToDefault(t *testing.T) {
	ref := imageReference("")
	if ref.Publisher == nil || *ref.Publisher != "Canonical" {
		t.Errorf("empty image should fall back to default Ubuntu URN, got %v", ref.Publisher)
	}
}

func TestImageReference_MalformedURNFallsBack(t *testing.T) {
	// Only two segments — not a valid publisher:offer:sku:version tuple.
	ref := imageReference("foo:bar")
	if ref.Publisher == nil || *ref.Publisher != "Canonical" {
		t.Errorf("malformed URN should fall back to default, got %v", ref.Publisher)
	}
}

func TestExtractExitCode(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantOut  string
		wantCode int
	}{
		{
			name:     "zero exit",
			in:       "hello world\n" + rcSentinel + "0",
			wantOut:  "hello world",
			wantCode: 0,
		},
		{
			name:     "nonzero exit",
			in:       "some output\n" + rcSentinel + "127",
			wantOut:  "some output",
			wantCode: 127,
		},
		{
			name:     "no sentinel assumes zero",
			in:       "plain output",
			wantOut:  "plain output",
			wantCode: 0,
		},
		{
			name:     "sentinel only",
			in:       rcSentinel + "3",
			wantOut:  "",
			wantCode: 3,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, code := extractExitCode(c.in)
			if strings.TrimSpace(out) != c.wantOut {
				t.Errorf("out = %q, want %q", strings.TrimSpace(out), c.wantOut)
			}
			if code != c.wantCode {
				t.Errorf("code = %d, want %d", code, c.wantCode)
			}
		})
	}
}

func TestParseRunCommandOutput(t *testing.T) {
	statuses := []*armcompute.InstanceViewStatus{
		{Code: to.Ptr("ComponentStatus/StdOut/succeeded"), Message: to.Ptr("out here")},
		{Code: to.Ptr("ComponentStatus/StdErr/succeeded"), Message: to.Ptr("err here")},
	}
	stdout, stderr := parseRunCommandOutput(statuses)
	if stdout != "out here" {
		t.Errorf("stdout = %q", stdout)
	}
	if stderr != "err here" {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestParseRunCommandOutput_NilSafe(t *testing.T) {
	statuses := []*armcompute.InstanceViewStatus{
		nil,
		{Code: nil, Message: to.Ptr("ignored")},
		{Code: to.Ptr("ComponentStatus/StdOut/succeeded"), Message: nil},
	}
	stdout, stderr := parseRunCommandOutput(statuses)
	if stdout != "" || stderr != "" {
		t.Errorf("expected empty outputs for nil-ish statuses, got %q / %q", stdout, stderr)
	}
}

func TestParseInt(t *testing.T) {
	if n, err := parseInt("42"); err != nil || n != 42 {
		t.Errorf("parseInt(42) = %d, %v", n, err)
	}
	if _, err := parseInt(""); err == nil {
		t.Error("parseInt(empty) should error")
	}
	if _, err := parseInt("1a"); err == nil {
		t.Error("parseInt(non-digit) should error")
	}
}

func TestComputerNameTruncation(t *testing.T) {
	long := strings.Repeat("a", 80)
	if got := computerName(long); len(got) != 63 {
		t.Errorf("computerName length = %d, want 63", len(got))
	}
	if got := computerName("short-vm"); got != "short-vm" {
		t.Errorf("computerName(short-vm) = %q", got)
	}
}

func TestGenerateAdminPassword_Complexity(t *testing.T) {
	pw := generateAdminPassword()
	if len(pw) != 20 {
		t.Fatalf("password length = %d, want 20", len(pw))
	}
	hasUpper := strings.ContainsAny(pw, "ABCDEFGHJKLMNPQRSTUVWXYZ")
	hasLower := strings.ContainsAny(pw, "abcdefghijkmnpqrstuvwxyz")
	hasDigit := strings.ContainsAny(pw, "23456789")
	hasSpecial := strings.ContainsAny(pw, "!@#$%^&*-_")
	if !hasUpper || !hasLower || !hasDigit || !hasSpecial {
		t.Errorf("password %q missing a required class (upper=%v lower=%v digit=%v special=%v)",
			pw, hasUpper, hasLower, hasDigit, hasSpecial)
	}
}

func TestProviderMetadata(t *testing.T) {
	p := &AzureProvider{}
	if p.Name() != "azure" {
		t.Errorf("Name() = %q", p.Name())
	}
	if p.DisplayName() != "Microsoft Azure" {
		t.Errorf("DisplayName() = %q", p.DisplayName())
	}
}

func TestListInstanceTypes_Static(t *testing.T) {
	p := &AzureProvider{}
	types, err := p.ListInstanceTypes(context.TODO(), "eastus")
	if err != nil {
		t.Fatalf("ListInstanceTypes: %v", err)
	}
	if len(types) == 0 {
		t.Fatal("expected a non-empty static instance-type list")
	}
	for _, it := range types {
		if it.Name == "" || it.CPU <= 0 || it.MemoryGiB <= 0 {
			t.Errorf("malformed instance type: %+v", it)
		}
	}
}

func TestGetDefaultImage(t *testing.T) {
	p := &AzureProvider{}
	img, err := p.GetDefaultImage(context.TODO(), "eastus")
	if err != nil {
		t.Fatalf("GetDefaultImage: %v", err)
	}
	if !strings.HasPrefix(img, "Canonical:") {
		t.Errorf("default image = %q, want a Canonical URN", img)
	}
}
