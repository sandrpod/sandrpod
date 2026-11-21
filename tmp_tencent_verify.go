//go:build ignore

package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sandrpod/sandrpod/pkg/provider"
	tc "github.com/sandrpod/sandrpod/pkg/provider/tencent"
)

func creds() (string, string) {
	path := filepath.Join(os.Getenv("HOME"), "Downloads", "SecretKey.csv")
	f, err := os.Open(path)
	if err != nil {
		fmt.Println("open csv:", err)
		os.Exit(1)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil || len(rows) < 2 {
		fmt.Println("parse csv:", err)
		os.Exit(1)
	}
	return rows[1][0], rows[1][1]
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	id, key := creds()
	region := envOr("TC_REGION", "ap-guangzhou")
	zone := envOr("TC_ZONE", "ap-guangzhou-3")
	itype := envOr("TC_TYPE", "S5.MEDIUM2")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	p, err := tc.NewTencentProvider(&tc.Config{SecretID: id, SecretKey: key, Region: region, Zone: zone})
	if err != nil {
		fmt.Println("provider:", err)
		return
	}

	// 1. A zone outside the client's region is refused before any API call.
	_, err = p.CreateVM(ctx, &provider.CreateVMRequest{
		Name: "sandrpod-verify", Region: "ap-hongkong-1", InstanceType: itype,
	})
	fmt.Printf("zone-mismatch: refused=%v\n", err != nil)

	// 2. A real create in the configured zone.
	vm, err := p.CreateVM(ctx, &provider.CreateVMRequest{
		Name: "sandrpod-verify", Region: zone, InstanceType: itype,
		NetworkConfig: &provider.NetworkConfig{PublicIP: true},
	})
	if err != nil {
		fmt.Println("create failed:", err)
		return
	}
	fmt.Printf("created: %s state=%v publicIP=%q\n", vm.ID, vm.State, vm.PublicIP)

	// 3. A TAT command through the provider.
	res, err := p.ExecuteCommand(ctx, vm.ID, "echo tencent-ok; uname -r")
	if err != nil {
		fmt.Println("execute failed:", err)
	} else {
		fmt.Printf("execute: exit=%d out=%q err=%q\n", res.ExitCode, strings.TrimSpace(res.Output), res.Stderr)
	}

	// 4. Deleting twice: the second is "already gone" and must not be an error.
	if derr := p.DeleteVM(context.Background(), vm.ID); derr != nil {
		fmt.Println("delete: failed:", derr)
	} else {
		fmt.Println("delete: ok")
	}
	if derr := p.DeleteVM(context.Background(), vm.ID); derr != nil {
		fmt.Printf("delete-twice: error (want nil): %v\n", derr)
	} else {
		fmt.Println("delete-twice: ok")
	}

	// 5. Spot capacity, if the account and zone have any.
	spot, err := p.CreateVM(ctx, &provider.CreateVMRequest{
		Name: "sandrpod-verify-spot", Region: zone, InstanceType: itype,
		Spot: true, SpotPriceLimit: 0.5,
		NetworkConfig: &provider.NetworkConfig{PublicIP: true},
	})
	if err != nil {
		fmt.Println("spot-create:", err)
		return
	}
	fmt.Printf("spot-created: %s state=%v\n", spot.ID, spot.State)
	if derr := p.DeleteVM(context.Background(), spot.ID); derr != nil {
		fmt.Println("spot-delete failed:", derr)
	} else {
		fmt.Println("spot-delete: ok")
	}
}
