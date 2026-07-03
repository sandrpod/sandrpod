//go:build !windows

// Copyright 2024 SandrPod
package toolbox

import (
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// collect streams a process to completion and returns joined stdout + the exit
// code, failing if the end event does not arrive within the deadline.
func collect(t *testing.T, m *ProcManager, pid uint32) (string, int32) {
	t.Helper()
	var (
		mu   sync.Mutex
		out  strings.Builder
		exit int32
		done = make(chan struct{})
	)
	go func() {
		_, _ = m.Stream(pid, func(ev ProcEvent) error {
			mu.Lock()
			defer mu.Unlock()
			switch ev.Type {
			case "stdout", "pty":
				out.Write(ev.Data)
			case "end":
				exit = ev.ExitCode
				close(done)
			}
			return nil
		})
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for process end event")
	}
	mu.Lock()
	defer mu.Unlock()
	return out.String(), exit
}

func TestProcManagerForegroundEcho(t *testing.T) {
	m := NewProcManager()
	pid, err := m.Start(ProcStartConfig{Cmd: "/bin/sh", Args: []string{"-c", "echo hello world"}})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	out, exit := collect(t, m, pid)
	if strings.TrimSpace(out) != "hello world" {
		t.Fatalf("stdout = %q, want %q", out, "hello world")
	}
	if exit != 0 {
		t.Fatalf("exit = %d, want 0", exit)
	}
}

func TestProcManagerStdinRoundtrip(t *testing.T) {
	m := NewProcManager()
	// `cat` echoes stdin to stdout until EOF.
	pid, err := m.Start(ProcStartConfig{Cmd: "/bin/cat"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !m.SendInput(pid, []byte("ping\n"), false) {
		t.Fatal("SendInput returned false")
	}
	if !m.CloseStdin(pid) { // EOF so cat exits
		t.Fatal("CloseStdin returned false")
	}
	out, exit := collect(t, m, pid)
	if strings.TrimSpace(out) != "ping" {
		t.Fatalf("stdout = %q, want %q", out, "ping")
	}
	if exit != 0 {
		t.Fatalf("exit = %d, want 0", exit)
	}
}

func TestProcManagerKillBackground(t *testing.T) {
	m := NewProcManager()
	// A long sleep stands in for a background process.
	pid, err := m.Start(ProcStartConfig{Cmd: "/bin/sh", Args: []string{"-c", "sleep 30"}})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	// It should show up in the list while running.
	if procs := m.List(); len(procs) != 1 || procs[0].PID != pid {
		t.Fatalf("List = %+v, want one entry for pid %d", procs, pid)
	}
	if !m.Signal(pid, syscall.SIGKILL) {
		t.Fatal("Signal returned false")
	}
	_, exit := collect(t, m, pid)
	if exit == 0 {
		t.Fatal("expected non-zero exit after SIGKILL")
	}
}
