// Copyright 2024 SandrPod
// GCP remote execution over SSH.
//
// GCP has no managed run-command service, so ExecuteCommand connects to the VM
// over SSH using the ephemeral key generated at CreateVM time. This is the
// project's second execution backend (the other three clouds use agent-based
// managed APIs), kept behind the same Provider.ExecuteCommand interface.

package gcp

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/sandrpod/sandrpod/pkg/provider"
)

// sshCred is the per-VM SSH identity: the Linux user and the private-key signer.
type sshCred struct {
	user   string
	signer ssh.Signer
}

// generateSSHKey creates an ephemeral ed25519 key pair. It returns a signer for
// the private half and the authorized-keys line for the public half (suffixed
// with a comment so it's identifiable in the VM's authorized_keys).
func generateSSHKey(user string) (ssh.Signer, string, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, "", err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, "", err
	}
	authKey := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " sandrpod-" + user
	return signer, authKey, nil
}

// sshDialTimeout bounds a single TCP+handshake attempt.
const sshDialTimeout = 15 * time.Second

// sshRegistrationTimeout bounds how long ExecuteCommand retries dialing while a
// freshly booted VM's sshd is still coming up (connection refused/timeouts).
const sshRegistrationTimeout = 3 * time.Minute

// sshExecTimeout bounds the whole ExecuteCommand call when the caller's context
// carries no deadline — long enough for slow bootstrap commands.
const sshExecTimeout = 5 * time.Minute

// ExecuteCommand runs a shell command on the VM over SSH and returns its output
// and real exit code (via *ssh.ExitError), matching the semantics of the
// managed-agent providers.
func (p *GCPProvider) ExecuteCommand(ctx context.Context, vmID, command string) (*provider.CommandResult, error) {
	p.mu.RLock()
	cred, ok := p.sshCreds[vmID]
	p.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("no ssh credential for %s (VM created by a different process?)", vmID)
	}

	vm, err := p.GetVM(ctx, vmID)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve VM address: %w", err)
	}
	if vm.PublicIP == "" {
		return nil, fmt.Errorf("VM %s has no public IP to SSH to", vmID)
	}
	addr := net.JoinHostPort(vm.PublicIP, "22")

	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, sshExecTimeout)
		defer cancel()
	}

	config := &ssh.ClientConfig{
		User:            cred.user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(cred.signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // ephemeral first-boot host; no known_hosts to pin
		Timeout:         sshDialTimeout,
	}

	// Retry dialing while sshd is still starting on a fresh VM.
	dialDeadline := time.Now().Add(sshRegistrationTimeout)
	for {
		client, derr := ssh.Dial("tcp", addr, config)
		if derr == nil {
			defer client.Close()
			return runSSHCommand(ctx, client, command)
		}
		if time.Now().After(dialDeadline) {
			return nil, fmt.Errorf("could not SSH to %s before timeout: %w", addr, derr)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("ssh to %s cancelled: %w", addr, ctx.Err())
		case <-time.After(5 * time.Second):
		}
	}
}

// runSSHCommand runs one command on an established client, capturing stdout,
// stderr, and the exit code. A non-zero exit surfaces as *ssh.ExitError, which
// is translated into ExitCode rather than an error — only transport/session
// failures return an error.
func runSSHCommand(ctx context.Context, client *ssh.Client, command string) (*provider.CommandResult, error) {
	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("failed to open ssh session: %w", err)
	}
	defer session.Close()

	// session.Run has no context support; watch ctx and tear the connection
	// down on cancellation so a hung remote command can't block forever.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			client.Close()
		case <-done:
		}
	}()

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr
	// Bootstrap commands (docker …) require root. The managed-agent providers
	// (SSM / CloudAssist / Azure Run Command) execute as root; match that by
	// piping the command into `sudo bash` over stdin — this runs as root and
	// sidesteps shell-quoting of the command body. A metadata-provisioned GCP
	// user is a passwordless sudoer (google-sudoers), so `sudo -n` succeeds.
	session.Stdin = strings.NewReader(command + "\n")

	exitCode := 0
	if runErr := session.Run("sudo -n bash"); runErr != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("ssh command cancelled: %w", ctx.Err())
		}
		var exitErr *ssh.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitStatus()
		} else {
			// Transport/session error (not a normal non-zero exit).
			return nil, fmt.Errorf("ssh command failed: %w", runErr)
		}
	}

	return &provider.CommandResult{
		Output:     strings.TrimSpace(stdout.String()),
		Stderr:     strings.TrimSpace(stderr.String()),
		ExitCode:   exitCode,
		ExecutedAt: time.Now(),
	}, nil
}
