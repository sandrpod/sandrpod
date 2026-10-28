// Copyright 2026 SandrPod Contributors
// Egress enforcement for container pods.
//
// A sandbox in a container has the same problem as one on a laptop, with a
// better toolkit: Docker can take the container's route to the world away, so
// the proxy stops being advisory. The shape is:
//
//		sandbox ── (internal network) ── sidecar proxy ── (bridge) ── internet
//
//	  - a per-pod bridge network created with Internal: true. Docker installs no
//	    masquerade rule for it, so a container attached to it has no route off
//	    the machine;
//	  - the sandbox container is attached to that network and nothing else;
//	  - a sidecar container runs the egress proxy, attached to the internal
//	    network (so the sandbox can reach it by name) and to an ordinary network
//	    (so it can reach the internet). It is the sandbox's only exit.
//
// Why a sidecar and not the poder itself: on Linux the poder could bind the
// bridge gateway address, but on Docker Desktop (macOS/Windows) that address
// lives inside the Docker VM and the host cannot bind it at all. A container
// that joins the network is the one arrangement that behaves identically on
// every platform.
//
// What this does NOT cover, said here rather than discovered later: the sandbox
// can still reach anything else that joins the same internal network — today
// only the proxy — and on Linux it can reach services the host itself exposes
// on the bridge subnet. Closing that last one needs host-level iptables rules
// (a DOCKER-USER chain, root on the host) and is not in this build.
//
// Refusing beats degrading: if the proxy never reports itself listening, the
// pod is not created. A sandbox asking for a policy and silently not getting
// one is worse than one that was never created.
package poder

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/sandrpod/sandrpod/pkg/egress"
)

// Fixed ports: the sandbox's environment is built before the proxy container
// exists, so it cannot be told a port the kernel picked.
const (
	egressHTTPPort  = 8080
	egressSOCKSPort = 1080
)

// egressImage is the sidecar image. Overridable because an air-gapped install
// mirrors it, and because the integration test builds one locally.
func egressImage() string {
	if img := os.Getenv("SANDRPOD_EGRESS_IMAGE"); img != "" {
		return img
	}
	return "ghcr.io/sandrpod/egress-proxy:latest"
}

// podEgress is the per-pod enforcement state, so DeletePod can put back what
// CreatePod created.
type podEgress struct {
	network   string // internal network ID
	sidecar   string // sidecar container ID
	env       []string
	httpAddr  string
	socksAddr string
}

func egressNetworkName(podID string) string { return "sandrpod-egress-" + podID }
func egressSidecarName(podID string) string { return "sandrpod-egress-proxy-" + podID }

// setupEgress creates the internal network and the proxy sidecar, and returns
// the state plus the environment the sandbox container needs.
func (p *DockerPoder) setupEgress(ctx context.Context, podID string, policy *egress.Policy) (*podEgress, error) {
	compiled, err := egress.Parse(*policy)
	if err != nil {
		return nil, fmt.Errorf("egress policy: %w", err)
	}
	policyJSON, err := json.Marshal(compiled.Policy())
	if err != nil {
		return nil, fmt.Errorf("marshal egress policy: %w", err)
	}
	token, err := egressToken()
	if err != nil {
		return nil, err
	}

	netName := egressNetworkName(podID)
	created, err := p.dockerClient.NetworkCreate(ctx, netName, network.CreateOptions{
		Driver:   "bridge",
		Internal: true,
		Labels: map[string]string{
			"sandrpod/egress":      "true",
			"sandrpod/sandbox-pod": podID,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create egress network %s: %w", netName, err)
	}
	cleanup := func() { _ = p.dockerClient.NetworkRemove(ctx, created.ID) }

	sidecarName := egressSidecarName(podID)
	resp, err := p.dockerClient.ContainerCreate(ctx,
		&container.Config{
			Image: egressImage(),
			Cmd: []string{
				"-listen", fmt.Sprintf("0.0.0.0:%d", egressHTTPPort),
				"-socks", fmt.Sprintf("0.0.0.0:%d", egressSOCKSPort),
				"-token", token,
				"-policy-json", string(policyJSON),
			},
			Labels: map[string]string{
				"sandrpod/egress":      "true",
				"sandrpod/sandbox-pod": podID,
			},
		},
		&container.HostConfig{
			NetworkMode: container.NetworkMode(netName),
			// The proxy holds the sandbox's only route out; if it dies there is
			// nothing to fall back to, and restarting it does not widen
			// anything. `no` keeps a crash loud in `docker ps` instead of
			// silently cycling.
			RestartPolicy: container.RestartPolicy{Name: "no"},
		},
		nil, nil, sidecarName)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("create egress proxy container: %w", err)
	}
	removeSidecar := func() {
		_ = p.dockerClient.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
	}

	// Give the proxy a way out: attach it to an ordinary network. The internal
	// network alone would leave it as trapped as the sandbox.
	outside := p.networkName
	if outside == "" {
		outside = "bridge"
	}
	if err := p.dockerClient.NetworkConnect(ctx, outside, resp.ID, nil); err != nil {
		removeSidecar()
		cleanup()
		return nil, fmt.Errorf("attach egress proxy to %q: %w", outside, err)
	}

	if err := p.dockerClient.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		removeSidecar()
		cleanup()
		return nil, fmt.Errorf("start egress proxy: %w", err)
	}

	// Wait for the proxy to say it is listening. Starting the sandbox first
	// would give it a few hundred milliseconds of "connection refused" that
	// looks, from inside, exactly like a network outage.
	if err := p.waitForEgressReady(ctx, resp.ID, sidecarName, 15*time.Second); err != nil {
		removeSidecar()
		cleanup()
		return nil, err
	}

	// The sandbox is now on an internal network, so the toolbox URL the poder
	// dials (http://<pod IP>:8080) only resolves if the poder itself is on
	// that network. A poder running as a container joins it here; a poder on
	// the host reaches the bridge directly (Linux) and needs nothing.
	if err := p.attachSelfToNetwork(ctx, created.ID); err != nil {
		removeSidecar()
		cleanup()
		return nil, err
	}

	host := sidecarName // resolved by the internal network's embedded DNS
	proxyURL := fmt.Sprintf("http://sandbox:%s@%s:%d", token, host, egressHTTPPort)
	socksURL := fmt.Sprintf("socks5://sandbox:%s@%s:%d", token, host, egressSOCKSPort)
	env := []string{
		"HTTP_PROXY=" + proxyURL, "http_proxy=" + proxyURL,
		"HTTPS_PROXY=" + proxyURL, "https_proxy=" + proxyURL,
		"ALL_PROXY=" + socksURL, "all_proxy=" + socksURL,
		"NODE_USE_ENV_PROXY=1",
	}

	allow, deny := compiled.Strings()
	log.Printf("poder: egress for %s via %s (internal net %s) allow=%v deny=%v",
		podID, sidecarName, netName, allow, deny)

	return &podEgress{
		network:   created.ID,
		sidecar:   resp.ID,
		env:       env,
		httpAddr:  fmt.Sprintf("%s:%d", host, egressHTTPPort),
		socksAddr: fmt.Sprintf("%s:%d", host, egressSOCKSPort),
	}, nil
}

// waitForEgressReady polls the sidecar's log for its ready line.
func (p *DockerPoder) waitForEgressReady(ctx context.Context, containerID, sidecarName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		logs, err := p.dockerClient.ContainerLogs(ctx, containerID, container.LogsOptions{
			ShowStdout: true, ShowStderr: true, Tail: "20",
		})
		if err == nil {
			raw, _ := io.ReadAll(io.LimitReader(logs, 64<<10))
			_ = logs.Close()
			if strings.Contains(string(raw), "egress proxy listening") {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("egress proxy did not become ready within %s (check `docker logs %s`)",
				timeout, sidecarName)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// teardownEgress removes the sidecar and the internal network. Safe for a pod
// that never had one.
// egressCleanupTargets reports which objects belong to podID. The in-memory map
// is a convenience, not the authority: it is empty after a pode restart, and
// trusting it alone leaked one container and one bridge network for every egress
// sandbox created before that restart (measured: restart the worker, delete the
// sandbox, `docker ps` still lists sandrpod-egress-proxy-<id>).
func egressCleanupTargets(e *podEgress, podID string) (sidecar, network string) {
	sidecar, network = egressSidecarName(podID), egressNetworkName(podID)
	if e == nil {
		return sidecar, network
	}
	if e.sidecar != "" {
		sidecar = e.sidecar
	}
	if e.network != "" {
		network = e.network
	}
	return sidecar, network
}

func (p *DockerPoder) teardownEgress(ctx context.Context, podID string) {
	sidecar, network := egressCleanupTargets(p.takeEgress(podID), podID)

	if err := p.dockerClient.ContainerRemove(ctx, sidecar, container.RemoveOptions{Force: true}); err != nil &&
		!client.IsErrNotFound(err) {
		log.Printf("poder: remove egress proxy for %s: %v", podID, err)
	}
	p.detachSelfFromNetwork(ctx, network)
	// Removing a network with a container still attached fails; by now the sandbox
	// container is gone (DeletePod removes it first) or the whole thing is being
	// abandoned, so a failure here is noise, not a leak of reachability.
	if err := p.dockerClient.NetworkRemove(ctx, network); err != nil && !client.IsErrNotFound(err) {
		log.Printf("poder: remove egress network for %s: %v", podID, err)
	}
}

// sweepOrphanedEgress removes egress sidecars and networks whose sandbox
// container no longer exists. Deletion covers the normal path, but a pod that
// died with its host never gets a delete call — and each leftover holds a
// bridge network and a container that nothing will ever reference again.
//
// Runs once at startup, in the background: a pode that cannot list its own
// containers should still serve.
func (p *DockerPoder) sweepOrphanedEgress(ctx context.Context) {
	containers, err := p.dockerClient.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", "sandrpod/egress=true")),
	})
	if err != nil {
		log.Printf("poder: egress sweep: list containers: %v", err)
		return
	}
	removed := 0
	for _, c := range containers {
		podID := c.Labels["sandrpod/sandbox-pod"]
		if podID == "" {
			continue
		}
		if _, err := p.dockerClient.ContainerInspect(ctx, podID); err == nil {
			continue // the sandbox is still there; this sidecar is in use
		} else if !client.IsErrNotFound(err) {
			continue // cannot tell; leave it alone rather than break a live sandbox
		}
		sidecar, network := egressCleanupTargets(nil, podID)
		if err := p.dockerClient.ContainerRemove(ctx, sidecar, container.RemoveOptions{Force: true}); err != nil &&
			!client.IsErrNotFound(err) {
			log.Printf("poder: egress sweep: remove %s: %v", sidecar, err)
			continue
		}
		_ = p.dockerClient.NetworkRemove(ctx, network)
		removed++
	}
	if removed > 0 {
		log.Printf("poder: egress sweep: removed %d orphaned sidecar(s)", removed)
	}
}

func (p *DockerPoder) takeEgress(podID string) *podEgress {
	p.egressMu.Lock()
	defer p.egressMu.Unlock()
	e := p.egress[podID]
	delete(p.egress, podID)
	return e
}

func (p *DockerPoder) putEgress(podID string, e *podEgress) {
	p.egressMu.Lock()
	defer p.egressMu.Unlock()
	if p.egress == nil {
		p.egress = make(map[string]*podEgress)
	}
	p.egress[podID] = e
}

// selfContainerID returns the ID of the container this process runs in, or ""
// when it runs on the host. Docker sets a container's hostname to its short ID
// by default, which is enough to look itself up.
//
// The distinction matters: a poder in a container has to join each pod's
// internal network to reach the toolbox, while a poder on the host (Linux)
// already routes to the bridge.
func (p *DockerPoder) selfContainerID(ctx context.Context) string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return ""
	}
	info, err := p.dockerClient.ContainerInspect(ctx, host)
	if err != nil || info.ID == "" || info.Config == nil {
		return ""
	}
	// A host whose name happens to match a container ID would be a coincidence
	// we cannot rule out; require the container to be running and to be the
	// one whose network namespace we are not otherwise in.
	if !info.State.Running {
		return ""
	}
	return info.ID
}

// attachSelfToNetwork joins the poder's own container to a pod's internal
// network. A no-op (and not an error) when the poder runs on the host.
func (p *DockerPoder) attachSelfToNetwork(ctx context.Context, networkID string) error {
	self := p.selfContainerID(ctx)
	if self == "" {
		return nil
	}
	if err := p.dockerClient.NetworkConnect(ctx, networkID, self, nil); err != nil {
		return fmt.Errorf("attach the poder container to the pod's egress network "+
			"(without it the toolbox is unreachable): %w", err)
	}
	log.Printf("poder: joined pod egress network %s so the toolbox stays reachable", networkID[:12])
	return nil
}

// detachSelfFromNetwork undoes attachSelfToNetwork. Best effort: the network is
// about to be removed, and a poder that cannot leave it is not a security
// problem, only a leftover.
func (p *DockerPoder) detachSelfFromNetwork(ctx context.Context, networkID string) {
	self := p.selfContainerID(ctx)
	if self == "" || networkID == "" {
		return
	}
	if err := p.dockerClient.NetworkDisconnect(ctx, networkID, self, true); err != nil {
		log.Printf("poder: leave pod egress network %s: %v", networkID[:12], err)
	}
}

// egressToken mints the per-pod proxy password: 128 bits, so a neighbouring
// sandbox cannot guess its way onto another pod's allow-list.
func egressToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate egress token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
