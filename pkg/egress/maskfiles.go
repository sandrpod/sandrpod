// Copyright 2026 SandrPod Contributors
// Masked files: a credentials file the sandbox can read but not learn from.
//
// The file is rewritten on the host with every masked value replaced by its
// sentinel, and the copy is bound read-only over the original inside the
// sandbox. Tools that read the file keep working — the shape is unchanged —
// and the bytes they see are worthless outside this sandbox.
//
// The rewrite is per line and format-agnostic on purpose: `~/.aws/credentials`,
// `~/.netrc`, `~/.docker/config.json` and `~/.kube/config` have four different
// syntaxes, and a parser per format would be four ways to get it wrong. What
// they share is that the secret appears somewhere as a literal substring, which
// is exactly what a replacement handles.
package egress

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
)

// MaskedFile is one rewritten credentials file.
type MaskedFile struct {
	// RealPath is the file the sandbox will attempt to read.
	RealPath string
	// FakePath is the rewritten copy, bound read-only over RealPath.
	FakePath string
	// Names lists the credentials replaced in it.
	Names []string
}

// MaskFile rewrites path into dir, replacing every masked value with its
// sentinel. A file with no masked value is returned unchanged with Names empty:
// masking a file that does not contain a credential is a no-op, not an error,
// because a config list commonly spans machines.
func (m *Masker) MaskFile(path, dir string) (*MaskedFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("egress: read %s: %w", path, err)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := raw
	var names []string
	for name, sentinel := range m.byName {
		real := m.bySentinel[sentinel].Value
		if real == "" || !containsBytes(out, []byte(real)) {
			continue
		}
		out = replaceAllBytes(out, []byte(real), []byte(sentinel))
		names = append(names, name)
	}
	if len(names) == 0 {
		return &MaskedFile{RealPath: path}, nil
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("egress: mask dir: %w", err)
	}
	// Keep the base name: some tools derive the path from it, and a 1:1 mapping
	// keeps the fake directory readable in a log.
	fake := filepath.Join(dir, filepath.Base(path))
	if err := os.WriteFile(fake, out, 0o600); err != nil {
		return nil, fmt.Errorf("egress: write masked %s: %w", fake, err)
	}
	return &MaskedFile{RealPath: path, FakePath: fake, Names: names}, nil
}

func containsBytes(b, sub []byte) bool { return bytes.Contains(b, sub) }

func replaceAllBytes(b, old, new []byte) []byte { return bytes.ReplaceAll(b, old, new) }
