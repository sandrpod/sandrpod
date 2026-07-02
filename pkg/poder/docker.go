// Copyright 2024 SandrPod
// Docker Poder implementation - for local development and testing

package poder

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/distribution/reference"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// DockerPoder is the Docker-backed implementation of Poder.
type DockerPoder struct {
	*BasePoder
	dockerClient *client.Client
	networkName  string
}

// NewDockerPoder creates a Docker-backed Poder.
// networkName specifies the Docker network that sandbox containers join. If empty,
// it falls back to the SANDRPOD_NETWORK environment variable. If still empty,
// no network is specified and containers use Docker's default bridge network.
func NewDockerPoder(region, networkName string) (*DockerPoder, error) {
	dockerClient, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("failed to create docker client: %w", err)
	}

	base := NewBasePoder("docker", "Docker Provider", region)

	if networkName == "" {
		networkName = os.Getenv("SANDRPOD_NETWORK")
	}
	// networkName is still empty → use Docker's default network, no explicit override

	return &DockerPoder{
		BasePoder:    base,
		dockerClient: dockerClient,
		networkName:  networkName,
	}, nil
}

// EnsureNetwork ensures the configured Docker network exists, creating it if needed.
func (p *DockerPoder) EnsureNetwork(ctx context.Context) error {
	// Check whether the network already exists
	networks, err := p.dockerClient.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		return err
	}

	for _, net := range networks {
		if net.Name == p.networkName {
			return nil
		}
	}

	// Create the network
	_, err = p.dockerClient.NetworkCreate(ctx, p.networkName, network.CreateOptions{
		Driver: "bridge",
	})
	return err
}

// CreatePod creates a new sandbox container pod.
func (p *DockerPoder) CreatePod(ctx context.Context, req *CreatePodRequest) (*PodInfo, error) {
	// Only ensure the network exists when a custom network is specified
	if p.networkName != "" {
		if err := p.EnsureNetwork(ctx); err != nil {
			return nil, fmt.Errorf("failed to ensure network: %w", err)
		}
	}

	// Generate a unique Pod ID
	podID := fmt.Sprintf("sp-%d-%s", time.Now().Unix(), randomString(8))

	// Resolve image name
	imageName := req.ImageID
	if imageName == "" {
		imageName = os.Getenv("SANDRPOD_TOOLBOX_IMAGE")
		if imageName == "" {
			imageName = "sandrpod/toolbox:test"
		}
	}

	// Build container labels
	labels := req.Labels
	if labels == nil {
		labels = make(map[string]string)
	}
	labels["sandrpod/sandbox-name"] = req.Name

	// ContainerCreate (unlike `docker run`) does not pull a missing image, so
	// a fresh host needs an explicit pull. Best-effort: if the image already
	// exists locally this is a no-op; a genuine pull failure is surfaced by the
	// ContainerCreate below with a clear "No such image" error.
	if rc, perr := p.dockerClient.ImagePull(ctx, imageName, image.PullOptions{}); perr == nil {
		_, _ = io.Copy(io.Discard, rc)
		_ = rc.Close()
	} else {
		log.Printf("poder: pull image %q failed (continuing, may exist locally): %v", imageName, perr)
	}

	containerConfig := &container.Config{
		Image: imageName,
		Env: []string{
			"TOOLBOX_API_URL=" + req.APIURL,
			"PODER_VERSION=" + req.PoderVersion,
			"LOG_LEVEL=" + req.LogLevel,
		},
		Labels: labels,
	}

	hostConfig := &container.HostConfig{}
	if p.networkName != "" {
		hostConfig.NetworkMode = container.NetworkMode(p.networkName)
	}
	// Per-sandbox resource limits (noisy-neighbor isolation). NanoCPUs is CPU
	// cores × 1e9; Memory is in bytes. Both are ignored by Docker when zero.
	if req.CPUCores > 0 {
		hostConfig.NanoCPUs = int64(req.CPUCores * 1e9)
	}
	if req.MemoryMB > 0 {
		hostConfig.Memory = req.MemoryMB * 1024 * 1024
	}

	resp, err := p.dockerClient.ContainerCreate(ctx, containerConfig, hostConfig, nil, nil, podID)
	if err != nil {
		return nil, fmt.Errorf("failed to create container: %w", err)
	}

	// Start the container
	if err := p.dockerClient.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return nil, fmt.Errorf("failed to start container: %w", err)
	}

	// Inspect the container to get runtime details
	containerInfo, err := p.dockerClient.ContainerInspect(ctx, resp.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect container: %w", err)
	}

	// Resolve container IP: prefer the specified network; fall back to the first available network IP
	ip := ""
	if p.networkName != "" {
		if netSettings, ok := containerInfo.NetworkSettings.Networks[p.networkName]; ok {
			ip = netSettings.IPAddress
		}
	} else {
		for _, netSettings := range containerInfo.NetworkSettings.Networks {
			if netSettings.IPAddress != "" {
				ip = netSettings.IPAddress
				break
			}
		}
	}

	// Build PodInfo
	pod := &PodInfo{
		ID:           podID,
		Name:         req.Name,
		Region:       req.Region,
		Provider:     "docker",
		InstanceType: req.InstanceType,
		State:        PodStateStarting,
		IP:           ip,
		CreatedAt:    time.Now(),
	}

	p.RegisterPod(pod)

	// Wait for the container to reach Running state in the background
	go func() {
		time.Sleep(5 * time.Second)
		p.UpdatePodState(podID, PodStateRunning)
		pod.State = PodStateRunning
	}()

	return pod, nil
}

// DeletePod stops and removes the container for the given pod.
func (p *DockerPoder) DeletePod(ctx context.Context, podID string) error {
	// Stop the container gracefully
	_ = p.dockerClient.ContainerStop(ctx, podID, container.StopOptions{Timeout: ptr(10)})

	// Remove the container
	if err := p.dockerClient.ContainerRemove(ctx, podID, container.RemoveOptions{Force: true}); err != nil {
		return fmt.Errorf("failed to remove container: %w", err)
	}

	p.UnregisterPod(podID)
	return nil
}

// PausePod pauses the container, preserving its state.
func (p *DockerPoder) PausePod(ctx context.Context, podID string) error {
	return p.dockerClient.ContainerPause(ctx, podID)
}

// UnpausePod resumes a paused pod.
func (p *DockerPoder) UnpausePod(ctx context.Context, podID string) error {
	return p.dockerClient.ContainerUnpause(ctx, podID)
}

// SnapshotPod commits the pod's container to a new image (docker commit) and
// returns the resulting image reference. imageName may be "repo:tag"; if it has
// no tag, Docker applies :latest.
func (p *DockerPoder) SnapshotPod(ctx context.Context, podID, imageName string) (string, error) {
	if imageName == "" {
		return "", fmt.Errorf("snapshot image name is required")
	}
	ref, err := reference.ParseNormalizedNamed(imageName)
	if err != nil {
		return "", fmt.Errorf("invalid snapshot image name %q: %w", imageName, err)
	}
	ref = reference.TagNameOnly(ref)
	if _, err := p.dockerClient.ContainerCommit(ctx, podID, container.CommitOptions{
		Reference: ref.String(),
		Comment:   "sandrpod snapshot",
	}); err != nil {
		return "", fmt.Errorf("failed to commit container %s: %w", podID, err)
	}
	return ref.String(), nil
}

// GetPod returns information about a pod, consulting the internal registry first
// and falling back to a direct Docker inspect if the pod is not registered.
func (p *DockerPoder) GetPod(ctx context.Context, podID string) (*PodInfo, error) {
	// Try the internal registry first
	pod, ok := p.GetPodByID(podID)
	if !ok {
		// Not in registry, query Docker directly
		info, err := p.dockerClient.ContainerInspect(ctx, podID)
		if err != nil {
			return nil, fmt.Errorf("pod %s not found", podID)
		}
		// Build PodInfo from the Docker inspect response
		pod = &PodInfo{
			ID:    info.ID,
			Name:  info.Name,
			State: PodStateStopped,
		}
		// Resolve IP from network settings
		if p.networkName != "" {
			if net, ok := info.NetworkSettings.Networks[p.networkName]; ok {
				pod.IP = net.IPAddress
			}
		} else {
			for _, net := range info.NetworkSettings.Networks {
				if net.IPAddress != "" {
					pod.IP = net.IPAddress
					break
				}
			}
		}
	}

	// Query live container state from Docker
	info, err := p.dockerClient.ContainerInspect(ctx, podID)
	if err != nil {
		return pod, nil // Return cached state if Docker inspect fails
	}

	// Paused must be checked before Running, since paused containers have Running=true
	if info.State.Paused {
		pod.State = PodStateStopped
	} else if info.State.Running {
		pod.State = PodStateRunning
	} else if info.State.ExitCode != 0 {
		pod.State = PodStateError
	} else {
		pod.State = PodStateStopped
	}

	return pod, nil
}

// ListRunningSandboxNames returns the sandbox names of all running SandrPod-managed
// containers by querying Docker directly. This is the authoritative source: it
// reflects reality even after a Poder restart (unlike BasePoder.ListPods which
// only knows about containers created in the current process lifetime).
func (p *DockerPoder) ListRunningSandboxNames(ctx context.Context) ([]string, error) {
	containers, err := p.dockerClient.ContainerList(ctx, container.ListOptions{
		Filters: filters.NewArgs(
			filters.Arg("status", "running"),
			filters.Arg("label", "sandrpod/sandbox-name"),
		),
	})
	if err != nil {
		return nil, fmt.Errorf("docker ContainerList: %w", err)
	}
	names := make([]string, 0, len(containers))
	for _, c := range containers {
		if name := c.Labels["sandrpod/sandbox-name"]; name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

// FindPodByName finds a pod by its sandbox name via a direct Docker container list query.
func (p *DockerPoder) FindPodByName(ctx context.Context, sandboxName string) (*PodInfo, error) {
	containers, err := p.dockerClient.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %w", err)
	}

	for _, c := range containers {
		if c.Labels["sandrpod/sandbox-name"] == sandboxName {
			return p.GetPod(ctx, c.ID)
		}
	}

	return nil, fmt.Errorf("pod with sandbox name %s not found", sandboxName)
}

// GetPodLogs retrieves logs from a container.
func (p *DockerPoder) GetPodLogs(ctx context.Context, podID string, tail string) (string, error) {
	if tail == "" {
		tail = "100"
	}

	options := container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Timestamps: true,
		Tail:       tail,
	}

	resp, err := p.dockerClient.ContainerLogs(ctx, podID, options)
	if err != nil {
		return "", fmt.Errorf("failed to get container logs: %w", err)
	}
	defer resp.Close()

	// Decode the log stream (Docker logs use multiplexed format)
	var stdout, stderr bytes.Buffer
	_, err = stdcopy.StdCopy(&stdout, &stderr, resp)
	if err != nil {
		return "", fmt.Errorf("failed to copy logs: %w", err)
	}

	// Combine stdout and stderr
	logs := stdout.String()
	if stderr.Len() > 0 {
		logs += stderr.String()
	}

	return logs, nil
}

// ExecuteCommand runs a shell command inside the container and returns the output.
func (p *DockerPoder) ExecuteCommand(ctx context.Context, podID, command string) (*CommandResult, error) {
	execResp, err := p.dockerClient.ContainerExecCreate(ctx, podID, container.ExecOptions{
		Cmd:          []string{"/bin/sh", "-c", command},
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create exec: %w", err)
	}

	resp, err := p.dockerClient.ContainerExecAttach(ctx, execResp.ID, container.ExecAttachOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to attach exec: %w", err)
	}
	defer resp.Close()

	var stdout, stderr bytes.Buffer
	_, err = stdcopy.StdCopy(&stdout, &stderr, resp.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to copy output: %w", err)
	}

	// Retrieve the exit code
	info, err := p.dockerClient.ContainerExecInspect(ctx, execResp.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect exec: %w", err)
	}

	return &CommandResult{
		Output:     stdout.String(),
		Stderr:     stderr.String(),
		ExitCode:   int(info.ExitCode),
		ExecutedAt: time.Now(),
	}, nil
}

// ExecWithPty creates a PTY-attached exec session and returns the exec ID.
func (p *DockerPoder) ExecWithPty(ctx context.Context, podID string, width, height int) (string, error) {
	shell := "/bin/bash"

	execResp, err := p.dockerClient.ContainerExecCreate(ctx, podID, container.ExecOptions{
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          true,
		Cmd:          []string{shell},
	})
	if err != nil {
		return "", fmt.Errorf("failed to create exec: %w", err)
	}

	// Start exec in the background (ContainerExecStart blocks until the session ends)
	go func() {
		err := p.dockerClient.ContainerExecStart(context.Background(), execResp.ID, container.ExecStartOptions{
			Tty: true,
		})
		if err != nil {
			// Normal error when the session ends or is aborted; log at debug level only
			_ = err
		}
	}()

	// Wait briefly for the exec process to initialize
	time.Sleep(200 * time.Millisecond)

	return execResp.ID, nil
}

// AttachExec attaches to a running exec process and returns a HijackedResponse.
func (p *DockerPoder) AttachExec(ctx context.Context, execID string) (*types.HijackedResponse, error) {
	resp, err := p.dockerClient.ContainerExecAttach(ctx, execID, container.ExecAttachOptions{Tty: true})
	if err != nil {
		return nil, fmt.Errorf("failed to attach to exec: %w", err)
	}

	return &resp, nil
}

// InspectExec returns status information about an exec process.
func (p *DockerPoder) InspectExec(ctx context.Context, execID string) (*container.ExecInspect, error) {
	info, err := p.dockerClient.ContainerExecInspect(ctx, execID)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect exec: %w", err)
	}
	return &info, nil
}

// GetHealthStatus returns the health status of a pod.
func (p *DockerPoder) GetHealthStatus(ctx context.Context, podID string) (*HealthStatus, error) {
	pod, err := p.GetPod(ctx, podID)
	if err != nil {
		return nil, err
	}

	status := &HealthStatus{
		PodReady:     pod.State == PodStateRunning,
		DockerReady:  true,
		ToolboxReady: pod.State == PodStateRunning,
	}

	return status, nil
}

// WaitUntilRunning blocks until the pod reaches the Running state or the timeout expires.
func (p *DockerPoder) WaitUntilRunning(ctx context.Context, podID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(1 * time.Second)

	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for pod %s to be running", podID)
		}

		pod, err := p.GetPod(ctx, podID)
		if err == nil && pod != nil && pod.State == PodStateRunning {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Cleanup removes all SandrPod-managed containers.
func (p *DockerPoder) Cleanup(ctx context.Context) error {
	// List all containers
	containers, err := p.dockerClient.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return err
	}

	// Remove all containers tagged as SandrPod-managed
	for _, c := range containers {
		if c.Labels["sandrpod"] == "true" {
			_ = p.dockerClient.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true})
		}
	}

	return nil
}

// randomString generates a cryptographically random hex string from n bytes (resulting length is 2n).
func randomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// ptr returns a pointer to the given value.
func ptr[T any](v T) *T {
	return &v
}
