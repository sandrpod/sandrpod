// Copyright 2026 SandrPod
// Test helper for file open — separated so the production package has zero
// extra imports.

package audit

import "os"

func openFile(path string) (*os.File, error) {
	return os.Open(path)
}
