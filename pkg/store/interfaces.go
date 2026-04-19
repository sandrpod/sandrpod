// Package store provides in-memory and persistent implementations of the
// repository interfaces defined in pkg/sandpod.
//
// Type aliases are exported so callers may use either sandpod.XxxRepository
// or store.XxxRepository interchangeably.
package store

import "github.com/sandrpod/sandrpod/pkg/sandpod"

// Type aliases — canonical definitions live in pkg/sandpod/repo.go.
type (
	SandboxRepository = sandpod.SandboxRepository
	PoderRepository   = sandpod.PoderRepository
	JobRepository     = sandpod.JobRepository
	Stores            = sandpod.Stores
)
