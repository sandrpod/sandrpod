// Copyright 2026 SandrPod
//
// Package audit records every permission decision (path Check, exec scan,
// PTY open) as a structured event, persists them to a local NDJSON file,
// and ships batches to a configurable HTTP endpoint.
//
// Why local-first?
//
//   The agent runs on an employee laptop that may be offline (WFH, plane,
//   poor wifi). Decisions still happen and STILL need to be auditable; we
//   can't fail-close on "audit upload failed" because that would shut the
//   AI down whenever the network blinks. So we always write locally first,
//   then upload asynchronously. The local file is the source of truth; the
//   upload is opportunistic best-effort.
//
// What is NOT in the audit pipeline?
//
//   - The contents of files read or written. Auditing the *fact* of access
//     is enough for compliance; auditing the *content* would itself become
//     a privacy nightmare and would inflate storage 1000x. If a customer
//     wants content-level audit, they can layer that on at the
//     application level (their LLM proxy, their MDM, etc.).
//
//   - The full prompt the LLM was working from. We capture the request's
//     "Reason" string when the executor sets one, but we do not pull
//     conversational context. That's the orchestrator's job, and it
//     already lives in Acme's session log.

package audit

import "time"

// Source identifies which kind of permission check produced the event.
// String values are stable on the wire — never rename without a migration.
type Source string

const (
	SourcePathCheck Source = "path" // permission.Manager.Check
	SourceExec      Source = "exec" // permission.Manager.CheckExec
	SourcePTY       Source = "pty"  // permission.Manager.CheckPTY
)

// Event is the wire-format record. We keep field names short (under 16 chars
// where possible) because audit volumes can reach millions per day on a
// large fleet — the byte savings add up.
type Event struct {
	// EventID is a client-side UUID so the server can dedupe replays
	// (which happen when a batch upload partially succeeded then the
	// agent restarted before recording the cursor advance).
	EventID string `json:"event_id"`

	// OccurredAt is when the decision was made on the agent's clock.
	// Server records a separate ReceivedAt to bound clock skew impact.
	OccurredAt time.Time `json:"occurred_at"`

	// Source — which check fired.
	Source Source `json:"source"`

	// Decision — "allow" | "deny" | "warn" (warn is only valid for Source=exec).
	Decision string `json:"decision"`

	// Path is filled for SourcePathCheck and SourcePTY (the latter as
	// "PTY:<sandbox-name>"). Empty for SourceExec.
	Path string `json:"path,omitempty"`

	// Mode is "r" / "w" / "rw" / "x". Filled for path & PTY events; empty for exec.
	Mode string `json:"mode,omitempty"`

	// Caller is the free-text label the executor passed (e.g. "files.read").
	Caller string `json:"caller,omitempty"`

	// SessionID is the sandbox session id when known.
	SessionID string `json:"session_id,omitempty"`

	// Reason — for deny events, the human-readable explanation.
	// For warn events, the matched command name. For allow, usually empty.
	Reason string `json:"reason,omitempty"`

	// MatchedCommand is filled for SourceExec when a deny or warn hit
	// a specific entry in the command policy.
	MatchedCommand string `json:"matched_command,omitempty"`

	// SandboxName is the local sandbox identifier the agent advertised
	// at registration. Useful for filtering on the server when one user
	// runs multiple sandboxes.
	SandboxName string `json:"sandbox_name,omitempty"`

	// AgentVersion is the sandrpod-agent build that produced this record.
	// Helps the server attribute behavior changes to upgrades.
	AgentVersion string `json:"agent_version,omitempty"`

	// HostOS / HostArch let server-side dashboards group by platform
	// without an extra round-trip to a sandbox-metadata table.
	HostOS   string `json:"host_os,omitempty"`
	HostArch string `json:"host_arch,omitempty"`
}

// Batch is the wire payload posted to the upload endpoint. We wrap a
// version field so the server can reject incompatible agents; bumping it
// is rare but the cost of NOT having it is a flag day if the schema
// changes.
type Batch struct {
	Version int     `json:"version"`
	Events  []Event `json:"events"`
}

// CurrentBatchVersion is the wire format we currently emit.
const CurrentBatchVersion = 1
