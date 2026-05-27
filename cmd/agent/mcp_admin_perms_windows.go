//go:build windows

package main

// tightenSocketPerms is a no-op on Windows: Go's os.Chmod only maps to the
// read-only bit (which we don't want to set), and AF_UNIX sockets on
// Windows inherit their parent directory's ACLs — restricting access via
// the discretionary parent-dir ACL (typically per-user under %USERPROFILE%)
// is the right control here.
func tightenSocketPerms(path string) {}
