// Copyright 2024 SandrPod
// SandPod state type definitions

package sandpod

// State represents the lifecycle state of a SandPod sandbox.
type State string

const (
	StatePending    State = "PENDING"    // waiting to be created
	StateStarting   State = "STARTING"   // starting up
	StateRunning    State = "RUNNING"    // running normally
	StateStopping   State = "STOPPING"   // in the process of stopping
	StateStopped    State = "STOPPED"    // stopped
	StateError      State = "ERROR"      // encountered an error
	StateTerminated State = "TERMINATED" // permanently terminated
)
