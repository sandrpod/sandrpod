// Copyright 2026 SandrPod Contributors
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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/distribution/reference"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// DockerPoder is the Docker-backed implementation of Poder.
type DockerPoder struct {
	*BasePoder
	dockerClient *client.Client
	networkName  string

	// Per-pod egress enforcement state (see egress.go). Guarded because
	// CreatePod and DeletePod can run concurrently on different pods.
	egressMu sync.Mutex
	egress   map[string]*podEgress
}

// NewDockerPoder creates a Docker-backed Poder.
// networkName specifies the Docker network that sandbox containers join. If empty,
// it falls back to the SANDRPOD_NETWORK environment variable. If still empty,
// no network is specified and containers use Docker's default bridge network.
func NewDockerPoder(region, networkName string) (*DockerPoder, error) {
	// WithAPIVersionNegotiation pings the daemon and downgrades to the highest
	// API version it actually supports. Without it the SDK sends whatever
	// version it was compiled against, and any daemon older than that rejects
	// every call with "client version X is too new". DOCKER_API_VERSION still
	// wins if set — FromEnv marks it a manual override and skips negotiation.
	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("failed to create docker client: %w", err)
	}

	base := NewBasePoder("docker", "Docker Provider", region)

	if networkName == "" {
		networkName = os.Getenv("SANDRPOD_NETWORK")
	}
	// networkName is still empty → use Docker's default network, no explicit override

	pode := &DockerPoder{
		BasePoder:    base,
		dockerClient: dockerClient,
		networkName:  networkName,
	}

	// Clean up egress sidecars whose sandbox died with the host. Off the startup
	// path: a pode that cannot list containers should still serve.
	go pode.sweepOrphanedEgress(context.Background())

	return pode, nil
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
func (p *DockerPoder) CreatePod(ctx context.Context, req *CreatePodRequest) (pod *PodInfo, retErr error) {
	// Only ensure the network exists when a custom network is specified
	if p.networkName != "" {
		if err := p.EnsureNetwork(ctx); err != nil {
			return nil, fmt.Errorf("failed to ensure network: %w", err)
		}
	}

	// A create for a sandbox that already has a container is a retry, not a second
	// sandbox: the delivery promise is at-least-once, so the same request can
	// arrive twice. Removing the previous container first makes the operation
	// idempotent — without it each retry leaked a container (and, with an egress
	// policy, a network), which on a real worker ended in "all predefined address
	// pools have been fully subnetted".
	if existing, err := p.findContainerBySandboxName(ctx, req.Name); err == nil && existing != "" {
		log.Printf("poder: replacing existing container %s for sandbox %s (retry of a create)", existing, req.Name)
		if err := p.DeletePod(ctx, existing); err != nil {
			log.Printf("poder: could not remove the previous container %s for %s: %v", existing, req.Name, err)
		}
	}

	// Generate a unique Pod ID
	podID := fmt.Sprintf("sp-%d-%s", time.Now().Unix(), randomString(8))

	// Resolve image name
	imageName := req.ImageID
	if imageName == "" {
		imageName = os.Getenv("SANDRPOD_TOOLBOX_IMAGE")
		if imageName == "" {
			imageName = "ghcr.io/sandrpod/toolbox:latest"
		}
	}

	// Build container labels
	labels := req.Labels
	if labels == nil {
		labels = make(map[string]string)
	}
	labels["sandrpod/sandbox-name"] = req.Name

	// ContainerCreate (unlike `docker run`) does not pull a missing image, so a
	// fresh host needs an explicit pull — but only then. Calling ImagePull
	// unconditionally costs a registry round-trip on every create: measured at
	// 1.8s against a local image, which was the single largest component of a
	// 4.2s cold start.
	if _, _, ierr := p.dockerClient.ImageInspectWithRaw(ctx, imageName); ierr != nil {
		if rc, perr := p.dockerClient.ImagePull(ctx, imageName, image.PullOptions{}); perr == nil {
			_, _ = io.Copy(io.Discard, rc)
			_ = rc.Close()
		} else {
			log.Printf("poder: pull image %q failed (continuing, may exist locally): %v", imageName, perr)
		}
	}

	// Egress enforcement, when the sandbox was created with a policy: a
	// per-pod internal network plus a proxy on its gateway. This runs before
	// the container exists so a sandbox never starts on a network it should
	// not have had; if it fails, the create fails (see egress.go).
	containerNetwork := p.networkName
	env := []string{
		"TOOLBOX_API_URL=" + req.APIURL,
		"PODER_VERSION=" + req.PoderVersion,
		"LOG_LEVEL=" + req.LogLevel,
		// The sandbox name drives the OAuth redirect_uri: the bridge constructs
		// "https://7099-{name}.{domain}/callback" from it, so a browser's OAuth
		// redirect reaches this sandbox's bridge through the E2B gateway's port
		// proxy. Without the name the bridge would build a loopback URL that no
		// browser can reach.
		"SANDRPOD_SANDBOX_NAME=" + req.Name,
	}
	// The wildcard domain comes from the worker's own env (operators set it once
	// on the worker container; the pode forwards it to every sandbox it creates).
	if dom := os.Getenv("SANDRPOD_E2B_DOMAIN"); dom != "" {
		env = append(env, "SANDRPOD_E2B_DOMAIN="+dom)
	}
	if req.Network != nil {
		enforced, eerr := p.setupEgress(ctx, podID, req.Network)
		if eerr != nil {
			return nil, eerr
		}
		containerNetwork = egressNetworkName(podID)
		env = append(env, enforced.env...)
		p.putEgress(podID, enforced)
		// A failure past this point must not leave the proxy and the network
		// behind; every early return below tears them down.
		defer func() {
			if retErr != nil {
				p.teardownEgress(ctx, podID)
			}
		}()
	}

	containerConfig := &container.Config{
		Image:  imageName,
		Env:    env,
		Labels: labels,
	}

	hostConfig := &container.HostConfig{}
	// A named volume rather than a bind mount: it survives container removal
	// (image upgrades, create retries) and Docker keeps it out of the host
	// namespace. The name is derived from the sandbox so a retried create
	// reattaches the same data instead of starting from scratch.
	if req.PersistData {
		wsVol := WorkspaceVolumeName(req.Name)
		// A sandbox created from a snapshot inherits the image layers but not the
		// source's volume (docker commit excludes mounts). Copy it so a "fork"
		// actually carries the working state it was forked from.
		if src := p.snapshotSource(ctx, imageName); src != "" {
			if err := p.copyVolume(ctx, WorkspaceVolumeName(src), wsVol); err != nil {
				log.Printf("poder: copy snapshot workspace %s -> %s failed (fork starts with an empty /workspace): %v", src, req.Name, err)
			} else {
				log.Printf("poder: forked workspace from %s into volume %s", src, wsVol)
			}
		}
		hostConfig.Mounts = append(hostConfig.Mounts, mount.Mount{
			Type:   mount.TypeVolume,
			Source: wsVol,
			Target: "/workspace",
		})
		log.Printf("poder: sandbox %s persists /workspace in volume %s", req.Name, wsVol)
	}
	if containerNetwork != "" {
		hostConfig.NetworkMode = container.NetworkMode(containerNetwork)
	}
	// Per-sandbox resource limits (noisy-neighbor isolation). NanoCPUs is CPU
	// cores × 1e9; Memory is in bytes. Both are ignored by Docker when zero.
	if req.CPUCores > 0 {
		hostConfig.NanoCPUs = int64(req.CPUCores * 1e9)
	}
	if req.MemoryMB > 0 {
		hostConfig.Memory = req.MemoryMB * 1024 * 1024
	}
	if req.GPU > 0 {
		// The nvidia container runtime injects the driver and maps /dev/nvidia*.
		// Count -1 would mean "all"; we allocate exactly what was asked for so
		// two GPU sandboxes cannot collide on the same device.
		hostConfig.DeviceRequests = []container.DeviceRequest{{
			Driver:       "nvidia",
			Count:        req.GPU,
			Capabilities: [][]string{{"gpu"}},
		}}
	}

	resp, err := p.dockerClient.ContainerCreate(ctx, containerConfig, hostConfig, nil, nil, podID)
	if err != nil {
		retErr = fmt.Errorf("failed to create container: %w", err)
		return nil, retErr
	}
	// From here on a failure must not leave a container behind: it is attached
	// to the per-pod network, so leaving it would also make that network
	// impossible to remove, and the next CreatePod with the same name would
	// fail on a name collision. Registered after the egress teardown so it
	// runs first (defers are LIFO) and the network can actually go away.
	defer func() {
		if retErr != nil {
			_ = p.dockerClient.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		}
	}()

	// Start the container
	if err := p.dockerClient.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		retErr = fmt.Errorf("failed to start container: %w", err)
		return nil, retErr
	}

	// Inspect the container to get runtime details
	containerInfo, err := p.dockerClient.ContainerInspect(ctx, resp.ID)
	if err != nil {
		retErr = fmt.Errorf("failed to inspect container: %w", err)
		return nil, retErr
	}

	// Resolve container IP: prefer the network the container actually joined
	// (the per-pod egress network when one was created); fall back to the
	// first available network IP.
	ip := ""
	if containerNetwork != "" {
		if netSettings, ok := containerInfo.NetworkSettings.Networks[containerNetwork]; ok {
			ip = netSettings.IPAddress
		}
	}
	if ip == "" {
		for _, netSettings := range containerInfo.NetworkSettings.Networks {
			if netSettings.IPAddress != "" {
				ip = netSettings.IPAddress
				break
			}
		}
	}

	// Build PodInfo
	pod = &PodInfo{
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

// DeletePod stops and removes the container for the given pod, then releases
// the pod's egress proxy and internal network (if it had them).
func (p *DockerPoder) DeletePod(ctx context.Context, podID string) error {
	// Egress objects are named after the container's *name* (sp-<ts>-<rand>,
	// assigned before the daemon handed back an ID), while the control plane
	// deletes by container ID. Resolve the name before removal — afterwards there
	// is nothing left to inspect — or the sidecar and its bridge network survive
	// every delete, silently, because "not found" was the expected outcome.
	egressKey := podID
	if info, err := p.dockerClient.ContainerInspect(ctx, podID); err == nil && info.Name != "" {
		egressKey = strings.TrimPrefix(info.Name, "/")
	}

	// The workspace volume is named after the sandbox, and the sandbox name
	// lives on the container label — resolve it before the container is gone.
	wsVol := ""
	if info, err := p.dockerClient.ContainerInspect(ctx, podID); err == nil {
		wsVol = WorkspaceVolumeName(info.Config.Labels["sandrpod/sandbox-name"])
	}

	// Stop the container gracefully
	_ = p.dockerClient.ContainerStop(ctx, podID, container.StopOptions{Timeout: ptr(10)})

	// Remove the container
	if err := p.dockerClient.ContainerRemove(ctx, podID, container.RemoveOptions{Force: true}); err != nil {
		// The network cannot be removed while a container is attached, and
		// the proxy is useless without its container: still try to release
		// both, then report the container failure.
		p.teardownEgress(ctx, egressKey)
		return fmt.Errorf("failed to remove container: %w", err)
	}

	p.teardownEgress(ctx, egressKey)
	// An explicit delete removes the data too: a kept volume with no sandbox is
	// exactly the leak the cloud sweep exists to prevent, one layer down.
	if wsVol != "" && wsVol != WorkspaceVolumeName("") {
		if err := p.dockerClient.VolumeRemove(ctx, wsVol, true); err != nil {
			if !os.IsNotExist(err) {
				log.Printf("poder: remove workspace volume %s: %v", wsVol, err)
			}
		} else {
			log.Printf("poder: removed workspace volume %s", wsVol)
		}
	}
	p.UnregisterPod(podID)
	return nil
}

// PausePod releases the container's CPU and memory by stopping it. It used to
// call ContainerPause, which freezes the processes but keeps every byte of
// memory resident — the opposite of what "stop an idle sandbox" is for. The
// container and its /workspace volume stay, so UnpausePod resumes in place with
// the data intact.
func (p *DockerPoder) PausePod(ctx context.Context, podID string) error {
	if err := p.dockerClient.ContainerStop(ctx, podID, container.StopOptions{Timeout: ptr(10)}); err != nil {
		return fmt.Errorf("failed to stop container: %w", err)
	}
	return nil
}

// UnpausePod resumes a stopped pod (docker start), reattaching its volume.
func (p *DockerPoder) UnpausePod(ctx context.Context, podID string) error {
	if err := p.dockerClient.ContainerStart(ctx, podID, container.StartOptions{}); err != nil {
		return fmt.Errorf("failed to start container: %w", err)
	}
	return nil
}

// snapshotSource returns the sandbox name an image was committed from, if the
// image carries the snapshot label.
func (p *DockerPoder) snapshotSource(ctx context.Context, imageName string) string {
	info, _, err := p.dockerClient.ImageInspectWithRaw(ctx, imageName)
	if err != nil || info.Config == nil {
		return ""
	}
	return info.Config.Labels["sandrpod.snapshot-of"]
}

// SnapshotInfo describes one snapshot image as the console needs it.
type SnapshotInfo struct {
	Image     string `json:"image"`
	Tag       string `json:"tag"`
	Source    string `json:"source"` // sandbox it was committed from
	CreatedAt int64  `json:"created_at"`
	SizeBytes int64  `json:"size_bytes"`
}

// SnapshotImages lists the images SandrPod committed with SnapshotPod, newest
// first. The label is what makes them OURS: a host may carry unrelated images.
func (p *DockerPoder) SnapshotImages(ctx context.Context) ([]SnapshotInfo, error) {
	list, err := p.dockerClient.ImageList(ctx, image.ListOptions{All: true})
	if err != nil {
		return nil, err
	}
	out := make([]SnapshotInfo, 0, len(list))
	for _, img := range list {
		src := img.Labels["sandrpod.snapshot-of"]
		if src == "" {
			continue
		}
		for _, tag := range img.RepoTags {
			if strings.HasPrefix(tag, "<none>") {
				continue
			}
			out = append(out, SnapshotInfo{
				Image: tag, Tag: tag, Source: src,
				CreatedAt: img.Created, SizeBytes: img.Size,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out, nil
}

// HasGPU reports whether this docker daemon can allocate NVIDIA GPUs: the nvidia
// container runtime must be registered. The pode usually runs in a container of
// its own, so /dev/nvidia0 is not visible from inside it — the daemon's runtime
// list is the view that actually predicts whether --gpus will work.
func (p *DockerPoder) HasGPU(ctx context.Context) bool {
	info, err := p.dockerClient.Info(ctx)
	if err != nil || info.Runtimes == nil {
		return false
	}
	_, ok := info.Runtimes["nvidia"]
	return ok
}

// toolboxImage is the image used for one-shot helper containers (volume copies).
func (p *DockerPoder) toolboxImage() string {
	if img := os.Getenv("SANDRPOD_TOOLBOX_IMAGE"); img != "" {
		return img
	}
	return "sandrpod-toolbox:v2"
}

// copyVolume copies one docker volume into another using a throwaway container
// from the toolbox image (already present on a worker; no registry round-trip).
// It is best-effort: a fork with an empty workspace is still a usable sandbox.
func (p *DockerPoder) copyVolume(ctx context.Context, from, to string) error {
	// Entrypoint, not Cmd: the toolbox image's entrypoint IS the toolbox binary,
	// so a Cmd rode along with it and the helper container never exited — the
	// copy waited forever and the create hung in PENDING.
	resp, err := p.dockerClient.ContainerCreate(ctx,
		&container.Config{
			Image:      p.toolboxImage(),
			Entrypoint: []string{"sh", "-c", "cp -a /from/. /to/ 2>/dev/null || true"},
		},
		&container.HostConfig{Mounts: []mount.Mount{
			{Type: mount.TypeVolume, Source: from, Target: "/from", ReadOnly: true},
			{Type: mount.TypeVolume, Source: to, Target: "/to"},
		}},
		nil, nil, "")
	if err != nil {
		return err
	}
	defer func() {
		_ = p.dockerClient.ContainerRemove(context.Background(), resp.ID, container.RemoveOptions{Force: true})
	}()
	if err := p.dockerClient.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return err
	}
	copyCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	statusCh, errCh := p.dockerClient.ContainerWait(copyCtx, resp.ID, container.WaitConditionNotRunning)
	select {
	case <-copyCtx.Done():
		return fmt.Errorf("volume copy timed out")
	case err := <-errCh:
		return err
	case st := <-statusCh:
		if st.StatusCode != 0 {
			return fmt.Errorf("volume copy exited %d", st.StatusCode)
		}
	}
	return nil
}

// WorkspaceVolumeName is the volume a sandbox's /workspace lives in. Derived
// from the sandbox name so a retried create reattaches the same data, and so
// DeletePod can remove exactly the right volume.
func WorkspaceVolumeName(sandboxName string) string {
	repl := strings.NewReplacer("/", "-", " ", "-", ":", "-")
	return "sandrpod-ws-" + repl.Replace(sandboxName)
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
	// Record which sandbox this image came from: docker commit does not include
	// mounted volumes, so a fork from this image starts with an EMPTY /workspace
	// unless the create path copies the source volume across. The label is how
	// it knows which one to copy.
	changes := []string{}
	if info, err := p.dockerClient.ContainerInspect(ctx, podID); err == nil {
		if src := info.Config.Labels["sandrpod/sandbox-name"]; src != "" {
			changes = append(changes, "LABEL sandrpod.snapshot-of="+src)
		}
	}
	if _, err := p.dockerClient.ContainerCommit(ctx, podID, container.CommitOptions{
		Reference: ref.String(),
		Comment:   "sandrpod snapshot",
		Changes:   changes,
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
	} else if info.State.ExitCode == 137 || info.State.ExitCode == 143 {
		// docker stop terminates with SIGKILL(137)/SIGTERM(143) when the
		// entrypoint has no trap. That is a normal stop, not a crash: treating it
		// as ERROR made "stop then start" impossible, because the start path only
		// resumes a STOPPED pod and then waited out its five-minute timeout.
		pod.State = PodStateStopped
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

// findContainerBySandboxName returns the ID of the container that already serves
// req.Name, if any. The label is what CreatePod stamps, so it survives restarts.
func (p *DockerPoder) findContainerBySandboxName(ctx context.Context, name string) (string, error) {
	containers, err := p.dockerClient.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", "sandrpod/sandbox-name="+name)),
	})
	if err != nil {
		return "", err
	}
	if len(containers) == 0 {
		return "", nil
	}
	return containers[0].ID, nil
}
