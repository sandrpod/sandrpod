// Copyright 2026 SandrPod
// sandrpod-tray — user-session GUI companion to sandrpod-agent.
//
// Responsibilities:
//   - Render the macOS / Linux / Windows tray icon and menu.
//   - Listen on the per-user IPC socket and answer permission asks coming
//     from sandrpod-agent (running as a separate process) by displaying a
//     native consent dialog.
//   - Expose a minimal local HTTP settings page (opened in the default
//     browser when the employee clicks "授权管理…") that lists existing
//     rules, supports adding/removing permanent grants, and shows hardlocks
//     as read-only.
//   - Provide CLI subcommands (`unlock`, `lock`, `rules ls/add/rm`, `seed`)
//     for power users and provisioning scripts that don't go through the GUI.
//
// Runtime layout:
//
//   sandrpod-tray serve    (default; brings up tray + IPC + HTTP)
//   sandrpod-tray unlock <path> --i-understand-the-risk
//   sandrpod-tray lock <path>
//   sandrpod-tray rules ls
//   sandrpod-tray rules add <path> --mode rw
//   sandrpod-tray rules rm <path>
//   sandrpod-tray seed         (force-write default hardlock seeds)
//
// All subcommands operate on $HOME/.sandrpod/permissions.json (override via
// SANDRPOD_PERMISSION_FILE).
package main

import (
	"fmt"
	"os"

	"github.com/sandrpod/sandrpod/pkg/permission"
)

func storePath() string {
	if v := os.Getenv("SANDRPOD_PERMISSION_FILE"); v != "" {
		return v
	}
	p, err := permission.DefaultStorePath()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal: cannot resolve home dir:", err)
		os.Exit(1)
	}
	return p
}

func socketPath() string {
	if v := os.Getenv("SANDRPOD_AUTHZ_SOCKET"); v != "" {
		return v
	}
	p, err := permission.DefaultSocketPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal: cannot resolve home dir:", err)
		os.Exit(1)
	}
	return p
}

func usage() {
	fmt.Fprint(os.Stderr, `sandrpod-tray — Acme Sandbox consent UI

Usage:
  sandrpod-tray [serve]                    — run the tray icon + IPC server (default)
  sandrpod-tray unlock <path> --i-understand-the-risk
                                           — remove a hardlock entry (DANGEROUS)
  sandrpod-tray lock <path>                — install a hardlock entry
  sandrpod-tray rules ls                   — list permanent rules and hardlocks
  sandrpod-tray rules add <path> [--mode r|w|rw]
                                           — grant a permanent rule
  sandrpod-tray rules rm <path>            — remove a permanent rule (refuses hardlocks)
  sandrpod-tray policy ls                  — show command deny/warn lists
  sandrpod-tray policy deny <cmd>          — add command to denylist
  sandrpod-tray policy warn <cmd>          — add command to warnlist
  sandrpod-tray policy rm <cmd>            — remove command from both lists
  sandrpod-tray seed                       — install default hardlock seeds (first-run)
  sandrpod-tray help                       — show this message

Environment:
  SANDRPOD_PERMISSION_FILE  override permissions.json location
  SANDRPOD_AUTHZ_SOCKET     override Unix socket path

Files:
  ~/.sandrpod/permissions.json   on-disk policy (chmod 0600)
  ~/.sandrpod/authz.sock         IPC socket (chmod 0600)
`)
}

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
	}

	switch cmd {
	case "serve", "":
		runServe()
	case "unlock":
		runUnlock(args[1:])
	case "lock":
		runLock(args[1:])
	case "rules":
		runRules(args[1:])
	case "policy":
		runPolicy(args[1:])
	case "seed":
		runSeed()
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}
