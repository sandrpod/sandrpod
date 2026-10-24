// Copyright 2026 SandrPod Contributors
// Docker-backed egress enforcement, exercised against a real daemon.
//
// Opt-in: these pull and run containers, and they need outbound network access
// to prove an allow-list lets something through. Set SANDRPOD_DOCKER_TEST=1
// to run them:
//
//	docker build -t sandrpod-egress-test - <<'EOF'
//	FROM alpine:3.20
//	RUN apk add --no-cache curl ca-certificates
//	CMD ["sleep","infinity"]
//	EOF
//	SANDRPOD_DOCKER_TEST=1 go test ./pkg/poder -run Egress -v
package poder

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sandrpod/sandrpod/pkg/egress"
)

func dockerTestImage() string {
	if img := os.Getenv("SANDRPOD_EGRESS_TEST_IMAGE"); img != "" {
		return img
	}
	return "sandrpod-egress-test"
}

func requireDocker(t *testing.T) {
	t.Helper()
	if os.Getenv("SANDRPOD_DOCKER_TEST") == "" {
		t.Skip("set SANDRPOD_DOCKER_TEST=1 to run docker integration tests")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skipf("docker daemon not available: %v", err)
	}
	out, err := exec.Command("docker", "image", "inspect", dockerTestImage()).CombinedOutput()
	if err != nil {
		t.Skipf("test image %q missing (see the file header): %s", dockerTestImage(), out)
	}
}

// dockerExec runs a command inside the pod and returns its stdout plus exit code.
func dockerExec(t *testing.T, podID string, env []string, args ...string) (string, int) {
	t.Helper()
	full := append([]string{"exec"}, podID)
	full = append(full, args...)
	cmd := exec.Command("docker", full...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	out, err := cmd.CombinedOutput()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("docker exec: %v", err)
	}
	return string(out), code
}

// curl runs curl inside the pod and returns its HTTP status word.
func curl(t *testing.T, podID string, extraEnv []string, args ...string) (status string, code int) {
	t.Helper()
	base := []string{"curl", "-sS", "-m", "15", "-o", "/dev/null", "-w", "%{http_code}"}
	out, code := dockerExec(t, podID, extraEnv, append(base, args...)...)
	return strings.TrimSpace(out), code
}

func TestDockerPoder_EgressPolicyIsEnforced(t *testing.T) {
	requireDocker(t)

	p, err := NewDockerPoder("local", "")
	if err != nil {
		t.Fatalf("NewDockerPoder: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// A policy that allows exactly one host: everything else must fail, and
	// the container must have no direct route to prove it with.
	pod, err := p.CreatePod(ctx, &CreatePodRequest{
		Name:    "egress-it",
		ImageID: dockerTestImage(),
		Network: &egress.Policy{
			AllowedDomains: []string{"example.com"},
			DeniedDomains:  []string{"denied.example.com"},
		},
	})
	if err != nil {
		t.Fatalf("CreatePod with a policy: %v", err)
	}
	t.Cleanup(func() { _ = p.DeletePod(context.Background(), pod.ID) })

	if pod.IP == "" {
		t.Fatal("pod has no IP: the control path to the toolbox would be broken")
	}

	// 1. No proxy, straight at the internet: there must be no route.
	//    (--noproxy '*' makes curl ignore the environment for this request.)
	status, code := curl(t, pod.ID, nil, "--noproxy", "*", "http://example.com/")
	if code == 0 && status == "200" {
		t.Fatalf("BYPASS: the container reached example.com without the proxy (status %s)", status)
	}

	// 2. Through the proxy, a host the policy does not name: 403.
	status, _ = curl(t, pod.ID, nil, "http://example.org/")
	if status != "403" {
		t.Errorf("unlisted host through the proxy: status = %q, want 403", status)
	}

	// 3. Through the proxy, a denied host: 403 as well.
	status, _ = curl(t, pod.ID, nil, "http://denied.example.com/")
	if status != "403" {
		t.Errorf("denied host through the proxy: status = %q, want 403", status)
	}

	// 4. Through the proxy, the allowed host: it works.
	status, _ = curl(t, pod.ID, nil, "http://example.com/")
	if status != "200" {
		t.Errorf("allowed host through the proxy: status = %q, want 200", status)
	}
}

// TestDockerPoder_NoPolicyLeavesTheContainerAlone is the control case: without
// a policy the container keeps its ordinary network, so a regression that
// started isolating every sandbox would show up here.
func TestDockerPoder_NoPolicyLeavesTheContainerAlone(t *testing.T) {
	requireDocker(t)

	p, err := NewDockerPoder("local", "")
	if err != nil {
		t.Fatalf("NewDockerPoder: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	pod, err := p.CreatePod(ctx, &CreatePodRequest{Name: "no-egress-it", ImageID: dockerTestImage()})
	if err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	t.Cleanup(func() { _ = p.DeletePod(context.Background(), pod.ID) })

	status, _ := curl(t, pod.ID, nil, "--noproxy", "*", "http://example.com/")
	if status != "200" {
		t.Errorf("without a policy the container should reach the network directly, status = %q", status)
	}
}
