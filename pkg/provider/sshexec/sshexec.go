// Copyright 2024 SandrPod
// Shared SSH remote-execution helper for providers that have no managed
// run-command API (DigitalOcean, Hetzner, ...). The VM lifecycle differs per
// provider, but the "connect over SSH with a per-VM ephemeral key and run a
// command as root" part is identical, so it lives here.
//
// The GCP provider predates this package and keeps its own copy (it injects the
// key via instance metadata and runs as a non-root sudoer); this helper targets
// providers that boot with a root login and inject the key via cloud-init.

package sshexec

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
)

const (
	dialTimeout        = 15 * time.Second
	registrationWindow = 3 * time.Minute // retry dialing while sshd is still coming up
	defaultExecTimeout = 5 * time.Minute
)

// GenerateEd25519 creates an ephemeral ed25519 key pair, returning a signer for
// the private half and the authorized-keys line for the public half (suffixed
// with an identifying comment).
func GenerateEd25519(comment string) (ssh.Signer, string, error) {
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
	authKey := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
	if comment != "" {
		authKey += " " + comment
	}
	return signer, authKey, nil
}

// CloudInitRootKey builds a cloud-init user-data document that installs the
// given authorized key for root, so ExecuteCommand can SSH in as root. Used by
// providers (DigitalOcean, Hetzner) whose images boot with a root login and
// take cloud-init user-data.
func CloudInitRootKey(authKey string) string {
	// Keys from GenerateEd25519 are base64 + a fixed comment and can't contain
	// a single quote, but strip defensively — a quote would break out of the
	// shell quoting below and inject into the cloud-init runcmd.
	authKey = strings.ReplaceAll(authKey, "'", "")
	return "#cloud-config\nruncmd:\n" +
		"  - mkdir -p /root/.ssh && chmod 700 /root/.ssh\n" +
		fmt.Sprintf("  - echo '%s' >> /root/.ssh/authorized_keys\n", authKey) +
		"  - chmod 600 /root/.ssh/authorized_keys\n"
}

// Config configures a single Run invocation.
type Config struct {
	User   string     // SSH login user (e.g. "root")
	Signer ssh.Signer // private-key signer
	Sudo   bool       // wrap the command in `sudo -n bash` (for non-root users)
}

// Result holds the outcome of a remotely executed command.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Run connects to host:22 and runs command, retrying the dial while a freshly
// booted VM's sshd is still starting. A non-zero command exit is returned via
// Result.ExitCode (not an error); only transport/session failures error.
func Run(ctx context.Context, host string, cfg Config, command string) (*Result, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultExecTimeout)
		defer cancel()
	}

	addr := net.JoinHostPort(host, "22")
	clientCfg := &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(cfg.Signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // ephemeral first-boot host, nothing to pin
		Timeout:         dialTimeout,
	}

	deadline := time.Now().Add(registrationWindow)
	for {
		client, err := ssh.Dial("tcp", addr, clientCfg)
		if err == nil {
			defer client.Close()
			return runOnce(ctx, client, cfg, command)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("could not SSH to %s before timeout: %w", addr, err)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("ssh to %s cancelled: %w", addr, ctx.Err())
		case <-time.After(5 * time.Second):
		}
	}
}

func runOnce(ctx context.Context, client *ssh.Client, cfg Config, command string) (*Result, error) {
	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("failed to open ssh session: %w", err)
	}
	defer session.Close()

	// session.Run has no context support, so a hung remote command would block
	// forever past the caller's deadline. Watch ctx and tear the connection
	// down on cancellation, which forces Run to return.
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

	runTarget := command
	if cfg.Sudo {
		// Pipe the command into `sudo -n bash` over stdin so it runs as root and
		// arbitrary command bodies don't need shell-quoting.
		session.Stdin = strings.NewReader(command + "\n")
		runTarget = "sudo -n bash"
	}

	exitCode := 0
	if runErr := session.Run(runTarget); runErr != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("ssh command cancelled: %w", ctx.Err())
		}
		var exitErr *ssh.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitStatus()
		} else {
			return nil, fmt.Errorf("ssh command failed: %w", runErr)
		}
	}
	return &Result{
		Stdout:   strings.TrimSpace(stdout.String()),
		Stderr:   strings.TrimSpace(stderr.String()),
		ExitCode: exitCode,
	}, nil
}
