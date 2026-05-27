package mcpbridge

import "context"

// Decision is the outcome of a permission check.
type Decision string

const (
	DecisionAllow Decision = "allow"
	DecisionDeny  Decision = "deny"
)

// PermissionEvent describes a sandboxed action the bridge wants to take.
type PermissionEvent struct {
	// Source is one of: mcp.install, mcp.spawn, mcp.call, mcp.restart.
	Source string
	// Server is the mcp.json key (e.g. "github").
	Server string
	// Tool is the un-prefixed tool name for Source=mcp.call; empty otherwise.
	Tool string
	// Command + Args identify the subprocess for spawn / install.
	Command string
	Args    []string
	// EnvKeys lists the names (not values) of env vars that will be set.
	EnvKeys []string
}

// PermissionGate decides whether an event is allowed. Implementations should
// be fail-closed under error (returning Deny on uncertainty).
type PermissionGate interface {
	Check(ctx context.Context, evt PermissionEvent) (Decision, error)
}

// allowAllGate is the no-op default used when the host did not inject a gate.
type allowAllGate struct{}

func (allowAllGate) Check(context.Context, PermissionEvent) (Decision, error) {
	return DecisionAllow, nil
}

// AuditEvent records a single bridge action for downstream NDJSON / uploader.
type AuditEvent struct {
	Source         string
	Decision       Decision
	Server         string
	Tool           string
	ArgsSummary    string
	ResultStatus   string
	DurationMs     int64
	Reason         string
	SessionID      string
	Caller         string
	Extras         map[string]string
}

// AuditSink receives audit events. Implementations must be non-blocking
// (buffer + drop on overflow) to keep request latency predictable.
type AuditSink interface {
	Record(evt AuditEvent)
}

type nopAuditSink struct{}

func (nopAuditSink) Record(AuditEvent) {}
