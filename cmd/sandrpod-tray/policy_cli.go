// Copyright 2026 SandrPod
// `sandrpod-tray policy ...` — manage command deny/warn lists from CLI.

package main

import (
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/sandrpod/sandrpod/pkg/permission"
)

func runPolicy(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: sandrpod-tray policy ls|deny|warn|rm [...]")
		os.Exit(2)
	}
	switch args[0] {
	case "ls":
		runPolicyList()
	case "deny":
		runPolicyAdd(args[1:], true)
	case "warn":
		runPolicyAdd(args[1:], false)
	case "rm":
		runPolicyRemove(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown policy subcommand: %q\n", args[0])
		os.Exit(2)
	}
}

func runPolicyList() {
	store := mustLoadStore()
	policy := store.Snapshot().CommandPolicy

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer tw.Flush()
	fmt.Fprintln(tw, "ACTION\tCOMMAND")
	denies := append([]string(nil), policy.Deny...)
	sort.Strings(denies)
	for _, d := range denies {
		fmt.Fprintf(tw, "deny\t%s\n", d)
	}
	warns := append([]string(nil), policy.Warn...)
	sort.Strings(warns)
	for _, w := range warns {
		fmt.Fprintf(tw, "warn\t%s\n", w)
	}
	if len(denies)+len(warns) == 0 {
		fmt.Fprintln(tw, "(empty — run `sandrpod-tray seed` then re-run to install defaults)")
	}
}

// runPolicyAdd adds a command to the deny or warn list, depending on `deny`.
// We canonicalize via normalizeCommandName so users can pass `scp.exe` and
// it ends up stored as `scp` (matching the runtime tokenizer).
func runPolicyAdd(args []string, deny bool) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: sandrpod-tray policy deny|warn <command-name>")
		os.Exit(2)
	}
	name := args[0]

	store := mustLoadStore()
	cur := store.Snapshot().CommandPolicy

	upd := upsertCommand(cur, name, deny)
	if err := store.SetCommandPolicy(upd); err != nil {
		fmt.Fprintln(os.Stderr, "policy:", err)
		os.Exit(1)
	}
	action := "warn"
	if deny {
		action = "deny"
	}
	fmt.Printf("%s: added %q to %s list\n", action, name, action)
}

func runPolicyRemove(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: sandrpod-tray policy rm <command-name>")
		os.Exit(2)
	}
	name := args[0]
	store := mustLoadStore()
	cur := store.Snapshot().CommandPolicy

	cur.Deny = removeFromList(cur.Deny, name)
	cur.Warn = removeFromList(cur.Warn, name)
	if err := store.SetCommandPolicy(cur); err != nil {
		fmt.Fprintln(os.Stderr, "policy rm:", err)
		os.Exit(1)
	}
	fmt.Printf("removed %q from deny+warn lists\n", name)
}

// upsertCommand inserts `name` into the appropriate list, removing it from
// the other list to avoid contradiction.
func upsertCommand(cur permission.CommandPolicy, name string, deny bool) permission.CommandPolicy {
	if deny {
		cur.Deny = appendUnique(cur.Deny, name)
		cur.Warn = removeFromList(cur.Warn, name)
	} else {
		cur.Warn = appendUnique(cur.Warn, name)
		cur.Deny = removeFromList(cur.Deny, name)
	}
	return cur
}

func appendUnique(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

func removeFromList(list []string, v string) []string {
	out := list[:0]
	for _, x := range list {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}
