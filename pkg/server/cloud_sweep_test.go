// Copyright 2026 SandrPod Contributors
package server

import (
	"context"
	"errors"
	"testing"

	"github.com/sandrpod/sandrpod/pkg/provider"
	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
	"github.com/sandrpod/sandrpod/pkg/store"
)

func TestSweepCloudOnce_DeletesOrphanOnly(t *testing.T) {
	stores := store.NewMemoryStores()
	// One owned cloud poder, one local poder with no VM.
	if _, err := stores.Poders.Register(&podpkg.RegisterPoderRequest{
		Name: "owned", Region: "hk", ProviderType: "aliyun", VMID: "vm-owned",
		Resources: podpkg.PoderResources{CPUCores: 2, MemoryBytes: 1 << 30},
	}); err != nil {
		t.Fatal(err)
	}
	list := func(context.Context) ([]providerVM, error) {
		return []providerVM{
			{Provider: "aliyun", VM: &provider.VMInfo{ID: "vm-owned", Name: "sandrpod-owned"}},
			{Provider: "aliyun", VM: &provider.VMInfo{ID: "vm-orphan", Name: "sandrpod-orphan"}},
			{Provider: "aliyun", VM: &provider.VMInfo{ID: "vm-manual", Name: "my-own-box"}},
		}, nil
	}
	var deleted []string
	terminate := func(_ context.Context, providerType, vmID string) error {
		deleted = append(deleted, providerType+"/"+vmID)
		return nil
	}
	orphans, err := sweepCloudOnce(context.Background(), stores.Poders, sweepDelete, list, terminate)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(orphans) != 1 || orphans[0] != "vm-orphan" {
		t.Fatalf("orphans = %v, want [vm-orphan]", orphans)
	}
	if len(deleted) != 1 || deleted[0] != "aliyun/vm-orphan" {
		t.Fatalf("deleted = %v, want [aliyun/vm-orphan]", deleted)
	}
}

func TestSweepCloudOnce_AlertModeDoesNotDelete(t *testing.T) {
	stores := store.NewMemoryStores()
	list := func(context.Context) ([]providerVM, error) {
		return []providerVM{{Provider: "aliyun", VM: &provider.VMInfo{ID: "vm-orphan", Name: "sandrpod-x"}}}, nil
	}
	terminate := func(context.Context, string, string) error {
		t.Fatal("alert mode must not terminate")
		return nil
	}
	orphans, err := sweepCloudOnce(context.Background(), stores.Poders, sweepAlert, list, terminate)
	if err != nil || len(orphans) != 1 {
		t.Fatalf("orphans=%v err=%v, want one orphan and no error", orphans, err)
	}
}

func TestSweepCloudOnce_TerminateFailureKeepsVM(t *testing.T) {
	stores := store.NewMemoryStores()
	list := func(context.Context) ([]providerVM, error) {
		return []providerVM{{Provider: "aliyun", VM: &provider.VMInfo{ID: "vm-orphan", Name: "sandrpod-x"}}}, nil
	}
	terminate := func(context.Context, string, string) error { return errors.New("cloud said no") }
	orphans, err := sweepCloudOnce(context.Background(), stores.Poders, sweepDelete, list, terminate)
	if err != nil {
		t.Fatalf("list errors must not fail the sweep: %v", err)
	}
	if len(orphans) != 1 {
		t.Fatalf("orphans = %v, want the orphan reported even when termination fails", orphans)
	}
}

func TestSweepCloudOnce_ListErrorsStillReturnOrphans(t *testing.T) {
	stores := store.NewMemoryStores()
	list := func(context.Context) ([]providerVM, error) {
		return nil, errors.New("provider down")
	}
	orphans, err := sweepCloudOnce(context.Background(), stores.Poders, sweepDelete, list, func(context.Context, string, string) error { return nil })
	if err == nil {
		t.Fatal("expected the list error to surface")
	}
	if len(orphans) != 0 {
		t.Fatalf("orphans = %v, want none", orphans)
	}
}
