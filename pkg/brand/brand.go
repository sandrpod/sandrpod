// Copyright 2026 SandrPod
// Package brand resolves the product name shown on user-facing surfaces
// (tray menu, consent dialogs, local settings page). SandrPod is often
// embedded as the sandbox layer of a larger platform, and the human who
// sees a permission dialog should recognize the platform they actually
// use — set SANDRPOD_BRAND in that deployment (e.g. in the launchd /
// service definition of the agent and tray) to white-label these strings.
package brand

import "os"

const defaultName = "SandrPod"

// Name returns the display brand: SANDRPOD_BRAND if set, else "SandrPod".
// Read per call — it is only used on UI construction paths.
func Name() string {
	if v := os.Getenv("SANDRPOD_BRAND"); v != "" {
		return v
	}
	return defaultName
}
