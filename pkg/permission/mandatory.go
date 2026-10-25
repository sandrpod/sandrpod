// Copyright 2026 SandrPod
// Mandatory deny: the handful of paths inside a work tree that an agent must
// never be able to write, whatever the employee granted it.
//
// The hardlock list protects what is *outside* the sandbox — ~/.ssh, ~/.aws,
// the gate's own state. This list protects what is *inside* it, because the
// work tree is where persistence hides: a line appended to .git/hooks/pre-commit
// survives every later sandbox, runs on the host on the next `git commit`, and
// is not something any permission dialog was ever about. The same is true of
// .bashrc, .mcp.json (which configures what the agent itself may run) and the
// IDE launch configs.
//
// So these are denied unconditionally — above work_dir, above permanent grants
// — on the same footing as a hardlock, and for the same reason: a rule that can
// be dialogs away is not a boundary. They stay *readable* (a build may need to
// read .git/config); only writes are refused.
//
// The set is deliberately small and boring. It is not "all the dangerous files
// on a machine" — that list does not exist — it is the ones whose only plausible
// purpose in an agent's work tree is persistence or privilege escalation.
package permission

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// dangerousFiles are base names that must not be written anywhere in the work
// tree: shell startup files, git config, ripgrep config (which can add
// --pre flags), and the MCP server definitions the agent itself consumes.
var dangerousFiles = []string{
	".bashrc",
	".bash_profile",
	".bash_login",
	".zshrc",
	".zprofile",
	".zshenv",
	".profile",
	".gitconfig",
	".gitmodules",
	".ripgreprc",
	".mcp.json",
}

// dangerousDirs are directories that must not be written anywhere in the work
// tree. They are checked by name at any depth: a nested repository's hooks are
// as dangerous as the top-level one's.
var dangerousDirs = []string{
	".vscode",
	".idea",
	filepath.Join(".claude", "commands"),
	filepath.Join(".claude", "agents"),
	filepath.Join(".git", "hooks"),
}

// dangerousFilesInGit are files inside a .git directory that must stay
// read-only: config can point at a hook path, and the hook machinery is how a
// commit on the host later runs an agent's code.
var dangerousFilesInGit = []string{
	filepath.Join(".git", "config"),
}

// IsMandatoryDenied reports whether path is one of the always-write-denied
// paths. The check is by name, at any depth, and applies inside work_dir.
//
// Symlinks are not followed here: the caller canonicalizes first (the manager
// does), so a link that lands on a protected name is judged by where it lands.
func IsMandatoryDenied(path string) bool {
	if path == "" {
		return false
	}
	clean := filepath.Clean(path)
	base := filepath.Base(clean)
	for _, f := range dangerousFiles {
		if strings.EqualFold(base, f) {
			return true
		}
	}
	// Directory rules: match on the trailing path segments so ".git/hooks/x"
	// and "/any/depth/.git/hooks/x" are both caught without a full walk.
	sep := string(filepath.Separator)
	for _, d := range dangerousDirs {
		suffix := sep + d
		if strings.HasSuffix(clean, suffix) || strings.Contains(clean, suffix+sep) {
			return true
		}
	}
	// A `.git` *file* (a worktree or submodule) is writable in itself, but
	// .git/config is not; and the .git indirection file points at the real
	// git dir, so writing it is equivalent to repointing the repository.
	if strings.EqualFold(base, ".git") {
		if info, err := os.Lstat(clean); err == nil && info.Mode().IsRegular() {
			return true
		}
	}
	for _, f := range dangerousFilesInGit {
		if strings.HasSuffix(clean, sep+f) {
			return true
		}
	}
	return false
}

// MandatoryDenyPaths walks root and returns the paths that must be write-denied
// inside it, up to depth levels below root (depth 0 = root itself).
//
// The walk exists because the OS sandbox needs concrete paths to mount over,
// and because a deny rule that fires only when someone names the path is not a
// deny rule. Bounded on purpose: a deep scan of a large tree at startup is a
// cost an operator would feel, so the default depth is small and configurable.
func MandatoryDenyPaths(root string, depth int) []string {
	if root == "" {
		return nil
	}
	if depth < 0 {
		depth = 0
	}
	var out []string
	root = filepath.Clean(root)

	// Git's own layout is checked explicitly at the root and at every depth
	// below, because .git/hooks is a directory inside a directory.
	rootDepth := strings.Count(root, string(filepath.Separator))

	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable directory is not our problem here: the sandbox
			// simply does not know about its contents, which the operator sees
			// as "nothing was masked there".
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		// Depth is counted in directory hops below root, so a direct child is
		// depth 0 (the TrimPrefix leaves a leading separator to drop first).
		rel := strings.TrimPrefix(strings.TrimPrefix(path, root), string(filepath.Separator))
		relDepth := strings.Count(rel, string(filepath.Separator))
		if path != root && relDepth > depth {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		_ = rootDepth

		name := d.Name()
		if d.IsDir() {
			switch {
			case name == ".git":
				// Descend: hooks and config live inside.
				return nil
			case name == ".vscode", name == ".idea":
				out = append(out, path)
				return fs.SkipDir
			}
			// .claude/commands and .claude/agents are two levels down.
			if name == ".claude" {
				for _, sub := range []string{"commands", "agents"} {
					p := filepath.Join(path, sub)
					if _, err := os.Lstat(p); err == nil {
						out = append(out, p)
					}
				}
				return fs.SkipDir
			}
			return nil
		}

		base := filepath.Base(path)
		for _, f := range dangerousFiles {
			if strings.EqualFold(base, f) {
				out = append(out, path)
				return nil
			}
		}
		// .git/hooks and .git/config are collected by gitDirsUnder below, which
		// matches the .git directory itself. An inline "contains /.git" test
		// looks equivalent and is not: it also matches .github/, which then had
		// its config shaded as if it were git's.
		return nil
	})

	// The git directory's own protected pieces, whether or not the walk above
	// reached into them.
	for _, git := range gitDirsUnder(root, depth) {
		hooks := filepath.Join(git, "hooks")
		if _, err := os.Lstat(hooks); err == nil {
			out = append(out, hooks)
		}
		config := filepath.Join(git, "config")
		if _, err := os.Lstat(config); err == nil {
			out = append(out, config)
		}
	}
	return dedupe(out)
}

// gitDirsUnder finds .git directories (and .git files, for worktrees) up to
// depth levels below root.
func gitDirsUnder(root string, depth int) []string {
	var out []string
	root = filepath.Clean(root)
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(path, root), string(filepath.Separator))
		if path != root && strings.Count(rel, string(filepath.Separator)) > depth {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() && d.Name() == ".git" {
			out = append(out, path)
			return fs.SkipDir
		}
		return nil
	})
	return out
}

func dedupe(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	out := paths[:0]
	for _, p := range paths {
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}
