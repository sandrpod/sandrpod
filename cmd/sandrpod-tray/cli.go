// Copyright 2026 SandrPod
// CLI subcommands for sandrpod-tray.
//
// These are headless operations on permissions.json — no GUI, no IPC. They
// are safe to run while the tray daemon is also running because Store uses
// atomic file replacement (tmp+rename); a concurrent tray process will
// observe a clean snapshot on its next read.

package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/sandrpod/sandrpod/pkg/permission"
)

// reorderFlagsFirst moves all flag-looking tokens (-x or --x[=...]) to the
// front of argv so the stdlib flag package's "stop at first positional"
// behavior doesn't trip ordinary users who type `cmd <path> --mode rw`.
//
// We don't try to handle the `-flag value` (separate-token) form for flags
// that come after a positional — because stdlib flag's API doesn't tell us
// in advance which flags take a value, the heuristic gets fragile. Users
// who need that form can use `--flag=value` which we handle correctly.
func reorderFlagsFirst(args []string) []string {
	flags := make([]string, 0, len(args))
	rest := make([]string, 0, len(args))
	skipNext := false
	for i, a := range args {
		if skipNext {
			flags = append(flags, a)
			skipNext = false
			continue
		}
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			// If it looks like `-flag value` (no `=` in the token, and the
			// next token is not another flag and not the last positional)
			// pull the value too. This is best-effort; flags whose value
			// happens to start with `-` will be misparsed.
			if !strings.Contains(a, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				skipNext = true
			}
			continue
		}
		rest = append(rest, a)
	}
	return append(flags, rest...)
}

func mustLoadStore() *permission.Store {
	store, err := permission.LoadStore(storePath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "load permissions.json:", err)
		os.Exit(1)
	}
	return store
}

// runUnlock removes a hardlock. We deliberately require an explicit
// --i-understand-the-risk flag so an employee can't accidentally `tray
// unlock ~/.ssh` after auto-completing a path. This is the ONLY documented
// way to escape a hardlock — the GUI cannot do it.
func runUnlock(args []string) {
	fs := flag.NewFlagSet("unlock", flag.ExitOnError)
	confirm := fs.Bool("i-understand-the-risk", false, "explicit acknowledgement that you understand removing a hardlock exposes sensitive paths to the AI agent")
	_ = fs.Parse(reorderFlagsFirst(args))

	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: sandrpod-tray unlock <path> --i-understand-the-risk")
		os.Exit(2)
	}
	if !*confirm {
		fmt.Fprintln(os.Stderr, "refusing: --i-understand-the-risk is required to remove a hardlock")
		os.Exit(2)
	}

	store := mustLoadStore()
	path := fs.Arg(0)
	if err := store.RemoveRule(path, true /* removeHardlock */); err != nil {
		fmt.Fprintln(os.Stderr, "unlock:", err)
		os.Exit(1)
	}
	fmt.Printf("unlocked %s — AI agent can now request access (will still prompt on first use)\n", path)
}

// runLock installs a hardlock entry. No --i-understand-the-risk needed —
// adding restrictions is always safe.
func runLock(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: sandrpod-tray lock <path>")
		os.Exit(2)
	}
	store := mustLoadStore()
	if err := store.AddHardlock(args[0]); err != nil {
		fmt.Fprintln(os.Stderr, "lock:", err)
		os.Exit(1)
	}
	fmt.Printf("locked %s — agent denials will be silent (no consent prompt)\n", args[0])
}

// runRules dispatches `rules ls/add/rm`.
func runRules(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: sandrpod-tray rules ls|add|rm [...]")
		os.Exit(2)
	}
	switch args[0] {
	case "ls":
		runRulesList()
	case "add":
		runRulesAdd(args[1:])
	case "rm":
		runRulesRemove(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown rules subcommand: %q\n", args[0])
		os.Exit(2)
	}
}

func runRulesList() {
	store := mustLoadStore()
	snap := store.Snapshot()

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer tw.Flush()
	fmt.Fprintln(tw, "SCOPE\tMODE\tPATH\tNOTE")
	for _, r := range snap.Rules {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Scope, r.Mode, r.Path, r.Note)
	}
	for _, r := range snap.SessionGrants {
		fmt.Fprintf(tw, "%s(%s)\t%s\t%s\t%s\n", r.Scope, r.SessionID, r.Mode, r.Path, r.Note)
	}
	if len(snap.Rules)+len(snap.SessionGrants) == 0 {
		fmt.Fprintln(tw, "(no rules — run `sandrpod-tray seed` to install default hardlocks)")
	}
}

func runRulesAdd(args []string) {
	fs := flag.NewFlagSet("rules add", flag.ExitOnError)
	mode := fs.String("mode", "rw", "access mode: r | w | rw")
	_ = fs.Parse(reorderFlagsFirst(args))

	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: sandrpod-tray rules add <path> [--mode r|w|rw]")
		os.Exit(2)
	}
	m := permission.Mode(strings.ToLower(*mode))
	switch m {
	case permission.ModeRead, permission.ModeWrite, permission.ModeReadWrite:
	default:
		fmt.Fprintf(os.Stderr, "invalid --mode %q (expected r, w, or rw)\n", *mode)
		os.Exit(2)
	}

	store := mustLoadStore()
	if err := store.AddPermanentRule(permission.Rule{
		Path: fs.Arg(0),
		Mode: m,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "rules add:", err)
		os.Exit(1)
	}
	fmt.Printf("granted permanent %s access to %s\n", m, fs.Arg(0))
}

func runRulesRemove(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: sandrpod-tray rules rm <path>")
		os.Exit(2)
	}
	store := mustLoadStore()
	if err := store.RemoveRule(args[0], false /* refuse hardlocks */); err != nil {
		fmt.Fprintln(os.Stderr, "rules rm:", err)
		os.Exit(1)
	}
	fmt.Printf("removed rule for %s (hardlocks unaffected; use `unlock` for those)\n", args[0])
}

// runSeed force-installs the default hardlock seeds. Useful for provisioning
// scripts that need to guarantee baseline protection on a fresh install
// before the user ever opens the tray.
func runSeed() {
	store := mustLoadStore()
	added, err := permission.SeedHardlocksIfEmpty(store)
	if err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		os.Exit(1)
	}
	if added == 0 {
		fmt.Println("permissions.json already has rules — refusing to overwrite (use `rules rm` first to start clean)")
		return
	}
	fmt.Printf("seeded %d default hardlock entries\n", added)
}
